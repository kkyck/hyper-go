package main

// ─── .swm — Smart Watermark Sticker ─────────────────────────────────────
// Plugin ORISINAL: watermark VISIBLE di atas sticker + EXIF pack name.
//
// Belum pernah ada di bot WA lain: gabungan watermark terlihat (4 style)
// + QR code watermark yang bisa di-scan + EXIF pack name, dalam 1 command.
//
//   Reply gambar/sticker:
//   .swm [teks]      → diagonal tiled watermark (ala stock photo)
//   .swm -b [teks]   → bottom banner (bar hitam + teks putih)
//   .swm -c [teks]   → corner badge (pojok kanan bawah, dengan shadow)
//   .swm -o [teks]   → outline center (teks besar tengah, outline hitam)
//   .swm -qr [teks]  → QR code watermark (pojok kanan bawah, BISA DI-SCAN!)
//
// Semua style juga meng-set EXIF sticker-pack-name = teks watermark.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"strings"

	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
	"github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var swmFontCache *opentype.Font

// swmLoadFont memuat DejaVu Sans Bold dari system fonts (sekali saja).
func swmLoadFont() (*opentype.Font, error) {
	if swmFontCache != nil {
		return swmFontCache, nil
	}
	paths := []string{
		"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
		"/usr/share/fonts/truetype/noto/NotoSans-Bold.ttf",
	}
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			if f, err := opentype.Parse(b); err == nil {
				swmFontCache = f
				return f, nil
			}
		}
	}
	return nil, fmt.Errorf("font bold tidak ditemukan")
}

// swmMeasure mengukur lebar & tinggi teks pada ukuran font tertentu.
func swmMeasure(f *opentype.Font, text string, size float64) (w, h int) {
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72})
	if err != nil {
		return len(text) * int(size) / 2, int(size)
	}
	defer face.Close()
	d := &font.Drawer{Face: face}
	bounds, _ := d.BoundString(text)
	w = (bounds.Max.X - bounds.Min.X).Ceil()
	h = (bounds.Max.Y - bounds.Min.Y).Ceil()
	if w <= 0 {
		w = len(text) * int(size) / 2
	}
	if h <= 0 {
		h = int(size)
	}
	return w, h
}

// swmDrawText menggambar teks dengan outline + shadow ke RGBA.
func swmDrawText(dst *image.NRGBA, f *opentype.Font, text string, x, y int, size float64, fill color.Color) {
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72})
	if err != nil {
		return
	}
	defer face.Close()

	// Outline hitam (8 arah) + shadow
	outline := color.RGBA{0, 0, 0, 220}
	shadow := color.RGBA{0, 0, 0, 120}
	off := int(size / 14)
	if off < 2 {
		off = 2
	}
	drawAt := func(dx, dy int, c color.Color) {
		d := &font.Drawer{
			Dst:  dst,
			Src:  image.NewUniform(c),
			Face: face,
			Dot:  fixed.P(x+dx, y+dy),
		}
		d.DrawString(text)
	}
	// shadow
	drawAt(off+2, off+3, shadow)
	// outline
	for _, o := range [][2]int{{-off, 0}, {off, 0}, {0, -off}, {0, off}, {-off, -off}, {off, -off}, {-off, off}, {off, off}} {
		drawAt(o[0], o[1], outline)
	}
	// fill
	drawAt(0, 0, fill)
}

// swmOverlay menempel layer ke base dengan opacity (0.0–1.0).
func swmOverlay(base *image.NRGBA, layer image.Image, pos image.Point, opacity float64) *image.NRGBA {
	return imaging.Overlay(base, layer, pos, opacity)
}

// ─── Style 1: Diagonal tiled (ala watermark stock photo) ───

func swmDiagonal(base *image.NRGBA, f *opentype.Font, text string) *image.NRGBA {
	W := base.Bounds().Dx()
	H := base.Bounds().Dy()
	fontSize := float64(W) / 11
	if fontSize < 18 {
		fontSize = 18
	}

	// Render 1 tile teks
	tw, th := swmMeasure(f, text, fontSize)
	tile := image.NewNRGBA(image.Rect(0, 0, tw+40, th+30))
	swmDrawText(tile, f, text, 20, th+5, fontSize, color.RGBA{255, 255, 255, 255})

	// Rotate -30 derajat
	rotated := imaging.Rotate(tile, -30, color.Transparent)

	// Tile di seluruh kanvas
	layer := image.NewNRGBA(base.Bounds())
	rw := rotated.Bounds().Dx()
	rh := rotated.Bounds().Dy()
	for y := -rh; y < H+rh; y += rh + 40 {
		for x := -rw; x < W+rw; x += rw + 60 {
			layer = swmOverlay(layer, rotated, image.Pt(x, y), 1.0)
		}
	}
	return swmOverlay(base, layer, image.Pt(0, 0), 0.28)
}

// ─── Style 2: Bottom banner ───

func swmBottomBanner(base *image.NRGBA, f *opentype.Font, text string) *image.NRGBA {
	W := base.Bounds().Dx()
	H := base.Bounds().Dy()
	barH := H / 7
	if barH < 36 {
		barH = 36
	}

	// Bar hitam semi-transparan
	bar := image.NewNRGBA(image.Rect(0, 0, W, barH))
	draw.Draw(bar, bar.Bounds(), &image.Uniform{color.RGBA{0, 0, 0, 170}}, image.Point{}, draw.Src)

	// Teks putih di tengah bar
	fontSize := float64(barH) * 0.52
	tw, _ := swmMeasure(f, text, fontSize)
	tx := (W - tw) / 2
	ty := barH/2 + int(fontSize*0.36)
	swmDrawText(bar, f, text, tx, ty, fontSize, color.RGBA{255, 255, 255, 255})

	return swmOverlay(base, bar, image.Pt(0, H-barH), 1.0)
}

// ─── Style 3: Corner badge ───

func swmCorner(base *image.NRGBA, f *opentype.Font, text string) *image.NRGBA {
	W := base.Bounds().Dx()
	H := base.Bounds().Dy()
	fontSize := float64(W) / 16
	if fontSize < 14 {
		fontSize = 14
	}

	tw, th := swmMeasure(f, text, fontSize)
	pad := 12
	badge := image.NewNRGBA(image.Rect(0, 0, tw+pad*2, th+pad*2))
	// Background badge rounded-ish (kotak dengan alpha)
	draw.Draw(badge, badge.Bounds(), &image.Uniform{color.RGBA{0, 0, 0, 140}}, image.Point{}, draw.Src)
	swmDrawText(badge, f, text, pad, pad+th, fontSize, color.RGBA{255, 255, 255, 255})

	margin := 14
	return swmOverlay(base, badge, image.Pt(W-badge.Bounds().Dx()-margin, H-badge.Bounds().Dy()-margin), 0.92)
}

// ─── Style 4: Outline center (teks besar di tengah) ───

func swmOutlineCenter(base *image.NRGBA, f *opentype.Font, text string) *image.NRGBA {
	W := base.Bounds().Dx()
	H := base.Bounds().Dy()

	// Cari font size terbesar yang muat (max 80% lebar)
	var fontSize float64 = 120
	for fontSize > 20 {
		tw, _ := swmMeasure(f, text, fontSize)
		if tw <= int(float64(W)*0.82) {
			break
		}
		fontSize -= 8
	}

	tw, th := swmMeasure(f, text, fontSize)
	layer := image.NewNRGBA(base.Bounds())
	// Teks putih dengan outline tebal
	swmDrawText(layer, f, text, (W-tw)/2, (H-th)/2+th, fontSize, color.RGBA{255, 255, 255, 235})

	return swmOverlay(base, layer, image.Pt(0, 0), 0.85)
}

// ─── Style 5: QR code watermark (NOVELTY: bisa di-scan!) ───

func swmQR(base *image.NRGBA, qrText string) (*image.NRGBA, error) {
	W := base.Bounds().Dx()
	H := base.Bounds().Dy()
	qrSize := W / 4
	if qrSize < 90 {
		qrSize = 90
	}
	if qrSize > 200 {
		qrSize = 200
	}

	qr, err := qrcode.New(qrText, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("gagal bikin QR: %w", err)
	}
	qr.DisableBorder = false
	pngBytes, err := qr.PNG(qrSize)
	if err != nil {
		return nil, fmt.Errorf("gagal encode QR: %w", err)
	}
	qrImg, err := imaging.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("gagal decode QR: %w", err)
	}

	// Background putih rounded + QR di pojok kanan bawah
	margin := 12
	bgPad := 8
	bgSize := qrSize + bgPad*2
	bg := image.NewNRGBA(image.Rect(0, 0, bgSize, bgSize))
	draw.Draw(bg, bg.Bounds(), &image.Uniform{color.RGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)
	bg = imaging.Overlay(bg, qrImg, image.Pt(bgPad, bgPad), 1.0)

	return imaging.Overlay(base, bg, image.Pt(W-bgSize-margin, H-bgSize-margin), 0.95), nil
}

// ─── Handler ───

func handleSWMCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(botClient, msg) {
		return "🔒 Hanya owner/trusted/admin yang bisa pakai .swm."
	}

	// Parse style flag
	style := "diagonal"
	rest := strings.TrimSpace(args)
	if strings.HasPrefix(rest, "-b ") {
		style = "bottom"
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "-b "))
	} else if strings.HasPrefix(rest, "-c ") {
		style = "corner"
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "-c "))
	} else if strings.HasPrefix(rest, "-o ") {
		style = "outline"
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "-o "))
	} else if strings.HasPrefix(rest, "-qr ") {
		style = "qr"
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "-qr "))
	}
	wmText := rest
	if wmText == "" {
		return "🏷️ *Smart Watermark Sticker*\n\n" +
			"Reply gambar/sticker:\n" +
			"• `.swm [teks]` — diagonal tiled (default)\n" +
			"• `.swm -b [teks]` — bottom banner\n" +
			"• `.swm -c [teks]` — corner badge\n" +
			"• `.swm -o [teks]` — outline center\n" +
			"• `.swm -qr [teks/url]` — QR watermark (bisa di-scan!)\n\n" +
			"Contoh: `.swm -qr https://instagram.com/username`"
	}
	if len(wmText) > 60 {
		wmText = wmText[:60]
	}

	// Download: coba sticker dulu, lalu gambar
	var imgBuf []byte
	quoted := getQuotedMessage(msg)
	if quoted != nil {
		if buf, _, ok := downloadQuotedSticker(client, quoted); ok {
			imgBuf = buf
		} else if buf, _, ok := downloadQuotedImage(client, quoted); ok {
			imgBuf = buf
		}
	}
	// Coba media yang dilampirkan langsung di pesan
	if imgBuf == nil && msg.Message != nil {
		if im := msg.Message.GetImageMessage(); im != nil {
			if buf, err := downloadMedia(client, im); err == nil && len(buf) > 0 {
				imgBuf = buf
			}
		}
	}
	if imgBuf == nil {
		return "❌ Reply gambar/sticker dulu, atau kirim gambar dengan caption `.swm [teks]`."
	}

	// Decode (support jpeg/png/webp/gif)
	img, err := imaging.Decode(bytes.NewReader(imgBuf), imaging.AutoOrientation(true))
	if err != nil {
		return fmt.Sprintf("❌ Gagal decode gambar: %v", err)
	}
	// Resize ke 512 (standar sticker)
	img = imaging.Fit(img, 512, 512, imaging.Lanczos)
	base := imaging.Clone(img)

	// Terapkan watermark sesuai style
	switch style {
	case "qr":
		base, err = swmQR(base, wmText)
		if err != nil {
			return fmt.Sprintf("❌ %v", err)
		}
	default:
		f, err := swmLoadFont()
		if err != nil {
			return fmt.Sprintf("❌ %v", err)
		}
		switch style {
		case "bottom":
			base = swmBottomBanner(base, f, wmText)
		case "corner":
			base = swmCorner(base, f, wmText)
		case "outline":
			base = swmOutlineCenter(base, f, wmText)
		default: // diagonal
			base = swmDiagonal(base, f, wmText)
		}
	}

	// Encode ke WEBP
	var webpBuf bytes.Buffer
	if err := webp.Encode(&webpBuf, base, &webp.Options{Quality: 90}); err != nil {
		return fmt.Sprintf("❌ Gagal encode WEBP: %v", err)
	}

	// Kirim (EXIF pack name otomatis di-set oleh uploadSticker)
	if err := sendStickerMessageWithWM(client, msg.Info.Chat, webpBuf.Bytes(), msg, wmText); err != nil {
		if err2 := sendStickerMessageWithWM(client, msg.Info.Chat, webpBuf.Bytes(), nil, wmText); err2 != nil {
			return fmt.Sprintf("❌ Gagal kirim: %v", err2)
		}
	}
	styleName := map[string]string{
		"diagonal": "Diagonal Tiled", "bottom": "Bottom Banner",
		"corner": "Corner Badge", "outline": "Outline Center", "qr": "QR Code",
	}[style]
	return fmt.Sprintf("✅ SWM applied: \"%s\" (%s)", wmText, styleName)
}
