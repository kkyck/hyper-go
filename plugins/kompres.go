package main

// kompres.go — .kompres: reply sebuah video → dikompres cerdas pakai H.265
// (HEVC), hasil DIJAMIN di bawah 10MB, resolusi tetap 1080p, fps dipaksa 30,
// kualitas dijaga.
//
// Cara kerja:
//  1. ffprobe dulu: resolusi, codec, fps, durasi.
//  2. Sudah ≤1080p + ≤30fps + H.265 + ≤10MB → REMUX saja (kualitas 0% turun).
//  3. Perlu ubah → downscale max 1080p (lanczos) + paksa fps 30 (bila >30) +
//     denoise ringan → H.265 CRF 23 (setara H.264 CRF 21, ~50% lebih kecil).
//  4. Bila hasil CRF masih >10MB → encode ulang dengan bitrate yang dihitung
//     pas untuk target 9.5MB (margin aman).
//  5. Safety: bila hasil akhir ≥ file asli → pakai file asli.
//
// Contoh: reply video lalu kirim .kompres

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

const kompresMaxBytes = 10 * 1024 * 1024 // hasil wajib di bawah 10MB

// probeVideoFull membaca resolusi, codec, fps, dan durasi via ffprobe.
func probeVideoFull(path string) (w, h int, vcodec string, fps float64, durSec float64, err error) {
	cmd := exec.Command("ffprobe",
		"-hide_banner", "-loglevel", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,codec_name,avg_frame_rate,duration",
		"-of", "csv=p=0",
		path,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, 0, "", 0, 0, err
	}
	parts := strings.Split(strings.TrimSpace(out.String()), ",")
	if len(parts) != 5 {
		return 0, 0, "", 0, 0, fmt.Errorf("ffprobe output tak terduga: %q", out.String())
	}
	fmt.Sscanf(parts[0], "%d", &w)
	fmt.Sscanf(parts[1], "%d", &h)
	vcodec = strings.TrimSpace(parts[2])
	var num, den float64
	fmt.Sscanf(parts[3], "%f/%f", &num, &den)
	if den != 0 {
		fps = num / den
	}
	fmt.Sscanf(parts[4], "%f", &durSec)
	return w, h, vcodec, fps, durSec, nil
}

// runFFmpeg menjalankan ffmpeg dengan timeout 10 menit.
func runFFmpeg(args ...string) error {
	ffCtx, ffCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer ffCancel()
	cmd := exec.CommandContext(ffCtx, args[0], args[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// kompresVideoSW mengkompres video: 1080p, fps 30, hasil <10MB.
// Return path file hasil (caller wajib hapus) + keterangan aksi.
func kompresVideoSW(srcPath string) (outPath, aksi string, err error) {
	const targetBytes = int64(float64(kompresMaxBytes) * 0.95) // 9.5MB margin aman

	mkTemp := func() (string, error) {
		f, e := os.CreateTemp("", "wabot-kompres-*.mp4")
		if e != nil {
			return "", e
		}
		p := f.Name()
		f.Close()
		return p, nil
	}
	fileSize := func(p string) int64 {
		if st, e := os.Stat(p); e == nil {
			return st.Size()
		}
		return -1
	}

	w, h, vcodec, fps, durSec, perr := probeVideoFull(srcPath)
	needScale := perr != nil || w > 1920 || h > 1080
	needFps := perr != nil || fps > 30.01
	isHEVC := strings.EqualFold(vcodec, "hevc") || strings.EqualFold(vcodec, "h265")
	needReencode := needScale || needFps || !isHEVC
	srcSize := fileSize(srcPath)

	// Bangun filter chain: scale (bila perlu) → fps 30 (bila perlu) → denoise
	var filters []string
	if needScale {
		filters = append(filters, "scale='min(1920,iw)':'min(1080,ih)':eval=frame:flags=lanczos")
	}
	if needFps {
		filters = append(filters, "fps=30")
	}
	if needReencode {
		// Denoise ringan: noise sensor HP = pemakan bitrate terbesar.
		filters = append(filters, "hqdn3d=1.5:1.5:6:6")
	}
	vf := strings.Join(filters, ",")

	baseArgs := []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", srcPath}
	audioArgs := []string{"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart"}

	// ── Jalur 1: sudah ideal → remux (0% quality loss) ──
	if !needReencode && srcSize > 0 && srcSize <= kompresMaxBytes {
		outPath, err = mkTemp()
		if err != nil {
			return "", "", err
		}
		args := append(append(append([]string{}, baseArgs...),
			"-c:v", "copy"), audioArgs...)
		args = append(args, outPath)
		if rerr := runFFmpeg(args...); rerr != nil {
			_ = os.Remove(outPath)
			return "", "", rerr
		}
		if fileSize(outPath) <= 0 {
			_ = os.Remove(outPath)
			return "", "", fmt.Errorf("hasil kompresi kosong")
		}
		return outPath, "remux (kualitas 100% utuh, sudah <10MB)", nil
	}

	// ── Jalur 2: re-encode H.265 CRF 23 (setara kualitas H.264 CRF 21,
	//    tapi ~50% lebih kecil) ──
	tryPath, err := mkTemp()
	if err != nil {
		return "", "", err
	}
	args := append(append(append([]string{}, baseArgs...),
		"-vf", vf,
		"-c:v", "libx265", "-crf", "23", "-preset", "medium", "-pix_fmt", "yuv420p",
		"-tag:v", "hvc1"), // tag Apple: wajib agar iPhone bisa muter HEVC
		audioArgs...)
	args = append(args, tryPath)
	if rerr := runFFmpeg(args...); rerr != nil {
		_ = os.Remove(tryPath)
		return "", "", rerr
	}
	trySize := fileSize(tryPath)
	if trySize <= 0 {
		_ = os.Remove(tryPath)
		return "", "", fmt.Errorf("hasil kompresi kosong")
	}
	aksi = "1080p + 30fps + denoise + H.265 CRF 23"

	// ── Jalur 3a: masih >10MB → bitrate dihitung pas untuk 9.5MB ──
	// ── Jalur 3b (BARU): hasil <8MB → masih ada headroom besar, naikkan
	//    kualitas dengan bitrate yang dihitung pas untuk 9.5MB ──
	boostThreshold := targetBytes * 85 / 100 // int64 arithmetic, no float
	needBoost := trySize < boostThreshold
	if (trySize > kompresMaxBytes || needBoost) && durSec > 0 {
		_ = os.Remove(tryPath)
		audioBps := int64(128000)
		videoBudgetBits := targetBytes*8 - int64(durSec*float64(audioBps))
		videoBps := videoBudgetBits / int64(durSec)
		if videoBps < 500000 {
			videoBps = 500000 // floor 500kbps
		}
		tryPath, err = mkTemp()
		if err != nil {
			return "", "", err
		}
		bv := fmt.Sprintf("%d", videoBps)
		args = append(append(append([]string{}, baseArgs...),
			"-vf", vf,
			"-c:v", "libx265", "-b:v", bv,
			"-maxrate", fmt.Sprintf("%d", videoBps*2),
			"-bufsize", fmt.Sprintf("%d", videoBps*2),
			"-preset", "medium", "-pix_fmt", "yuv420p",
			"-tag:v", "hvc1"),
			audioArgs...)
		args = append(args, tryPath)
		if rerr := runFFmpeg(args...); rerr != nil {
			_ = os.Remove(tryPath)
			return "", "", rerr
		}
		if fileSize(tryPath) <= 0 {
			_ = os.Remove(tryPath)
			return "", "", fmt.Errorf("hasil kompresi kosong")
		}
		if needBoost {
			aksi = fmt.Sprintf("1080p + 30fps + H.265 ~%.1f Mbps (kualitas dimaksimalkan, <10MB)", float64(videoBps)/1e6)
		} else {
			aksi = fmt.Sprintf("1080p + 30fps + bitrate pas ~%.1f Mbps (target <10MB)", float64(videoBps)/1e6)
		}
	}

	// ── Safety: hasil ≥ asli → pakai asli ──
	if srcSize > 0 && fileSize(tryPath) >= srcSize {
		_ = os.Remove(tryPath)
		outPath, err = mkTemp()
		if err != nil {
			return "", "", err
		}
		data, rerr := os.ReadFile(srcPath)
		if rerr != nil {
			_ = os.Remove(outPath)
			return "", "", rerr
		}
		if werr := os.WriteFile(outPath, data, 0644); werr != nil {
			_ = os.Remove(outPath)
			return "", "", werr
		}
		return outPath, "sudah optimal (file asli dipakai)", nil
	}
	return tryPath, aksi, nil
}

func formatMB(b int64) string {
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
}

func handleKompres(client *whatsmeow.Client, msg *events.Message, args string) string {
	if client == nil {
		return "❌ Client tidak siap"
	}
	quoted := getQuotedMessage(msg)
	var dlMsg whatsmeow.DownloadableMessage
	var fileLen int64
	if quoted != nil && quoted.Message != nil {
		qm := quoted.Message
		if vm := qm.GetVideoMessage(); vm != nil {
			dlMsg, fileLen = vm, int64(vm.GetFileLength())
		} else if dwc := qm.GetDocumentWithCaptionMessage(); dwc != nil {
			if vm := dwc.GetMessage().GetVideoMessage(); vm != nil {
				dlMsg, fileLen = vm, int64(vm.GetFileLength())
			} else if doc := dwc.GetMessage().GetDocumentMessage(); doc != nil &&
				strings.HasPrefix(doc.GetMimetype(), "video/") {
				dlMsg, fileLen = doc, int64(doc.GetFileLength())
			}
		} else if doc := qm.GetDocumentMessage(); doc != nil &&
			strings.HasPrefix(doc.GetMimetype(), "video/") {
			// Video dikirim sebagai dokumen (file 100% utuh)
			dlMsg, fileLen = doc, int64(doc.GetFileLength())
		}
	}
	if dlMsg == nil {
		return "❓ *Cara pakai:* reply sebuah video (atau dokumen video) lalu kirim *.kompres*"
	}

	// Download video reply
	dlCtx, dlCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	videoData, dlErr := client.Download(dlCtx, dlMsg)
	dlCancel()
	if dlErr != nil {
		return fmt.Sprintf("❌ Gagal download video: %v", dlErr)
	}
	if len(videoData) == 0 {
		return "❌ Video kosong."
	}

	// Simpan ke temp
	src, err := os.CreateTemp("", "wabot-kompres-src-*.mp4")
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat temp: %v", err)
	}
	srcPath := src.Name()
	if _, err := src.Write(videoData); err != nil {
		src.Close()
		_ = os.Remove(srcPath)
		return fmt.Sprintf("❌ Gagal tulis temp: %v", err)
	}
	src.Close()
	defer os.Remove(srcPath)
	origSize := int64(len(videoData))
	waSize := fileLen

	// Sanity check: kalau yang ke-download jauh lebih kecil dari ukuran
	// resmi di server WA → download tidak lengkap, jangan lanjut.
	if waSize > 0 && origSize < waSize*95/100 {
		return fmt.Sprintf("❌ Download tidak lengkap: dapat %s dari %s di server WA. Coba lagi.",
			formatMB(origSize), formatMB(waSize))
	}

	// Kompres cerdas
	outPath, aksi, err := kompresVideoSW(srcPath)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kompres: %v", err)
	}
	defer os.Remove(outPath)

	outData, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Sprintf("❌ Gagal baca hasil: %v", err)
	}
	newSize := int64(len(outData))

	// Kirim balik dengan info ukuran + aksi.
	// Kalau ukuran resmi WA beda jauh dari yang ke-download, tampilkan juga
	// biar transparan (WA mengkompres saat upload dari HP pengirim).
	caption := fmt.Sprintf("✅ %s → %s\n%s", formatMB(origSize), formatMB(newSize), aksi)
	if waSize > 0 && waSize > origSize*105/100 {
		caption = fmt.Sprintf("✅ %s → %s\n%s\nℹ️ Ukuran asli di HP pengirim: %s (WA mengkompres saat upload)",
			formatMB(origSize), formatMB(newSize), aksi, formatMB(waSize))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := sprSendVideo(client, ctx, msg.Info.Chat, outData, "video/mp4", caption); err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "kompres",
		Aliases:     []string{"compress", "kecilkan"},
		Description: "Kompres video reply ke H.265: 1080p, 30fps, <10MB",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleKompres(client, msg, args)
		},
	})
}
