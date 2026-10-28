package main

// ads.go — .ads / .hy (album message dengan external ad reply)
// ════════════════════════════════════════════════════════════
// Kirim AlbumMessage dengan ExternalAdReply bergaya automated
// greeting message (iklan CTWA) ke grup. Owner-only, grup saja.
//
//   .ads <teks>            → thumbnail dari gambar yang di-reply,
//                            atau thumbnail default bila tidak ada.
// Reaksi ✅ bila terkirim, ❌ bila gagal (tanpa teks balasan).

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/proto/waCommon"
	"github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	"golang.org/x/image/draw"
	"google.golang.org/protobuf/proto"
)

// adsMaxBytes is the largest thumbnail the ad reply accepts.
const adsMaxBytes = 25 * 1024 * 1024

func adsReact(client *whatsmeow.Client, msg *events.Message, emoji string) {
	r := &waE2E.Message{
		ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(msg.Info.Chat.String()),
				FromMe:    proto.Bool(msg.Info.IsFromMe),
				ID:        proto.String(msg.Info.ID),
			},
			Text: proto.String(emoji),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = client.SendMessage(ctx, msg.Info.Chat, r)
}

// adsMakeThumb: cover-crop tengah ke 300x300 JPEG q80 (setara sharp).
// Fallback ala aslinya: decode gagal & buffer <100KB → pakai mentah.
func adsMakeThumb(buf []byte) []byte {
	if len(buf) == 0 {
		return nil
	}
	src, _, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		if len(buf) < 100_000 {
			return buf
		}
		return nil
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	side := w
	if h < side {
		side = h
	}
	x0 := b.Min.X + (w-side)/2
	y0 := b.Min.Y + (h-side)/2
	dst := image.NewRGBA(image.Rect(0, 0, 300, 300))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, image.Rect(x0, y0, x0+side, y0+side), draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return out.Bytes()
}

// adsDefaultThumb: gradient 300x300 sebagai pengganti src/media/Z3PH.png.
func adsDefaultThumb() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 300, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			t := float64(x+y) / 600.0
			img.Set(x, y, color.RGBA{
				R: uint8(30 + 60*t),
				G: uint8(20 + 30*t),
				B: uint8(80 + 100*t),
				A: 255,
			})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return out.Bytes()
}

func adsThumbSource(client *whatsmeow.Client, msg *events.Message) []byte {
	if quoted := getQuotedMessage(msg); quoted != nil {
		if buf, _, ok := downloadQuotedImage(client, quoted); ok && len(buf) > 0 && len(buf) <= adsMaxBytes {
			return buf
		}
	}
	return nil
}

func handleAds(client *whatsmeow.Client, msg *events.Message, args string) string {
	chat := msg.Info.Chat
	if !msg.Info.IsGroup {
		adsReact(client, msg, "❌")
		return ""
	}
	text := strings.TrimSpace(args)
	if text == "" {
		adsReact(client, msg, "❌")
		return ""
	}

	var thumb []byte
	if src := adsThumbSource(client, msg); len(src) > 0 {
		thumb = adsMakeThumb(src)
	}
	if len(thumb) == 0 {
		thumb = adsDefaultThumb()
	}
	if len(thumb) == 0 {
		adsReact(client, msg, "❌")
		return ""
	}

	payload := &waE2E.Message{
		AlbumMessage: &waE2E.AlbumMessage{
			ContextInfo: &waE2E.ContextInfo{
				ExternalAdReply: &waE2E.ContextInfo_ExternalAdReplyInfo{
					ContainsAutoReply:               proto.Bool(true),
					AutomatedGreetingMessageShown:   proto.Bool(true),
					GreetingMessageBody:             proto.String(text),
					AutomatedGreetingMessageCtaType: proto.String("OPEN_URL"),
					CtaPayload:                      proto.String("web"),
					SourceURL:                       proto.String(adsSourceURL()),
					Thumbnail:                       thumb,
					ShowAdAttribution:               proto.Bool(true),
					DisableNudge:                    proto.Bool(false),
					RenderLargerThumbnail:           proto.Bool(true),
					SourceApp:                       proto.String("facebook"),
					MediaURL:                        proto.String(adsSourceURL()),
					OriginalImageURL:                proto.String(adsSourceURL()),
					ThumbnailURL:                    proto.String(adsSourceURL()),
					AdType:                          waE2E.ContextInfo_ExternalAdReplyInfo_CTWA.Enum(),
					WtwaAdFormat:                    proto.Bool(false),
					WtwaWebsiteURL:                  proto.String(adsSourceURL()),
					AdPreviewURL:                    proto.String(adsSourceURL()),
				},
			},
		},
		MessageContextInfo: &waE2E.MessageContextInfo{
			DeviceListMetadata:        &waE2E.DeviceListMetadata{},
			DeviceListMetadataVersion: proto.Int32(2),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.SendMessage(ctx, chat, payload); err != nil {
		// Satu kali retry, sama seperti aslinya.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		if _, err2 := client.SendMessage(ctx2, chat, payload); err2 != nil {
			adsReact(client, msg, "❌")
			return ""
		}
	}
	adsReact(client, msg, "✅")
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "ads",
		Aliases:     []string{"hy"},
		Description: "kirim pesan greeting iklan (album + ad reply)",
		Category:    "Plugin",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAds(client, msg, args)
		},
	})
}
