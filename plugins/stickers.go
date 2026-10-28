package main

/**
 * 🖼️ stickers.go — Sticker maker (image → WEBP, text → WEBP)
 * ════════════════════════════════════════════════════════════
 * Features (prefix .):
 *   .s / .stiker <teks>    → Text → SVG → WEBP sticker
 *   .s / .stiker            → Reply/kirim gambar → sticker WEBP
 *   .anticolong / .setattr  → Reply sticker WEBP → inject EXIF anticolong
 *   .cwm [teks]             → Reply sticker → custom watermark + anticolong
 *   .cwm -n [teks]          → Custom watermark TANPA anticolong
 *   .tspk                   → Kirim sticker pack (1 pack per user)
 *   .addtspk                → Tambah sticker ke pack (reply sticker)
 *   .deltspk                → Hapus sticker dari pack (reply sticker)
 *   .stc save <id>          → Simpan sticker ke pack <id>
 *   .stc send pack <id>     → Kirim pack <id> sebagai sticker pack
 *   .stc anticolong on|off  → Toggle anticolong payload untuk .stc send
 *
 * Library mapping (Baileys → Go):
 *   sharp            → github.com/disintegration/imaging (resize) + image/png
 *   wa-sticker-formatter → manual: resize → RGBA → chai2010/webp Encode
 *   JSZip            → archive/zip (stdlib) for sticker pack
 *   crypto.hkdfSync  → golang.org/x/crypto/hkdf (stdlib)
 *
 * Storage: ./data/sticker-store.json (per-user legacy pack + named packs)
 */

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"golang.org/x/crypto/hkdf"
	"google.golang.org/protobuf/proto"
)

const stickerStoreFile = dataDir + "/sticker-store.json"

const (
	maxStickerDownloadBytes = 50 * 1024 * 1024 // 50MB
	maxLegacyPackSize       = 100
	maxStickersPerPack      = 100
	maxStickerUsers         = 500
)

type StickerItem struct {
	SHA256     string `json:"sha256"`
	Buffer     string `json:"buffer"` // base64
	Ext        string `json:"ext"`
	Mimetype   string `json:"mimetype"`
	IsAnimated bool   `json:"is_animated"`
	IsLottie   bool   `json:"is_lottie"`
}

type StickerUserStore struct {
	LegacyPack []StickerItem            `json:"legacy_pack"`
	Anticolong bool                     `json:"anticolong"`
	Packs      map[string][]StickerItem `json:"packs"`
}

var (
	stickerStore     = map[string]*StickerUserStore{}
	stickerStoreMu   sync.RWMutex
	stickerStoreOnce sync.Once
)

func loadStickerStore() {
	stickerStoreOnce.Do(func() {
		stickerStoreMu.Lock()
		defer stickerStoreMu.Unlock()
		_ = loadJSON(stickerStoreFile, &stickerStore)
		if stickerStore == nil {
			stickerStore = map[string]*StickerUserStore{}
		}
	})
}

func saveStickerStore() error {
	stickerStoreMu.RLock()
	defer stickerStoreMu.RUnlock()
	return saveJSON(stickerStoreFile, stickerStore)
}

func getStickerUserStore(sender string) *StickerUserStore {
	stickerStoreMu.Lock()
	defer stickerStoreMu.Unlock()
	s, ok := stickerStore[sender]
	if !ok {
		// enforce user limit
		if len(stickerStore) >= maxStickerUsers {
			// evict oldest (map iteration is random but acceptable)
			for k := range stickerStore {
				delete(stickerStore, k)
				break
			}
		}
		s = &StickerUserStore{
			LegacyPack: []StickerItem{},
			Packs:      map[string][]StickerItem{},
		}
		stickerStore[sender] = s
	}
	if s.LegacyPack == nil {
		s.LegacyPack = []StickerItem{}
	}
	if s.Packs == nil {
		s.Packs = map[string][]StickerItem{}
	}
	return s
}

// ─── Helpers: WEBP validity, chunk manipulation ──────────────────

func isRiffWebP(buf []byte) bool {
	return len(buf) >= 12 &&
		string(buf[0:4]) == "RIFF" &&
		string(buf[8:12]) == "WEBP"
}

// buildStickerExif builds the EXIF payload containing JSON metadata for WhatsApp sticker.
func buildStickerExif(metadata map[string]any) []byte {
	jsonBytes, _ := json.Marshal(metadata)
	// EXIF header (little-endian TIFF + AW tag)
	header := []byte{
		0x49, 0x49, 0x2a, 0x00, // TIFF magic (LE)
		0x08, 0x00, 0x00, 0x00, // IFD0 offset = 8
		0x01, 0x00, // num entries = 1
		0x41, 0x57, // tag 0x5741 ("AW") — WhatsApp sticker EXIF
		0x07, 0x00, // type = UNDEFINED (7)
	}
	out := make([]byte, 0, len(header)+4+4+len(jsonBytes))
	out = append(out, header...)
	// 4 bytes: count of components (will be json length)
	countBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(countBuf, uint32(len(jsonBytes)))
	out = append(out, countBuf...)
	// 4 bytes: value offset (placeholder; for small payloads the value is inline)
	out = append(out, 0x16, 0x00, 0x00, 0x00) // 22 — start offset of value
	out = append(out, jsonBytes...)
	return out
}

// makeStickerChunk builds a single RIFF chunk.
func makeStickerChunk(chunkType string, data []byte) []byte {
	t := []byte(chunkType)
	if len(t) != 4 {
		// pad with spaces
		pad := make([]byte, 4)
		copy(pad, t)
		t = pad
	}
	sizeBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(sizeBuf, uint32(len(data)))
	chunk := append([]byte{}, t...)
	chunk = append(chunk, sizeBuf...)
	chunk = append(chunk, data...)
	if len(data)%2 == 1 {
		chunk = append(chunk, 0)
	}
	return chunk
}

// setWebpExif injects/replaces the EXIF chunk in a WEBP buffer.
func setWebpExif(webpBuf []byte, metadata map[string]any) ([]byte, error) {
	if !isRiffWebP(webpBuf) {
		return nil, fmt.Errorf("file bukan WEBP valid")
	}

	var chunks [][]byte
	offset := 12
	for offset+8 <= len(webpBuf) {
		chunkType := string(webpBuf[offset : offset+4])
		size := binary.LittleEndian.Uint32(webpBuf[offset+4 : offset+8])
		chunkStart := offset
		chunkEnd := offset + 8 + int(size) + int(size%2)
		if chunkEnd > len(webpBuf) {
			break
		}
		if chunkType != "EXIF" {
			chunks = append(chunks, webpBuf[chunkStart:chunkEnd])
		}
		offset = chunkEnd
	}

	exifPayload := buildStickerExif(metadata)
	exifChunk := makeStickerChunk("EXIF", exifPayload)

	var body []byte
	for _, c := range chunks {
		body = append(body, c...)
	}
	body = append(body, exifChunk...)

	header := make([]byte, 12)
	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(body)+4))
	copy(header[8:12], []byte("WEBP"))

	return append(header, body...), nil
}

// applyAnticolongExifToSticker injects the anticolong EXIF metadata.
func applyAnticolongExifToSticker(buf []byte) []byte {
	if !isRiffWebP(buf) {
		return buf
	}
	packID := uuidHex()
	metadata := map[string]any{
		"sticker-pack-id":            packID,
		"sticker-pack-name":          "Hiura Mihate",
		"sticker-pack-publisher":     "Hiura Bot",
		"accessibility-text":         "ANTICOLONG",
		"android-app-store-link":     "https://whatsapp.com",
		"ios-app-store-link":         "https://whatsapp.com/ios",
		"emojis":                     []string{"🦸", "😴", "😌"},
		"is-from-sticker-maker":      0,
		"is-avatar-sticker":          1,
		"avatar-sticker-template-id": "whatsapp",
		"is-ai-sticker":              1,
		"is-avatar-country-sticker":  1,
		"is-avatar-instant-sticker":  1,
		"sticker-maker-source-type":  4,
		"is-avatar-social-sticker":   1,
		"avatar-sticker-style":       "whatsapp",
		"avatar-sticker-revision-id": "2026",
		"is-from-user-created-pack":  1,
		"origin-pack-id":             "whatsapp",
		"is-text-sticker":            1,
	}
	out, err := setWebpExif(buf, metadata)
	if err != nil {
		return buf
	}
	return out
}

// uuidHex returns a 16-byte random hex string (no dashes for compactness).
func uuidHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: waktu nano + counter agar tetap unik bila CSPRNG gagal.
		fmt.Printf("[STICKER] WARNING: crypto/rand gagal (%v), pakai fallback UUID\n", err)
		nano := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(nano >> (uint(i%8) * 8))
		}
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 32)
	for i, v := range b {
		out[i*2] = hexd[v>>4]
		out[i*2+1] = hexd[v&0x0f]
	}
	return string(out)
}

// sha256Bytes returns SHA-256 of buf.
func sha256Bytes(buf []byte) []byte {
	h := sha256.New()
	h.Write(buf)
	return h.Sum(nil)
}

// toB64URL encodes bytes to a base64url string (no padding).
func toB64URL(buf []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	for i := 0; i < len(buf); i += 3 {
		b1 := buf[i]
		var b2, b3 byte
		valid := 1
		if i+1 < len(buf) {
			b2 = buf[i+1]
			valid = 2
		}
		if i+2 < len(buf) {
			b3 = buf[i+2]
			valid = 3
		}
		out = append(out,
			alphabet[b1>>2],
			alphabet[((b1&0x03)<<4)|(b2>>4)],
		)
		if valid >= 2 {
			out = append(out, alphabet[((b2&0x0f)<<2)|(b3>>6)])
		} else {
			break
		}
		if valid == 3 {
			out = append(out, alphabet[b3&0x3f])
		}
	}
	return string(out)
}

// isAnimatedWebP checks if a WEBP buffer has VP8X/ANIM/ANMF chunks indicating animation.
func isAnimatedWebP(buf []byte) bool {
	if !isRiffWebP(buf) {
		return false
	}
	offset := 12
	for offset < len(buf)-8 {
		chunk := string(buf[offset : offset+4])
		size := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
		if chunk == "VP8X" && (buf[offset+8]&0x02) != 0 {
			return true
		}
		if chunk == "ANIM" || chunk == "ANMF" {
			return true
		}
		offset += 8 + int(size) + int(size%2)
	}
	return false
}

// classifyStickerBuffer detects (ext, mimetype, isAnimated, isLottie) from buffer + stickerMessage meta.
func classifyStickerBuffer(buf []byte, stickerMessage *waE2E.StickerMessage) (ext, mimetype string, isAnimated, isLottie bool) {
	if stickerMessage != nil && stickerMessage.GetIsLottie() {
		return "json", "application/json", true, true
	}
	return "webp", "image/webp", isAnimatedWebP(buf), false
}

// ─── Image → WEBP sticker ────────────────────────────────────────

// imageToStickerWEBP preprocesses (resize to ≤1024, preserve aspect) and encodes as WEBP.
// Quality starts at 90, fallback to 80/70 if too large (WhatsApp limit ~500KB).
func imageToStickerWEBP(imgBuf []byte) ([]byte, error) {
	// Decode any supported image format (jpeg/png/webp via registered decoders)
	img, err := imaging.Decode(bytes.NewReader(imgBuf), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	// Resize to max 1024 preserving aspect ratio
	resized := imaging.Fit(img, 1024, 1024, imaging.Lanczos)

	// Try multiple qualities to fit under WhatsApp limit
	qualities := []float32{95, 85, 75, 65, 55, 45, 35, 25}
	var lastErr error
	for _, q := range qualities {
		var buf bytes.Buffer
		err := webp.Encode(&buf, resized, &webp.Options{Quality: q, Lossless: false})
		if err == nil && buf.Len() > 0 && buf.Len() < 500*1024 {
			return buf.Bytes(), nil
		}
		lastErr = err
	}

	// Lossless fallback for tiny images (text-heavy)
	{
		var buf bytes.Buffer
		err := webp.Encode(&buf, resized, &webp.Options{Lossless: true})
		if err == nil && buf.Len() > 0 {
			return buf.Bytes(), nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("gagal encode WEBP")
}

// ─── Text → WEBP sticker (SVG-like via image/draw) ─────────────

// createStickerFromText draws text onto a transparent canvas, encodes as WEBP.
// Approximates the Baileys version which used SVG + Impact font + stroke.
// In Go, we use a system font (DejaVu Sans Bold) via golang.org/x/image/font
// but to keep deps minimal we draw a simpler bold text label with stroke.
func createStickerFromText(text string) ([]byte, error) {
	if text == "" {
		return nil, fmt.Errorf("text kosong")
	}
	upper := strings.ToUpper(text)

	// Canvas 1024x1024
	const W, H = 1024, 1024
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	// transparent background
	draw.Draw(img, img.Bounds(), image.Transparent, image.Point{}, draw.Src)

	// Approximate text drawing — use very simple bitmap blocks for clarity
	// (No font available, so we draw the text using freetype-like approach is overkill;
	// we'll draw using basic ascii block letters.)
	// NOTE: a more elegant solution would use github.com/golang/freetype but to keep deps minimal,
	// we delegate to a fallback: render text via image.Label (if available) or draw blocks.
	drawCenteredText(img, upper, W, H)

	// Resize to 512x512 for sticker
	resized := imaging.Fit(img, 512, 512, imaging.Lanczos)

	var buf bytes.Buffer
	if err := webp.Encode(&buf, resized, &webp.Options{Quality: 100, Lossless: true}); err != nil {
		return nil, err
	}
	if buf.Len() == 0 {
		return nil, fmt.Errorf("encode webp gagal")
	}
	return buf.Bytes(), nil
}

// drawCenteredText draws large text centered on the image with stroke + drop shadow.
// Uses very simple block-letter rendering (5x7 pixel font scaled up).
// For multi-word input, wraps at 12 chars per line.
func drawCenteredText(img *image.RGBA, text string, w, h int) {
	lines := wrapText(text, 12)
	fontSize := 80
	lineH := fontSize + 12
	if len(lines)*lineH > h-40 {
		fontSize = (h - 40) / (len(lines) + 1)
		lineH = fontSize + 12
	}
	// Use built-in block letters
	fillColor := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	strokeColor := color.RGBA{R: 0, G: 0, B: 0, A: 255}

	for i, line := range lines {
		// estimate width: each char ~ fontSize wide
		lineW := len(line) * fontSize
		// if too wide, scale down
		scale := 1
		if lineW > w-40 {
			scale = (w - 40) / lineW
			if scale < 1 {
				scale = 1
			}
		}
		_ = scale

		// draw block-letter version of each char
		xOff := (w - lineW) / 2
		yOff := 40 + i*lineH
		drawBlockLine(img, line, xOff, yOff, fontSize, fillColor, strokeColor)
	}
}

// wrapText splits text into lines of at most maxChars chars.
func wrapText(text string, maxChars int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{text}
	}
	var lines []string
	cur := ""
	for _, w := range words {
		test := cur
		if test != "" {
			test += " "
		}
		test += w
		if len(test) <= maxChars {
			cur = test
		} else {
			if cur != "" {
				lines = append(lines, cur)
			}
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		lines = []string{text}
	}
	return lines
}

// drawBlockLine draws a line of text using block letters (5x7 pixel grid scaled).
// Each character is drawn as 5x7 grid scaled to fontSize/5 wide per pixel.
// Stroke is drawn first (offset by 1 in all directions), then fill.
func drawBlockLine(img *image.RGBA, line string, x, y, fontSize int, fill, stroke color.RGBA) {
	// 5x7 font (limited uppercase letters + space + digits + symbols)
	font := blockFont()
	pixelSize := fontSize / 7
	if pixelSize < 4 {
		pixelSize = 4
	}

	for i, ch := range line {
		glyph, ok := font[ch]
		if !ok {
			glyph = font['?']
		}
		// draw each pixel of glyph
		for py := 0; py < 7; py++ {
			for px := 0; px < 5; px++ {
				if glyph[py]&(1<<(4-px)) != 0 {
					// fill pixel
					// draw a pixelSize x pixelSize block at (x + i*fontSize + px*pixelSize, y + py*pixelSize)
					baseX := x + i*(fontSize+pixelSize) + px*pixelSize
					baseY := y + py*pixelSize
					// stroke (draw rect offset)
					for offY := -pixelSize / 4; offY <= pixelSize/4; offY++ {
						for offX := -pixelSize / 4; offX <= pixelSize/4; offX++ {
							if offX != 0 || offY != 0 {
								drawPixelRect(img, baseX+offX, baseY+offY, pixelSize, pixelSize, stroke)
							}
						}
					}
					// fill
					drawPixelRect(img, baseX, baseY, pixelSize, pixelSize, fill)
				}
			}
		}
	}
}

// drawPixelRect fills a rect at (x,y) with size w×h.
func drawPixelRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	bounds := img.Bounds()
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			px := x + dx
			py := y + dy
			if px < bounds.Min.X || px >= bounds.Max.X || py < bounds.Min.Y || py >= bounds.Max.Y {
				continue
			}
			img.SetRGBA(px, py, c)
		}
	}
}

// blockFont returns a 5x7 ASCII font (uppercase A-Z, 0-9, space, basic punct).
// Each glyph is a [7]uint8 (7 rows of 5-bit, MSB = leftmost pixel).
// Adapted from public-domain 5x7 ASCII font.
func blockFont() map[rune][7]uint8 {
	return map[rune][7]uint8{
		' ': {0, 0, 0, 0, 0, 0, 0},
		'A': {0x0E, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
		'B': {0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E},
		'C': {0x0E, 0x11, 0x10, 0x10, 0x10, 0x11, 0x0E},
		'D': {0x1E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x1E},
		'E': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x1F},
		'F': {0x1F, 0x10, 0x10, 0x1E, 0x10, 0x10, 0x10},
		'G': {0x0E, 0x11, 0x10, 0x17, 0x11, 0x11, 0x0F},
		'H': {0x11, 0x11, 0x11, 0x1F, 0x11, 0x11, 0x11},
		'I': {0x0E, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0E},
		'J': {0x07, 0x02, 0x02, 0x02, 0x02, 0x12, 0x0C},
		'K': {0x11, 0x12, 0x14, 0x18, 0x14, 0x12, 0x11},
		'L': {0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x1F},
		'M': {0x11, 0x1B, 0x15, 0x15, 0x11, 0x11, 0x11},
		'N': {0x11, 0x11, 0x19, 0x15, 0x13, 0x11, 0x11},
		'O': {0x0E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
		'P': {0x1E, 0x11, 0x11, 0x1E, 0x10, 0x10, 0x10},
		'Q': {0x0E, 0x11, 0x11, 0x11, 0x15, 0x12, 0x0D},
		'R': {0x1E, 0x11, 0x11, 0x1E, 0x14, 0x12, 0x11},
		'S': {0x0F, 0x10, 0x10, 0x0E, 0x01, 0x01, 0x1E},
		'T': {0x1F, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
		'U': {0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E},
		'V': {0x11, 0x11, 0x11, 0x11, 0x11, 0x0A, 0x04},
		'W': {0x11, 0x11, 0x11, 0x15, 0x15, 0x15, 0x0A},
		'X': {0x11, 0x11, 0x0A, 0x04, 0x0A, 0x11, 0x11},
		'Y': {0x11, 0x11, 0x0A, 0x04, 0x04, 0x04, 0x04},
		'Z': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x10, 0x1F},
		'0': {0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E},
		'1': {0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E},
		'2': {0x0E, 0x11, 0x01, 0x06, 0x08, 0x10, 0x1F},
		'3': {0x1F, 0x01, 0x02, 0x06, 0x01, 0x11, 0x0E},
		'4': {0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02},
		'5': {0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E},
		'6': {0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E},
		'7': {0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
		'8': {0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E},
		'9': {0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C},
		'?': {0x0E, 0x11, 0x01, 0x02, 0x04, 0x00, 0x04},
		'!': {0x04, 0x04, 0x04, 0x04, 0x04, 0x00, 0x04},
		'.': {0x00, 0x00, 0x00, 0x00, 0x00, 0x0C, 0x0C},
		',': {0x00, 0x00, 0x00, 0x00, 0x0C, 0x0C, 0x08},
		':': {0x00, 0x0C, 0x0C, 0x00, 0x0C, 0x0C, 0x00},
		'-': {0x00, 0x00, 0x1F, 0x00, 0x00, 0x00, 0x00},
		'+': {0x00, 0x04, 0x04, 0x1F, 0x04, 0x04, 0x00},
		'/': {0x01, 0x02, 0x02, 0x04, 0x08, 0x08, 0x10},
	}
}

// ─── Send sticker via whatsmeow ──────────────────────────────────

// Default sticker watermark
const defaultStickerWM = "rubychan"

// uploadSticker uploads a WEBP buffer as WhatsApp sticker media and returns StickerMessage proto.
// wm = watermark text. If empty, uses default.
func uploadSticker(client *whatsmeow.Client, webpBuf []byte, isAnimated bool, wm string) (*waE2E.StickerMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if wm == "" {
		wm = defaultStickerWM
	}

	// CRITICAL: Use MediaImage, NOT MediaStickerPack.
	resp, err := client.Upload(ctx, webpBuf, whatsmeow.MediaImage)
	if err != nil {
		return nil, fmt.Errorf("upload sticker: %w", err)
	}

	mime := "image/webp"
	width := uint32(512)
	height := uint32(512)

	stickerMsg := &waE2E.StickerMessage{
		URL:                proto.String(resp.URL),
		FileSHA256:         resp.FileSHA256,
		FileEncSHA256:      resp.FileEncSHA256,
		MediaKey:           resp.MediaKey,
		Mimetype:           proto.String(mime),
		Height:             proto.Uint32(height),
		Width:              proto.Uint32(width),
		DirectPath:         proto.String(resp.DirectPath),
		FileLength:         proto.Uint64(uint64(len(webpBuf))),
		MediaKeyTimestamp:  proto.Int64(time.Now().Unix()),
		IsAnimated:         proto.Bool(isAnimated),
		AccessibilityLabel: proto.String(wm),
	}
	return stickerMsg, nil
}

// sendStickerMessage sends a sticker message to a chat, optionally quoting msg.
func sendStickerMessage(client *whatsmeow.Client, chat types.JID, webpBuf []byte, quoted *events.Message) error {
	return sendStickerMessageWithWM(client, chat, webpBuf, quoted, "")
}

// sendStickerMessageWithWM sends a sticker with custom watermark.
func sendStickerMessageWithWM(client *whatsmeow.Client, chat types.JID, webpBuf []byte, quoted *events.Message, wm string) error {
	stickerMsg, err := uploadSticker(client, webpBuf, false, wm)
	if err != nil {
		return err
	}

	out := &waE2E.Message{StickerMessage: stickerMsg}

	// attach context info if quoting
	if quoted != nil {
		quotedMsg := &waE2E.Message{
			Conversation: proto.String(extractText(quoted)),
		}
		if quoted.Message.GetExtendedTextMessage() != nil {
			quotedMsg = &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(extractText(quoted)),
				},
			}
		}
		if stickerMsg.ContextInfo == nil {
			stickerMsg.ContextInfo = &waE2E.ContextInfo{}
		}
		stickerMsg.ContextInfo.StanzaID = proto.String(quoted.Info.ID)
		stickerMsg.ContextInfo.Participant = proto.String(quoted.Info.Sender.String())
		stickerMsg.ContextInfo.QuotedMessage = quotedMsg
	}

	// Humanize
	out = humanizeMessage(out)

	_, err = client.SendMessage(context.Background(), chat, out)
	return err
}

// ─── Sticker pack upload (mimics Baileys uploadStickerPackAsset) ──

// uploadStickerPackAsset encrypts and uploads a buffer to WhatsApp media server.
// Returns the upload response containing DirectPath, FileSHA256, etc.
func uploadStickerPackAsset(client *whatsmeow.Client, buf []byte, hkdfInfo string, mediaPath string, mediaKey []byte) (*whatsmeow.UploadResponse, error) {
	if mediaKey == nil {
		mediaKey = make([]byte, 32)
		if _, err := rand.Read(mediaKey); err != nil {
			return nil, fmt.Errorf("rand mediaKey: %w", err)
		}
	}

	// HKDF expand to 112 bytes
	hkdfReader := hkdf.New(sha256.New, mediaKey, make([]byte, 32), []byte(hkdfInfo))
	expanded := make([]byte, 112)
	if _, err := io.ReadFull(hkdfReader, expanded); err != nil {
		return nil, err
	}

	iv := expanded[0:16]
	cipherKey := expanded[16:48]
	macKey := expanded[48:80]

	// AES-256-CBC encrypt (manual because Go stdlib uses CTR; we need CBC)
	// Actually, WhatsApp uses AES-256-CBC for media. We use our own CBC implementation.
	encrypted, err := aesCBCEncrypt(iv, cipherKey, buf)
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, macKey)
	mac.Write(iv)
	mac.Write(encrypted)
	macSum := mac.Sum(nil)
	encBuffer := append(encrypted, macSum[:10]...)

	// Upload via whatsmeow's MediaStickerPack
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := client.Upload(ctx, encBuffer, whatsmeow.MediaStickerPack)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	_ = mediaPath // not used directly; whatsmeow handles routing
	return &resp, nil
}

// sendCustomStickerPack builds a ZIP of sticker WEBPs + tray icon and sends a sticker pack message.
func sendCustomStickerPack(client *whatsmeow.Client, chat types.JID, pack []StickerItem) error {
	zipBuf := new(bytes.Buffer)
	zw := zip.NewWriter(zipBuf)

	var stickersMeta []*waE2E.StickerPackMessage_Sticker
	for _, item := range pack {
		itemBuf, _ := b64decodeStd(item.Buffer)
		fileName := toB64URL(sha256Bytes(itemBuf)) + "." + item.Ext

		fh, err := zw.Create(fileName)
		if err != nil {
			return err
		}
		if _, err := fh.Write(itemBuf); err != nil {
			return fmt.Errorf("zip write %s: %w", fileName, err)
		}

		stickersMeta = append(stickersMeta, &waE2E.StickerPackMessage_Sticker{
			FileName:           proto.String(fileName),
			IsAnimated:         proto.Bool(item.IsAnimated),
			Emojis:             []string{""},
			AccessibilityLabel: proto.String(""),
			IsLottie:           proto.Bool(item.IsLottie),
			Mimetype:           proto.String(item.Mimetype),
		})
	}

	// tray icon (use first non-lottie sticker or blank)
	trayBuf := make([]byte, 0)
	for _, item := range pack {
		if !item.IsLottie {
			trayBuf, _ = b64decodeStd(item.Buffer)
			break
		}
	}
	if len(trayBuf) == 0 {
		// empty 96x96 transparent WEBP
		img := image.NewRGBA(image.Rect(0, 0, 96, 96))
		var tb bytes.Buffer
		_ = webp.Encode(&tb, img, &webp.Options{Lossless: true})
		trayBuf = tb.Bytes()
	} else {
		// resize to 96x96
		img, err := imaging.Decode(bytes.NewReader(trayBuf))
		if err == nil {
			resized := imaging.Fit(img, 96, 96, imaging.Lanczos)
			var tb bytes.Buffer
			_ = webp.Encode(&tb, resized, &webp.Options{Lossless: true})
			trayBuf = tb.Bytes()
		}
	}
	trayFileName := "tray_icon.webp"
	fh, err := zw.Create(trayFileName)
	if err != nil {
		return fmt.Errorf("zip create %s: %w", trayFileName, err)
	}
	if _, err := fh.Write(trayBuf); err != nil {
		return fmt.Errorf("zip write %s: %w", trayFileName, err)
	}

	if err := zw.Close(); err != nil {
		return err
	}

	// Upload pack zip
	packUpload, err := uploadStickerPackAsset(client, zipBuf.Bytes(), "WhatsApp Sticker Pack Keys", "/mms/sticker-pack", nil)
	if err != nil {
		return fmt.Errorf("upload pack: %w", err)
	}

	// Upload thumbnail
	thumbUpload, err := uploadStickerPackAsset(client, trayBuf, "WhatsApp Sticker Pack Thumbnail Keys", "/mms/thumbnail-sticker-pack", packUpload.MediaKey)
	if err != nil {
		return fmt.Errorf("upload thumb: %w", err)
	}

	// Build StickerPackMessage
	packID := "Pack_" + uuidHex()[:16]
	spMsg := &waE2E.StickerPackMessage{
		StickerPackID:       proto.String(packID),
		Name:                proto.String("Hiura Mihate"),
		Publisher:           proto.String("Hiura Bot"),
		PackDescription:     proto.String("Sticker pack dibuat menggunakan Hiura Mihate Bot"),
		Stickers:            stickersMeta,
		FileLength:          proto.Uint64(uint64(zipBuf.Len())),
		FileSHA256:          packUpload.FileSHA256,
		FileEncSHA256:       packUpload.FileEncSHA256,
		MediaKey:            packUpload.MediaKey,
		DirectPath:          proto.String(packUpload.DirectPath),
		MediaKeyTimestamp:   proto.Int64(time.Now().Unix()),
		StickerPackSize:     proto.Uint64(uint64(zipBuf.Len())),
		StickerPackOrigin:   waE2E.StickerPackMessage_USER_CREATED.Enum(),
		TrayIconFileName:    proto.String(trayFileName),
		ThumbnailDirectPath: proto.String(thumbUpload.DirectPath),
		ThumbnailSHA256:     thumbUpload.FileSHA256,
		ThumbnailEncSHA256:  thumbUpload.FileEncSHA256,
		ThumbnailHeight:     proto.Uint32(96),
		ThumbnailWidth:      proto.Uint32(96),
		ImageDataHash:       proto.String(toB64URL(thumbUpload.FileSHA256)),
	}

	out := &waE2E.Message{StickerPackMessage: spMsg}
	_, err = client.SendMessage(context.Background(), chat, out)
	return err
}

// ─── Sticker command handlers ────────────────────────────────────

// handleStickerCommand processes .s / .stiker / .sticker
func handleStickerCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(botClient, msg) {
		return "❌ Hanya owner yang bisa membuat sticker saat self mode."
	}

	text := strings.TrimSpace(args)
	chatID := msg.Info.Chat

	// Try to get image from quoted message
	var imageBuf []byte
	if quoted := getQuotedMessage(msg); quoted != nil {
		if buf, mime, ok := downloadQuotedImage(client, quoted); ok {
			imageBuf = buf
			_ = mime
		}
	}

	// Try to get image from this message (image + caption .s)
	if len(imageBuf) == 0 {
		if buf, _, ok := downloadQuotedImage(client, msg); ok {
			imageBuf = buf
		}
	}

	if len(imageBuf) > 0 {
		if len(imageBuf) > maxStickerDownloadBytes {
			return "❌ File terlalu besar (max 50MB)."
		}
		// Convert to sticker
		stickerBuf, err := imageToStickerWEBP(imageBuf)
		if err != nil {
			return fmt.Sprintf("❌ Gagal convert gambar: %v", err)
		}
		// Send
		if err := sendStickerMessage(client, chatID, stickerBuf, msg); err != nil {
			// retry without quoted
			if err2 := sendStickerMessage(client, chatID, stickerBuf, nil); err2 != nil {
				return fmt.Sprintf("❌ Gagal kirim sticker: %v", err2)
			}
		}
		return "" // sticker sent, no text reply
	}

	if text == "" {
		return "🖼️ *Sticker*\n\n" +
			"• `.s <teks>` — buat sticker dari teks\n" +
			"• `.s` — reply gambar / kirim gambar caption `.s`"
	}

	// Text → sticker
	stickerBuf, err := createStickerFromText(text)
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat sticker teks: %v", err)
	}
	if err := sendStickerMessage(client, chatID, stickerBuf, msg); err != nil {
		if err2 := sendStickerMessage(client, chatID, stickerBuf, nil); err2 != nil {
			return fmt.Sprintf("❌ Gagal kirim sticker: %v", err2)
		}
	}
	return ""
}

// handleAnticolongCommand processes .anticolong / .setattr
func handleAnticolongCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(botClient, msg) {
		return "❌ Hanya owner yang bisa pakai .anticolong saat self mode."
	}
	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❌ Reply sticker WEBP dulu."
	}

	buf, _, ok := downloadQuotedSticker(client, quoted)
	if !ok {
		return "❌ Gagal download sticker. Coba reply ulang."
	}

	if !isRiffWebP(buf) {
		return "❌ Bukan sticker WEBP."
	}

	// Engine baru: webpmux dulu, fallback ke Go
	finalBuf, exifErr := InjectWebPMetadata(buf, StickerEXIFPackname, StickerEXIFAuthor)
	if exifErr != nil || !isRiffWebP(finalBuf) {
		fmt.Printf("[anticolong] webpmux gagal (%v), fallback Go\n", exifErr)
		finalBuf = applyAnticolongExifToSticker(buf)
		if !isRiffWebP(finalBuf) {
			finalBuf = buf
		}
	}
	if err := sendStickerMessage(client, msg.Info.Chat, finalBuf, msg); err != nil {
		if err2 := sendStickerMessage(client, msg.Info.Chat, finalBuf, nil); err2 != nil {
			return fmt.Sprintf("❌ Gagal kirim: %v", err2)
		}
	}
	return ""
}

// handleCWMCommand processes .cwm [teks] / .cwm -n [teks]
func handleCWMCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(botClient, msg) {
		return "❌ Hanya owner yang bisa pakai .cwm saat self mode."
	}

	cwmArgs := strings.TrimSpace(args)
	noAnticolong := false
	if strings.HasPrefix(cwmArgs, "-n ") || strings.HasPrefix(cwmArgs, "--no-anticolong ") {
		noAnticolong = true
		cwmArgs = strings.TrimSpace(strings.TrimPrefix(cwmArgs, "-n "))
		cwmArgs = strings.TrimSpace(strings.TrimPrefix(cwmArgs, "--no-anticolong "))
	}

	// Parse "packname | author" — contoh: .cwm lo jelek | by orang alay
	packname := StickerEXIFPackname
	author := StickerEXIFAuthor
	if cwmArgs != "" {
		parts := strings.SplitN(cwmArgs, "|", 2)
		if p := strings.TrimSpace(parts[0]); p != "" {
			packname = p
		}
		if len(parts) > 1 {
			if a := strings.TrimSpace(parts[1]); a != "" {
				author = a
			}
		}
	}
	// Fallback: jika tidak ada "|", pakai teks sebagai packname+author (kompatibel lama)
	watermark := packname
	if cwmArgs != "" && !strings.Contains(cwmArgs, "|") {
		watermark = cwmArgs
		packname = cwmArgs
		author = cwmArgs
	}

	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "🖼️ *Custom Watermark Sticker*\n\n" +
			"• `.cwm <teks>` — ganti wm + anticolong\n" +
			"• `.cwm <pack> | <author>` — pack & author terpisah\n" +
			"• `.cwm -n <teks>` — ganti wm tanpa anticolong\n\n" +
			"❌ Reply sticker WEBP dulu."
	}

	buf, _, ok := downloadQuotedSticker(client, quoted)
	if !ok {
		return "❌ Gagal download sticker."
	}

	// Inject EXIF via webpmux (engine baru), fallback ke Go jika gagal
	var finalBuf []byte
	if !noAnticolong {
		injected, err := InjectWebPMetadata(buf, packname, author)
		if err != nil {
			fmt.Printf("[cwm] webpmux gagal (%v), fallback ke Go EXIF\n", err)
			// Fallback: metadata anticolong via Go
			packID := uuidHex()
			metadata := map[string]any{
				"sticker-pack-id":        packID,
				"sticker-pack-name":      packname,
				"sticker-pack-publisher": author,
				"accessibility-text":     watermark,
				"android-app-store-link": "https://whatsapp.com",
				"ios-app-store-link":     "https://whatsapp.com/ios",
				"is-from-sticker-maker":  0,
				"is-avatar-sticker":      1,
				"is-ai-sticker":          1,
			}
			exifBuf, exifErr := setWebpExif(buf, metadata)
			if exifErr != nil || !isRiffWebP(exifBuf) {
				finalBuf = buf
			} else {
				finalBuf = exifBuf
			}
		} else {
			finalBuf = injected
		}
	} else {
		finalBuf = buf
	}

	if err := sendStickerMessageWithWM(client, msg.Info.Chat, finalBuf, msg, watermark); err != nil {
		if err2 := sendStickerMessageWithWM(client, msg.Info.Chat, finalBuf, nil, watermark); err2 != nil {
			return fmt.Sprintf("❌ Gagal kirim: %v", err2)
		}
	}
	return ""
}

// handleTSPKCommand processes .tspk / .addtspk / .deltspk
func handleTSPKCommand(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(botClient, msg) {
		return "❌ Hanya owner yang bisa pakai .tspk / .addtspk / .deltspk."
	}
	loadStickerStore()
	senderID := msg.Info.Sender.String()
	store := getStickerUserStore(senderID)

	cmd = strings.ToLower(cmd)

	switch cmd {
	case ".tspk":
		if len(store.LegacyPack) == 0 {
			return "❌ Pack masih kosong. Tambahkan dulu dengan `.addtspk`."
		}
		if err := sendCustomStickerPack(client, msg.Info.Chat, store.LegacyPack); err != nil {
			return fmt.Sprintf("❌ Gagal kirim pack: %v", err)
		}
		return ""

	case ".addtspk":
		quoted := getQuotedMessage(msg)
		if quoted == nil {
			return "❌ Reply sticker yang mau diproses."
		}
		buf, stickerMsg, ok := downloadQuotedSticker(client, quoted)
		if !ok {
			return "❌ Gagal download sticker."
		}
		hash := ""
		if stickerMsg != nil && len(stickerMsg.GetFileSHA256()) > 0 {
			hash = fmt.Sprintf("%x", stickerMsg.GetFileSHA256())
		} else {
			hash = fmt.Sprintf("%x", sha256Bytes(buf))
		}
		// dedup
		for _, it := range store.LegacyPack {
			if it.SHA256 == hash {
				return "❌ Sticker sudah ada di pack."
			}
		}
		if len(store.LegacyPack) >= maxLegacyPackSize {
			return fmt.Sprintf("❌ Pack penuh (max %d sticker). Hapus dulu dengan .deltspk.", maxLegacyPackSize)
		}
		ext, mime, isAnim, isLottie := classifyStickerBuffer(buf, stickerMsg)
		store.LegacyPack = append(store.LegacyPack, StickerItem{
			SHA256:     hash,
			Buffer:     b64encodeStd(buf),
			Ext:        ext,
			Mimetype:   mime,
			IsAnimated: isAnim,
			IsLottie:   isLottie,
		})
		_ = saveStickerStore()
		label := "static"
		if isLottie {
			label = "lottie"
		} else if isAnim {
			label = "animated"
		}
		return fmt.Sprintf("✅ Ditambahkan (%s). Total: %d sticker di pack.", label, len(store.LegacyPack))

	case ".deltspk":
		quoted := getQuotedMessage(msg)
		if quoted == nil {
			return "❌ Reply sticker yang mau dihapus."
		}
		buf, stickerMsg, ok := downloadQuotedSticker(client, quoted)
		if !ok {
			return "❌ Gagal download sticker."
		}
		hash := ""
		if stickerMsg != nil && len(stickerMsg.GetFileSHA256()) > 0 {
			hash = fmt.Sprintf("%x", stickerMsg.GetFileSHA256())
		} else {
			hash = fmt.Sprintf("%x", sha256Bytes(buf))
		}
		idx := -1
		for i, it := range store.LegacyPack {
			if it.SHA256 == hash {
				idx = i
				break
			}
		}
		if idx == -1 {
			return "❌ Sticker tidak ditemukan di pack."
		}
		store.LegacyPack = append(store.LegacyPack[:idx], store.LegacyPack[idx+1:]...)
		_ = saveStickerStore()
		return fmt.Sprintf("✅ Dihapus. Sisa: %d sticker di pack.", len(store.LegacyPack))
	}

	return "❓ *Cara pakai:* .tspk / .addtspk / .deltspk"
}

// handleSTCCommand processes .stc save/send/anticolong
func handleSTCCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(botClient, msg) {
		return "❌ Hanya owner yang bisa pakai .stc."
	}
	loadStickerStore()
	senderID := msg.Info.Sender.String()

	parts := strings.Fields(args)
	if len(parts) == 0 {
		return `📦 *Sticker Pack (.stc)*

• .stc save <id> — simpan sticker (reply) ke pack <id>
• .stc send pack <id> — kirim pack sebagai sticker pack
• .stc anticolong on/off — toggle anticolong payload

Legacy (1 pack per user):
• .addtspk — tambah sticker ke pack
• .deltspk — hapus sticker dari pack
• .tspk — kirim sticker pack`
	}

	sub := strings.ToLower(parts[0])

	switch sub {
	case "anticolong":
		if len(parts) < 2 || (parts[1] != "on" && parts[1] != "off") {
			return "❓ *Cara pakai:* .stc anticolong on|off"
		}
		store := getStickerUserStore(senderID)
		store.Anticolong = parts[1] == "on"
		_ = saveStickerStore()
		if store.Anticolong {
			return "✅ Anticolong aktif. Semua sticker yang dikirim lewat `.stc send pack <id>` otomatis disisipi payload anticolong."
		}
		return "✅ Anticolong dimatikan."

	case "save":
		if len(parts) < 2 {
			return "❓ *Cara pakai:* .stc save <id>\nContoh: .stc save lucu1"
		}
		id := parts[1]
		quoted := getQuotedMessage(msg)
		if quoted == nil {
			return "❌ Reply sticker yang mau disimpan."
		}
		buf, stickerMsg, ok := downloadQuotedSticker(client, quoted)
		if !ok {
			return "❌ Gagal download sticker."
		}
		hash := ""
		if stickerMsg != nil && len(stickerMsg.GetFileSHA256()) > 0 {
			hash = fmt.Sprintf("%x", stickerMsg.GetFileSHA256())
		} else {
			hash = fmt.Sprintf("%x", sha256Bytes(buf))
		}
		store := getStickerUserStore(senderID)
		list, ok := store.Packs[id]
		if !ok {
			list = []StickerItem{}
		}
		for _, it := range list {
			if it.SHA256 == hash {
				return fmt.Sprintf("❌ Sticker sudah ada di pack \"%s\".", id)
			}
		}
		if len(list) >= maxStickersPerPack {
			return fmt.Sprintf("❌ Pack \"%s\" penuh (max %d sticker).", id, maxStickersPerPack)
		}
		ext, mime, isAnim, isLottie := classifyStickerBuffer(buf, stickerMsg)
		list = append(list, StickerItem{
			SHA256:     hash,
			Buffer:     b64encodeStd(buf),
			Ext:        ext,
			Mimetype:   mime,
			IsAnimated: isAnim,
			IsLottie:   isLottie,
		})
		store.Packs[id] = list
		_ = saveStickerStore()
		label := "static"
		if isLottie {
			label = "lottie"
		} else if isAnim {
			label = "animated"
		}
		return fmt.Sprintf("✅ Tersimpan (%s) ke pack \"%s\". Total %d sticker.", label, id, len(list))

	case "send":
		if len(parts) < 3 || parts[1] != "pack" {
			return "❓ *Cara pakai:* .stc send pack <id>"
		}
		id := parts[2]
		store := getStickerUserStore(senderID)
		list, ok := store.Packs[id]
		if !ok || len(list) == 0 {
			return fmt.Sprintf("❌ Pack \"%s\" masih kosong. Tambahkan dulu dengan `.stc save %s`.", id, id)
		}

		// If anticolong active, inject EXIF for each non-lottie sticker
		finalPack := list
		if store.Anticolong {
			finalPack = make([]StickerItem, len(list))
			for i, item := range list {
				if item.IsLottie {
					finalPack[i] = item
					continue
				}
				buf, _ := b64decodeStd(item.Buffer)
				injected := applyAnticolongExifToSticker(buf)
				finalPack[i] = item
				finalPack[i].Buffer = b64encodeStd(injected)
			}
		}

		if err := sendCustomStickerPack(client, msg.Info.Chat, finalPack); err != nil {
			return fmt.Sprintf("❌ Gagal kirim pack: %v", err)
		}
		return ""
	}

	return "❓ Sub-command tidak dikenal. Ketik `.stc` untuk bantuan."
}

// ─── Media helpers (download image/sticker from quoted message) ─

// getQuotedMessage returns the quoted message if msg quotes one.
// whatsmeow populates msg.Message.ExtendedTextMessage.ContextInfo.QuotedMessage
// with the full original message (image, sticker, video, etc.).
// We wrap it in a synthetic *events.Message so existing call sites can use it.
// quotedContextInfo mengekstrak ContextInfo dari SEMUA tipe konten pesan,
// bukan hanya ExtendedTextMessage. Reply ke gambar/video/dokumen/audio/stiker
// juga membawa ContextInfo (termasuk quoted message).
func quotedContextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	if m == nil {
		return nil
	}
	if ci := m.GetExtendedTextMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetImageMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetVideoMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetDocumentMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetAudioMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetStickerMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetContactMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetLocationMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetLiveLocationMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetPollCreationMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetButtonsMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetListMessage().GetContextInfo(); ci != nil {
		return ci
	}
	if ci := m.GetTemplateMessage().GetContextInfo(); ci != nil {
		return ci
	}
	// Wrapper: view-once / ephemeral / device-sent bisa membungkus konten.
	if vm := m.GetViewOnceMessage(); vm != nil {
		if ci := quotedContextInfo(vm.GetMessage()); ci != nil {
			return ci
		}
	}
	if vm := m.GetViewOnceMessageV2(); vm != nil {
		if ci := quotedContextInfo(vm.GetMessage()); ci != nil {
			return ci
		}
	}
	if em := m.GetEphemeralMessage(); em != nil {
		if ci := quotedContextInfo(em.GetMessage()); ci != nil {
			return ci
		}
	}
	if dm := m.GetDeviceSentMessage(); dm != nil {
		if ci := quotedContextInfo(dm.GetMessage()); ci != nil {
			return ci
		}
	}
	return nil
}

func getQuotedMessage(msg *events.Message) *events.Message {
	if msg == nil || msg.Message == nil {
		return nil
	}
	ext := quotedContextInfo(msg.Message)
	if ext == nil {
		return nil
	}
	qMsg := ext.GetQuotedMessage()
	if qMsg == nil {
		return nil
	}
	// Build synthetic event with the quoted message + sender info from ContextInfo.
	synthetic := &events.Message{Message: qMsg}
	synthetic.Info.ID = ext.GetStanzaID()
	synthetic.Info.Chat = msg.Info.Chat
	synthetic.Info.Sender = types.EmptyJID
	if part := ext.GetParticipant(); part != "" {
		if jid, err := types.ParseJID(part); err == nil {
			synthetic.Info.Sender = jid
		}
	}
	// Set IsFromMe by comparing sender against bot's own JID (dual form).
	// This is CRITICAL for auto-trigger: main.go checks quoted.Info.IsFromMe
	// to decide whether to invoke AI on reply. Without this, IsFromMe is
	// always false and AI never auto-responds to replies.
	if botClient != nil && botClient.Store != nil && botClient.Store.ID != nil {
		ownJID := *botClient.Store.ID // dereference pointer
		sender := synthetic.Info.Sender
		if !sender.IsEmpty() {
			// Direct match
			if sender.String() == ownJID.String() || sender.User == ownJID.User {
				synthetic.Info.IsFromMe = true
			} else {
				// Dual-form match: resolve both JIDs to PN and compare
				for _, form := range dualForms(botClient, sender) {
					if form.String() == ownJID.String() || form.User == ownJID.User {
						synthetic.Info.IsFromMe = true
						break
					}
					pn1 := phoneOf(botClient, form)
					pn2 := phoneOf(botClient, ownJID)
					if pn1 != "" && pn1 == pn2 {
						synthetic.Info.IsFromMe = true
						break
					}
				}
			}
		}
	}
	return synthetic
}

// downloadQuotedImage tries to download an image from a message (own media only).
// Returns (buf, mime, ok).
func downloadQuotedImage(client *whatsmeow.Client, msg *events.Message) ([]byte, string, bool) {
	if msg == nil || msg.Message == nil {
		return nil, "", false
	}
	img := msg.Message.GetImageMessage()
	if img != nil {
		buf, err := downloadMedia(client, img)
		if err != nil || len(buf) == 0 {
			return nil, "", false
		}
		return buf, img.GetMimetype(), true
	}
	// try document with image mimetype
	doc := msg.Message.GetDocumentMessage()
	if doc != nil && strings.HasPrefix(doc.GetMimetype(), "image/") {
		buf, err := downloadMedia(client, doc)
		if err != nil || len(buf) == 0 {
			return nil, "", false
		}
		return buf, doc.GetMimetype(), true
	}
	return nil, "", false
}

// downloadQuotedSticker tries to download a sticker from a quoted message.
// Since whatsmeow doesn't expose the quoted message directly, we rely on the
// actual quoted msg being available in the events.Message struct.
// We try msg.Message.StickerMessage first, then look at extended text context.
func downloadQuotedSticker(client *whatsmeow.Client, msg *events.Message) ([]byte, *waE2E.StickerMessage, bool) {
	if msg == nil || msg.Message == nil {
		return nil, nil, false
	}
	sticker := msg.Message.GetStickerMessage()
	if sticker != nil {
		buf, err := downloadMedia(client, sticker)
		if err != nil || len(buf) == 0 {
			return nil, nil, false
		}
		return buf, sticker, true
	}
	return nil, nil, false
}

// downloadMedia downloads a DownloadableMessage via whatsmeow and returns the raw bytes.
func downloadMedia(client *whatsmeow.Client, downloadable whatsmeow.DownloadableMessage) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return client.Download(ctx, downloadable)
}

// ─── HTTP download (for remote image URLs from Kangwifi maker) ─

// httpDownload fetches a URL with timeout and size limit.
func httpDownload(url string, maxBytes int) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = 10 * 1024 * 1024 // 10MB default
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	mime := resp.Header.Get("Content-Type")
	limited := io.LimitReader(resp.Body, int64(maxBytes))
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}
	return buf, mime, nil
}

// ─── Encoding helpers ───────────────────────────────────────────

func b64encodeStd(buf []byte) string {
	return b64encode(buf, false)
}

func b64decodeStd(s string) ([]byte, error) {
	return b64decode(s, false)
}

// ─── AES-256-CBC (manual implementation for sticker pack upload) ─

// aesCBCEncrypt encrypts plaintext with AES-256-CBC using key and IV.
func aesCBCEncrypt(iv, key, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("AES-256 key must be 32 bytes")
	}
	if len(iv) != 16 {
		return nil, fmt.Errorf("AES CBC IV must be 16 bytes")
	}
	// PKCS7 padding
	padLen := 16 - (len(plaintext) % 16)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}

	// Use stdlib crypto/aes
	cipher, err := aesNewCipher(key)
	if err != nil {
		return nil, err
	}

	encrypted := make([]byte, len(padded))
	prev := iv
	for i := 0; i < len(padded); i += 16 {
		var block [16]byte
		copy(block[:], padded[i:i+16])
		// XOR with prev (CBC)
		for j := 0; j < 16; j++ {
			block[j] ^= prev[j]
		}
		cipher.Encrypt(encrypted[i:i+16], block[:])
		prev = encrypted[i : i+16]
	}
	return encrypted, nil
}

// ─── fileExists / file helpers ───────────────────────────────────

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
