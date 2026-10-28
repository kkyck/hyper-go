package main

// sticker_engine.go — Port dari plugin sticker terbaru (gowa-bot)
// ════════════════════════════════════════════════════════════
//
// Konversi media → WebP via ffmpeg/img2webp/webpmux (os/exec), bukan library Go.
// Fitur:
//   - StickerSourceAnimated: deteksi apakah sumber harus jadi WebP animasi
//   - IsAnimatedWebP: cek container WebP (bukan MIME)
//   - ConvertStickerToWebP: konversi via ffmpeg (statis) / img2webp+adaptive (animasi)
//   - InjectWebPMetadata: EXIF via webpmux
//   - Conversion gate: max 1 konversi bersamaan (hemat CPU)
//   - Album → sticker pack (StickerPackMessage)

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── Konfigurasi ─────────────────────────────────────────────

var stickerConvGate = make(chan struct{}, 1)

const ffmpegThreads = "2"
const maxStickerSize = 1 << 20 // 1 MiB

const (
	stickerAnimFPS       = 15
	stickerAnimMaxSec    = 6
	stickerRenderTimeout = 45 * time.Second
	stickerProbeTimeout  = 5 * time.Second
)

// StickerEXIFPackname / StickerEXIFAuthor — metadata default
const StickerEXIFPackname = "Ruby Stickers"
const StickerEXIFAuthor = "Ruby Hoshino"
const StickerPackName = "Ruby Pack"
const StickerPackPublisher = "Ruby Hoshino"

// ─── Deteksi animasi ─────────────────────────────────────────

// StickerSourceAnimated menentukan apakah sumber harus jadi WebP animasi.
func StickerSourceAnimated(mime, source string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "image/gif") {
		return true
	}
	source = strings.ToLower(strings.TrimSpace(source))
	if q := strings.IndexAny(source, "?#"); q >= 0 {
		source = source[:q]
	}
	for _, ext := range []string{".gif", ".mp4", ".mov", ".webm", ".mkv", ".avi", ".3gp"} {
		if strings.HasSuffix(source, ext) {
			return true
		}
	}
	return false
}

// IsAnimatedWebP mengecek container WebP, bukan mempercayai MIME sumber.
func IsAnimatedWebP(data []byte) bool {
	if len(data) < 16 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false
	}
	return bytes.Contains(data, []byte("ANIM")) || bytes.Contains(data, []byte("ANMF"))
}

// ─── Konversi utama ──────────────────────────────────────────

// ConvertStickerToWebP: canvas 512x512, padding transparan;
// animasi dibatasi 15fps × 6 detik (90 frame).
func ConvertStickerToWebP(data []byte, animated bool) ([]byte, error) {
	stickerConvGate <- struct{}{}
	defer func() { <-stickerConvGate }()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg tidak ditemukan. Install dengan: apt install ffmpeg")
	}

	inName, err := stickerTemp("sticker-input-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(inName)
	if err = os.WriteFile(inName, data, 0o600); err != nil {
		return nil, err
	}

	outName, err := stickerTemp("sticker-output-*.webp")
	if err != nil {
		return nil, err
	}
	defer os.Remove(outName)

	filter := "scale=512:512:force_original_aspect_ratio=decrease,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba"

	if !animated {
		args := []string{"-hide_banner", "-loglevel", "error", "-y",
			"-threads", ffmpegThreads,
			"-i", inName, "-vf", filter,
			"-vcodec", "libwebp", "-lossless", "0", "-q:v", "85",
			"-compression_level", "4", "-pix_fmt", "bgra",
			"-loop", "0", "-an", "-vsync", "0", outName,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg sticker: %s", strings.TrimSpace(string(output)))
		}
		webp, err := os.ReadFile(outName)
		if err != nil {
			return nil, err
		}
		if len(webp) == 0 {
			return nil, fmt.Errorf("hasil WebP kosong")
		}
		return webp, nil
	}

	dur := probeVideoDuration(inName)
	if dur <= 0 || dur > stickerAnimMaxSec {
		dur = stickerAnimMaxSec
	}

	if _, img2webpErr := exec.LookPath("img2webp"); img2webpErr == nil {
		webp, err := convertAnimatedStickerViaImg2webp(inName, outName, filter, dur)
		if err == nil && len(webp) <= maxStickerSize {
			return webp, nil
		}
		if err != nil {
			fmt.Printf("[sticker] img2webp gagal: %v — fallback ke ffmpeg adaptive\n", err)
		} else {
			fmt.Printf("[sticker] img2webp hasil %.2f MB > 1MB — fallback ke ffmpeg\n", float64(len(webp))/1024/1024)
		}
	}

	webp, err := convertAnimatedStickerAdaptive(inName, outName, dur)
	if err != nil {
		return nil, fmt.Errorf("adaptive ffmpeg gagal: %w", err)
	}
	if !IsAnimatedWebP(webp) {
		return nil, fmt.Errorf("adaptive ffmpeg menghasilkan WebP tanpa frame animasi")
	}
	return webp, nil
}

// ─── EXIF metadata ───────────────────────────────────────────

// InjectWebPMetadata memakai webpmux untuk menyematkan EXIF metadata ke WebP.
func InjectWebPMetadata(webp []byte, packname, author string) ([]byte, error) {
	if packname == "" && author == "" {
		return webp, nil
	}
	if _, err := exec.LookPath("webpmux"); err != nil {
		return webp, fmt.Errorf("webpmux tidak ditemukan")
	}

	exifData := buildStickerEXIF(packname, author)

	exifFile, err := os.CreateTemp("", "sticker-exif-*")
	if err != nil {
		return webp, err
	}
	exifPath := exifFile.Name()
	defer os.Remove(exifPath)
	if _, err = exifFile.Write(exifData); err != nil {
		exifFile.Close()
		return webp, err
	}
	exifFile.Close()

	inFile, err := os.CreateTemp("", "sticker-webp-in-*")
	if err != nil {
		return webp, err
	}
	inPath := inFile.Name()
	defer os.Remove(inPath)
	if _, err = inFile.Write(webp); err != nil {
		inFile.Close()
		return webp, err
	}
	inFile.Close()

	outFile, err := os.CreateTemp("", "sticker-webp-out-*.webp")
	if err != nil {
		return webp, err
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "webpmux", "-set", "exif", exifPath, inPath, "-o", outPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return webp, fmt.Errorf("webpmux: %s", strings.TrimSpace(string(output)))
	}

	result, err := os.ReadFile(outPath)
	if err != nil {
		return webp, err
	}
	if len(result) == 0 {
		return webp, fmt.Errorf("webpmux hasil kosong")
	}
	fmt.Printf("[sticker] EXIF injected: packname=%q author=%q\n", packname, author)
	return result, nil
}

// SendStickerMediaV2 mengunggah dan mengirim sticker dengan metadata EXIF.
func SendStickerMediaV2(client *whatsmeow.Client, chat types.JID, webp []byte, animated bool, packname, author string) error {
	webp2, err := InjectWebPMetadata(webp, packname, author)
	if err != nil {
		fmt.Printf("[sticker] WARNING: gagal inject EXIF: %v\n", err)
	} else {
		webp = webp2
	}

	upload, err := client.Upload(context.Background(), webp, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("gagal upload sticker: %w", err)
	}

	stickerMsg := &waE2E.StickerMessage{
		URL:               &upload.URL,
		DirectPath:        &upload.DirectPath,
		Mimetype:          proto.String("image/webp"),
		FileSHA256:        upload.FileSHA256,
		FileEncSHA256:     upload.FileEncSHA256,
		MediaKey:          upload.MediaKey,
		FileLength:        &upload.FileLength,
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		Width:             proto.Uint32(512),
		Height:            proto.Uint32(512),
		IsAnimated:        proto.Bool(animated),
		IsAiSticker:       proto.Bool(true),
		StickerSentTS:     proto.Int64(time.Now().UnixMilli()),
	}
	_, err = client.SendMessage(context.Background(), chat, &waE2E.Message{
		StickerMessage: stickerMsg,
	})
	return err
}

// ─── Internal: work dir & temp ───────────────────────────────

func stickerWorkDir() string {
	dir := filepath.Join(".", ".tmp", "sticker")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func stickerTemp(pattern string) (string, error) {
	f, err := os.CreateTemp(stickerWorkDir(), pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// ─── Internal: konversi animasi ──────────────────────────────

func convertAnimatedStickerViaImg2webp(inName, outName, filter string, dur int) ([]byte, error) {
	frameDir, err := os.MkdirTemp(stickerWorkDir(), "sticker-frames-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(frameDir)

	ffArgs := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-threads", ffmpegThreads,
		"-t", fmt.Sprintf("%d", dur), "-i", inName,
		"-vf", "fps=" + strconv.Itoa(stickerAnimFPS) + "," + filter,
		filepath.Join(frameDir, "f_%04d.png"),
	}
	ctx1, cancel1 := context.WithTimeout(context.Background(), stickerRenderTimeout)
	defer cancel1()
	cmd := exec.CommandContext(ctx1, "ffmpeg", ffArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg extract frames: %s", strings.TrimSpace(string(output)))
	}

	frames, err := filepath.Glob(filepath.Join(frameDir, "f_*.png"))
	if err != nil || len(frames) == 0 {
		return nil, fmt.Errorf("tidak ada frame PNG yang dihasilkan")
	}

	img2wArgs := append([]string{"-loop", "0", "-d", "66", "-q", "80"}, frames...)
	img2wArgs = append(img2wArgs, "-o", outName)

	ctx2, cancel2 := context.WithTimeout(context.Background(), stickerRenderTimeout)
	defer cancel2()
	cmd2 := exec.CommandContext(ctx2, "img2webp", img2wArgs...)
	if output, err := cmd2.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("img2webp: %s", strings.TrimSpace(string(output)))
	}

	webp, err := os.ReadFile(outName)
	if err != nil {
		return nil, err
	}
	if len(webp) == 0 {
		return nil, fmt.Errorf("hasil WebP kosong")
	}
	if !IsAnimatedWebP(webp) {
		return nil, fmt.Errorf("img2webp menghasilkan WebP tanpa frame animasi")
	}
	return webp, nil
}

func convertAnimatedStickerAdaptive(inName, outName string, dur int) ([]byte, error) {
	data, sizeMB, err := renderAnimatedWebP(inName, outName, dur, 512, stickerAnimFPS, 30)
	if err != nil {
		return nil, err
	}
	if len(data) <= maxStickerSize {
		fmt.Printf("[STICKER] OK (fast-path): %.2f MB dur=%ds\n", sizeMB, dur)
		return data, nil
	}

	res, fps, q := 512, stickerAnimFPS, 30
	for {
		switch {
		case res > 256:
			res -= 64
		case fps > 10:
			fps -= 3
		case q > 8:
			q -= 6
		case dur > 2:
			dur--
		default:
			return nil, fmt.Errorf("tidak bisa compress ke <1MB (size: %.2f MB)", sizeMB)
		}
		data, sizeMB, err = renderAnimatedWebP(inName, outName, dur, res, fps, q)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxStickerSize {
			fmt.Printf("[STICKER] OK (adaptive): %.2f MB dur=%ds res=%d fps=%d q=%d\n", sizeMB, dur, res, fps, q)
			return data, nil
		}
	}
}

func probeVideoDuration(path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), stickerProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-show_entries", "format=duration",
		"-of", "csv=p=0", path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
		return int(math.Ceil(v))
	}
	return 0
}

func renderAnimatedWebP(inName, outName string, dur, res, fps, q int) ([]byte, float64, error) {
	vf := fmt.Sprintf(
		"fps=%d,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=0x00000000,format=rgba",
		fps, res, res, res, res,
	)
	ctx, cancel := context.WithTimeout(context.Background(), stickerRenderTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-threads", ffmpegThreads,
		"-i", inName,
		"-t", fmt.Sprintf("%d", dur),
		"-vf", vf,
		"-vcodec", "libwebp",
		"-lossless", "0",
		"-compression_level", "4",
		"-q:v", fmt.Sprintf("%d", q),
		"-pix_fmt", "bgra",
		"-loop", "0",
		"-an",
		"-vsync", "0",
		outName,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, 0, fmt.Errorf("ffmpeg animated webp: %s", strings.TrimSpace(string(output)))
	}
	data, err := os.ReadFile(outName)
	if err != nil {
		return nil, 0, err
	}
	sizeMB := float64(len(data)) / 1024 / 1024
	fmt.Printf("[STICKER] dur=%ds res=%d fps=%d q=%d → %.2f MB\n", dur, res, fps, q, sizeMB)
	return data, sizeMB, nil
}

// ─── Internal: EXIF & pack ───────────────────────────────────

func buildStickerEXIF(packname, author string) []byte {
	jsonStr := fmt.Sprintf(`{"sticker-pack-name":"%s","sticker-pack-publisher":"%s"}`, packname, author)
	jsonBytes := []byte(jsonStr)

	var buf bytes.Buffer
	buf.Write([]byte{0x49, 0x49, 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00})
	buf.Write([]byte{0x01, 0x00})
	buf.Write([]byte{0x41, 0x57, 0x07, 0x00})
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00})
	buf.Write([]byte{0x16, 0x00, 0x00, 0x00})
	buf.Write(jsonBytes)

	result := buf.Bytes()
	result[14] = byte(len(jsonBytes))
	result[15] = byte(len(jsonBytes) >> 8)
	result[16] = byte(len(jsonBytes) >> 16)
	result[17] = byte(len(jsonBytes) >> 24)
	return result
}

type stickerZipEntry struct {
	Name string
	Data []byte
}

func sanitizePackName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ' ', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if out == "" {
		out = "unnamed"
	}
	return out
}

func buildStoreZip(entries []stickerZipEntry) []byte {
	putU16 := func(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
	putU32 := func(b []byte, v uint32) {
		b[0] = byte(v)
		b[1] = byte(v >> 8)
		b[2] = byte(v >> 16)
		b[3] = byte(v >> 24)
	}

	var locals []byte
	var centrals []byte
	var offset uint32

	for _, e := range entries {
		nl := uint16(len(e.Name))
		sz := uint32(len(e.Data))
		crc := crc32.ChecksumIEEE(e.Data)

		lh := make([]byte, 30+int(nl))
		putU32(lh[0:], 0x04034b50)
		putU16(lh[4:], 20)
		putU32(lh[14:], crc)
		putU32(lh[18:], sz)
		putU32(lh[22:], sz)
		putU16(lh[26:], nl)
		copy(lh[30:], e.Name)

		locals = append(locals, lh...)
		locals = append(locals, e.Data...)

		cd := make([]byte, 46+int(nl))
		putU32(cd[0:], 0x02014b50)
		putU16(cd[4:], 20)
		putU16(cd[6:], 20)
		putU32(cd[16:], crc)
		putU32(cd[20:], sz)
		putU32(cd[24:], sz)
		putU16(cd[28:], nl)
		putU32(cd[42:], offset)
		copy(cd[46:], e.Name)
		centrals = append(centrals, cd...)

		offset += 30 + uint32(nl) + sz
	}

	eocd := make([]byte, 22)
	putU32(eocd[0:], 0x06054b50)
	putU16(eocd[8:], uint16(len(entries)))
	putU16(eocd[10:], uint16(len(entries)))
	putU32(eocd[12:], uint32(len(centrals)))
	putU32(eocd[16:], offset)

	return append(append(locals, centrals...), eocd...)
}

// ─── Album → sticker pack ────────────────────────────────────

// MediaMsgEntry — satu pesan media kandidat album
type MediaMsgEntry struct {
	Msg       *waE2E.Message
	MsgID     string
	Sender    types.JID
	Timestamp time.Time
}

// handleStickerAlbum: konversi album menjadi sticker pack.
// albumMsgs: daftar pesan media (image/video).
func handleStickerAlbum(client *whatsmeow.Client, msg *events.Message, albumMsgs []MediaMsgEntry, packName string) string {
	packname := sanitizePackName(packName)
	if packname == "" || packname == "unnamed" {
		packname = StickerPackName
	}

	type decodedSticker struct {
		webp     []byte
		animated bool
	}
	stickers := make([]decodedSticker, len(albumMsgs))
	var errCount atomic.Int32

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, am := range albumMsgs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, am MediaMsgEntry) {
			defer wg.Done()
			defer func() { <-sem }()

			media := am.Msg
		unwrapLoop:
			for j := 0; j < 5; j++ {
				switch {
				case media.GetEphemeralMessage().GetMessage() != nil:
					media = media.GetEphemeralMessage().GetMessage()
				case media.GetViewOnceMessage().GetMessage() != nil:
					media = media.GetViewOnceMessage().GetMessage()
				case media.GetViewOnceMessageV2().GetMessage() != nil:
					media = media.GetViewOnceMessageV2().GetMessage()
				default:
					break unwrapLoop
				}
			}

			var data []byte
			var animated bool
			var dlErr error
			switch {
			case media.GetImageMessage() != nil:
				data, dlErr = client.Download(context.Background(), media.GetImageMessage())
				animated = false
			case media.GetVideoMessage() != nil:
				data, dlErr = client.Download(context.Background(), media.GetVideoMessage())
				animated = true
			default:
				errCount.Add(1)
				return
			}
			if dlErr != nil {
				errCount.Add(1)
				return
			}

			webp, convErr := ConvertStickerToWebP(data, animated)
			if convErr != nil {
				errCount.Add(1)
				return
			}
			webp, _ = InjectWebPMetadata(webp, StickerEXIFPackname, StickerEXIFAuthor)
			stickers[i] = decodedSticker{webp: webp, animated: animated}
		}(i, am)
	}
	wg.Wait()

	var valid []decodedSticker
	for _, sd := range stickers {
		if sd.webp != nil {
			valid = append(valid, sd)
		}
	}
	errors := int(errCount.Load())

	if len(valid) == 0 {
		return fmt.Sprintf("❌ Gagal proses album: %d media error", errors)
	}

	var zipEntries []stickerZipEntry
	zipNames := make([]string, len(valid))
	for i, sd := range valid {
		sum := sha256.Sum256(sd.webp)
		rawB64 := base64.StdEncoding.EncodeToString(sum[:])
		fileName := fmt.Sprintf("%02d_%s.webp", i,
			strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(rawB64))
		zipNames[i] = fileName
		zipEntries = append(zipEntries, stickerZipEntry{Name: fileName, Data: sd.webp})
	}

	trayWebP, thumbJpeg, err := makeTrayAndThumbnail(valid[0].webp)
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat tray/thumbnail: %s", err.Error())
	}
	trayHashRaw := sha256.Sum256(trayWebP)
	zipEntries = append(zipEntries, stickerZipEntry{Name: "tray_icon.webp", Data: trayWebP})

	zipData := buildStoreZip(zipEntries)
	fmt.Printf("[stickerpack-album] zip=%d bytes stickers=%d\n", len(zipData), len(valid))

	packUpload, upErr := client.Upload(context.Background(), zipData, whatsmeow.MediaStickerPack)
	if upErr != nil {
		return fmt.Sprintf("❌ Gagal upload pack: %s", upErr.Error())
	}
	thumbUpload, upErr := client.Upload(context.Background(), thumbJpeg, whatsmeow.MediaImage)
	if upErr != nil {
		return fmt.Sprintf("❌ Gagal upload thumbnail: %s", upErr.Error())
	}

	packRand := make([]byte, 4)
	rand.Read(packRand)
	packID := "Pack_" + hex.EncodeToString(packRand)
	now := time.Now().Unix()
	origin := waE2E.StickerPackMessage_USER_CREATED
	publisherVal := StickerPackPublisher
	packNameVal := packname
	packFL := packUpload.FileLength

	protoStickers := make([]*waE2E.StickerPackMessage_Sticker, len(valid))
	for i, sd := range valid {
		protoStickers[i] = &waE2E.StickerPackMessage_Sticker{
			FileName:           proto.String(zipNames[i]),
			Mimetype:           proto.String("image/webp"),
			IsAnimated:         proto.Bool(sd.animated),
			Emojis:             []string{""},
			AccessibilityLabel: proto.String(""),
		}
	}

	msgSecret := make([]byte, 32)
	rand.Read(msgSecret)

	outMsg := &waE2E.Message{
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: msgSecret},
		StickerPackMessage: &waE2E.StickerPackMessage{
			StickerPackID:       &packID,
			Name:                &packNameVal,
			Publisher:           &publisherVal,
			Stickers:            protoStickers,
			FileLength:          &packFL,
			FileSHA256:          packUpload.FileSHA256,
			FileEncSHA256:       packUpload.FileEncSHA256,
			MediaKey:            packUpload.MediaKey,
			DirectPath:          &packUpload.DirectPath,
			MediaKeyTimestamp:   &now,
			TrayIconFileName:    proto.String("tray_icon.webp"),
			ThumbnailHeight:     proto.Uint32(252),
			ThumbnailWidth:      proto.Uint32(252),
			ImageDataHash:       proto.String(base64.StdEncoding.EncodeToString(trayHashRaw[:])),
			ThumbnailDirectPath: &thumbUpload.DirectPath,
			ThumbnailSHA256:     thumbUpload.FileSHA256,
			ThumbnailEncSHA256:  thumbUpload.FileEncSHA256,
			StickerPackOrigin:   &origin,
			StickerPackSize:     proto.Uint64(packFL),
		},
	}

	_, sendErr := client.SendMessage(context.Background(), msg.Info.Chat, outMsg)
	if sendErr != nil {
		return fmt.Sprintf("❌ Gagal kirim stickerpack: %s", sendErr.Error())
	}
	return fmt.Sprintf("✅ Sticker pack \"%s\" terkirim (%d sticker)", packname, len(valid))
}

func makeTrayAndThumbnail(stickerWebp []byte) (trayWebP, thumbJPEG []byte, err error) {
	input := stickerWebp
	if extracted, xerr := extractFirstFrameWebP(stickerWebp); xerr != nil {
		fmt.Printf("[sticker] WARNING: extract first frame: %v\n", xerr)
	} else if extracted != nil {
		input = extracted
	}

	inPath, terr := stickerTemp("tray_in-*.webp")
	if terr != nil {
		return nil, nil, fmt.Errorf("gagal buat temp: %w", terr)
	}
	defer os.Remove(inPath)
	if werr := os.WriteFile(inPath, input, 0o600); werr != nil {
		return nil, nil, werr
	}

	tmpWebp := inPath + "_tray.webp"
	defer os.Remove(tmpWebp)
	ctx1, cancel1 := context.WithTimeout(context.Background(), stickerRenderTimeout)
	defer cancel1()
	cmd := exec.CommandContext(ctx1, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-threads", ffmpegThreads, "-i", inPath, "-vframes", "1",
		"-vf", "scale=252:252:force_original_aspect_ratio=decrease,pad=252:252:(ow-iw)/2:(oh-ih)/2:color=0x00000000",
		"-vcodec", "libwebp", "-lossless", "0", "-q:v", "80", tmpWebp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("ffmpeg tray: %s", stderr.String())
	}
	trayWebP, err = os.ReadFile(tmpWebp)
	if err != nil {
		return nil, nil, err
	}

	tmpJpg := inPath + "_thumb.jpg"
	defer os.Remove(tmpJpg)
	ctx2, cancel2 := context.WithTimeout(context.Background(), stickerRenderTimeout)
	defer cancel2()
	cmd = exec.CommandContext(ctx2, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-threads", ffmpegThreads, "-i", tmpWebp, "-vframes", "1",
		"-vf", "scale=252:252", "-q:v", "5", tmpJpg)
	cmd.Stderr = &stderr
	stderr.Reset()
	if err = cmd.Run(); err != nil {
		return nil, nil, fmt.Errorf("ffmpeg thumb: %s", stderr.String())
	}
	thumbJPEG, err = os.ReadFile(tmpJpg)
	if err != nil {
		return nil, nil, err
	}
	return trayWebP, thumbJPEG, nil
}

func extractFirstFrameWebP(data []byte) ([]byte, error) {
	if !IsAnimatedWebP(data) {
		return nil, nil
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, fmt.Errorf("bukan WebP valid")
	}
	pos := 12
	var canvasW, canvasH uint32
	for pos+8 <= len(data) {
		chunkID := string(data[pos : pos+4])
		chunkSize := uint32(data[pos+4]) | uint32(data[pos+5])<<8 | uint32(data[pos+6])<<16 | uint32(data[pos+7])<<24
		chunkStart := pos + 8
		if chunkStart+int(chunkSize) > len(data) {
			break
		}
		if chunkID == "VP8X" && chunkSize >= 10 {
			canvasW = uint32(data[chunkStart+4]) | uint32(data[chunkStart+5])<<8 | uint32(data[chunkStart+6])<<16
			canvasW++
			canvasH = uint32(data[chunkStart+7]) | uint32(data[chunkStart+8])<<8 | uint32(data[chunkStart+9])<<16
			canvasH++
		}
		if chunkID == "ANMF" && chunkSize >= 16 {
			anmfData := data[chunkStart+16 : chunkStart+int(chunkSize)]
			return buildStaticWebP(anmfData, canvasW, canvasH)
		}
		aligned := int(chunkSize)
		if aligned%2 != 0 {
			aligned++
		}
		pos = chunkStart + aligned
	}
	return nil, fmt.Errorf("ANMF frame tidak ditemukan")
}

func buildStaticWebP(frameData []byte, w, h uint32) ([]byte, error) {
	if len(frameData) < 4 {
		return nil, fmt.Errorf("frame data terlalu kecil")
	}
	id := string(frameData[:4])
	if id == "VP8L" || id == "VP8 " {
		var buf bytes.Buffer
		buf.WriteString("RIFF")
		size := uint32(4 + len(frameData))
		buf.Write([]byte{byte(size), byte(size >> 8), byte(size >> 16), byte(size >> 24)})
		buf.WriteString("WEBP")
		buf.Write(frameData)
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("frame type tidak dikenal: %s", id)
}
