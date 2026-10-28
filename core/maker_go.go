package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	makerFontBold *opentype.Font
	makerFontReg  *opentype.Font
)

func initMakerFonts() error {
	if makerFontBold != nil {
		return nil
	}
	boldData, err := os.ReadFile("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf")
	if err != nil {
		return err
	}
	makerFontBold, err = opentype.Parse(boldData)
	if err != nil {
		return err
	}
	regData, err := os.ReadFile("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf")
	if err != nil {
		return err
	}
	makerFontReg, err = opentype.Parse(regData)
	return err
}

func drawText(img *image.RGBA, text string, x, y int, size float64, col color.Color, bold bool) {
	f := makerFontReg
	if bold {
		f = makerFontBold
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72})
	if err != nil {
		return
	}
	defer face.Close()

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	d.DrawString(text)
}

func textWidth(text string, size float64, bold bool) int {
	f := makerFontReg
	if bold {
		f = makerFontBold
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72})
	if err != nil {
		return len(text) * int(size/2)
	}
	defer face.Close()
	d := &font.Drawer{Face: face}
	return d.MeasureString(text).Ceil()
}

func makerWrapText(text string, maxWidth int, size float64, bold bool) []string {
	var lines []string
	var cur string
	for _, w := range strings.Fields(text) {
		t := strings.TrimSpace(cur + " " + w)
		if textWidth(t, size, bold) > maxWidth && cur != "" {
			lines = append(lines, cur)
			cur = w
		} else {
			cur = t
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// makeBratGo generates brat-style image (lime green bg, black text) - pure Go
func makeBratGo(text, outPath string) error {
	if err := initMakerFonts(); err != nil {
		return err
	}
	W, H := 512, 512
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	// Lime green background (#8ACE00)
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{0x8A, 0xCE, 0x00, 0xFF}}, image.Point{}, draw.Src)

	lines := makerWrapText(text, W-60, 48, true)
	y := (H-len(lines)*60)/2 + 40
	for _, line := range lines {
		w := textWidth(line, 48, true)
		x := (W - w) / 2
		drawText(img, line, x, y, 48, color.Black, true)
		y += 60
	}

	// Simple blur effect: draw slightly offset copies with low opacity
	// (skip complex blur for performance, brat style is already recognizable)

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// makeQcGo generates quote image - pure Go
func makeQcGo(name, text, outPath string) error {
	if err := initMakerFonts(); err != nil {
		return err
	}
	W, H := 512, 320
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	// Dark background
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{0x1a, 0x1a, 0x2e, 0xFF}}, image.Point{}, draw.Src)

	// Avatar circle (simple)
	ax, ay, ar := 60, 60, 40
	for dy := -ar; dy <= ar; dy++ {
		for dx := -ar; dx <= ar; dx++ {
			if dx*dx+dy*dy <= ar*ar {
				img.Set(ax+dx, ay+dy, color.RGBA{0x4a, 0x4a, 0x6a, 0xFF})
			}
		}
	}
	// Initial letter
	if len(name) > 0 {
		drawText(img, strings.ToUpper(string([]rune(name)[0])), ax-12, ay+10, 28, color.White, true)
	}

	// Name
	drawText(img, name, ax+ar+15, ay+10, 24, color.RGBA{0x00, 0xd4, 0xff, 0xFF}, true)

	// Text
	lines := makerWrapText(text, W-60, 22, false)
	y := ay + ar + 40
	for i, line := range lines {
		if i >= 8 {
			break
		}
		drawText(img, line, 30, y, 22, color.White, false)
		y += 32
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
