package main

// sticker_cmd.go — Handler .s/.sticker port dari plugin terbaru
// ════════════════════════════════════════════════════════════
//
// Mendukung: URL langsung, media terlampir, reply media.
// Format teks: ".s packname | author" untuk EXIF custom.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// handleStickerCommandV2 — implementasi baru via ffmpeg engine.
// Mengembalikan "" jika sticker terkirim, atau pesan error/teks bantuan.
func handleStickerCommandV2(client *whatsmeow.Client, msg *events.Message, args string) string {
	text := strings.TrimSpace(args)

	// ── Ambil media: URL, lampiran langsung, atau reply ──
	var data []byte
	var mime string
	var animated bool
	var isURL bool

	lowerText := strings.ToLower(text)
	if strings.HasPrefix(lowerText, "http://") || strings.HasPrefix(lowerText, "https://") {
		isURL = true
		url := strings.TrimSpace(strings.SplitN(text, "|", 2)[0])
		fmt.Printf("[sticker] Download: %s\n", url)
		httpClient := &http.Client{Timeout: 30 * time.Second}
		resp, err := httpClient.Get(url)
		if err != nil {
			return fmt.Sprintf("❌ Gagal ambil URL: %s", err.Error())
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Sprintf("❌ URL balasan %d", resp.StatusCode)
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, 25<<20))
		if err != nil {
			return fmt.Sprintf("❌ Gagal baca URL: %s", err.Error())
		}
		mime = resp.Header.Get("Content-Type")
		animated = StickerSourceAnimated(mime, url) || IsAnimatedWebP(data)
	} else {
		// Cek reply dulu, lalu pesan sendiri
		var buf []byte
		var mim string
		var ok bool
		var isAnim bool

		if quoted := getQuotedMessage(msg); quoted != nil {
			buf, mim, isAnim, ok = downloadMediaForSticker(client, quoted)
		}
		if !ok {
			buf, mim, isAnim, ok = downloadMediaForSticker(client, msg)
		}
		if !ok {
			// Tidak ada media — tampilkan bantuan jika tidak ada teks juga
			if text == "" {
				return "🖼️ *Sticker*\n\n" +
					"• `.s <teks>` — buat sticker dari teks\n" +
					"• `.s` — reply gambar/video → sticker\n" +
					"• `.s <url>` — dari URL gambar/video\n" +
					"• `.s pack | author` — custom EXIF"
			}
			// Ada teks tapi tidak ada media → sticker teks (fallback ke handler lama)
			return handleStickerTextFallback(client, msg, text)
		}
		data, mime, animated = buf, mim, isAnim
		_ = mime
	}

	if len(data) == 0 {
		return "❌ Stiker gagal: media kosong"
	}
	if len(data) > 50<<20 {
		return "❌ File terlalu besar (max 50MB)"
	}

	webp, err := ConvertStickerToWebP(data, animated)
	if err != nil {
		return fmt.Sprintf("❌ Stiker gagal: %s", err.Error())
	}
	if animated && !IsAnimatedWebP(webp) {
		return "❌ Stiker gagal: hasil WebP tidak punya frame animasi"
	}
	fmt.Printf("[sticker] WebP %d bytes | animated=%v\n", len(webp), animated)

	// EXIF packname | author dari teks
	packname := StickerEXIFPackname
	author := StickerEXIFAuthor
	if text != "" && !isURL {
		parts := strings.SplitN(text, "|", 2)
		if p := strings.TrimSpace(parts[0]); p != "" && !strings.HasPrefix(strings.ToLower(p), "http") {
			// Hanya pakai sebagai packname jika bukan perintah teks biasa yang panjang
			// (teks panjang = sticker teks, sudah di-handle di atas)
			if len(p) < 50 {
				packname = p
			}
		}
		if len(parts) > 1 {
			if a := strings.TrimSpace(parts[1]); a != "" {
				author = a
			}
		}
	}

	if err := SendStickerMediaV2(client, msg.Info.Chat, webp, animated, packname, author); err != nil {
		return fmt.Sprintf("❌ Gagal kirim sticker: %s", err.Error())
	}
	return ""
}

// downloadMediaForSticker mengunduh media dari pesan untuk dijadikan sticker.
// Mengembalikan (data, mime, animated, ok).
func downloadMediaForSticker(client *whatsmeow.Client, msg *events.Message) ([]byte, string, bool, bool) {
	if msg == nil || msg.Message == nil {
		return nil, "", false, false
	}
	m := msg.Message

	// Unwrap ephemeral/view-once
	for i := 0; i < 5; i++ {
		if em := m.GetEphemeralMessage(); em != nil && em.GetMessage() != nil {
			m = em.GetMessage()
			continue
		}
		if vo := m.GetViewOnceMessage(); vo != nil && vo.GetMessage() != nil {
			m = vo.GetMessage()
			continue
		}
		if vo2 := m.GetViewOnceMessageV2(); vo2 != nil && vo2.GetMessage() != nil {
			m = vo2.GetMessage()
			continue
		}
		break
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if st := m.GetStickerMessage(); st != nil {
		data, err := client.Download(ctx, st)
		if err != nil || len(data) == 0 {
			return nil, "", false, false
		}
		return data, st.GetMimetype(), st.GetIsAnimated() || st.GetIsLottie(), true
	}
	if im := m.GetImageMessage(); im != nil {
		data, err := client.Download(ctx, im)
		if err != nil || len(data) == 0 {
			return nil, "", false, false
		}
		return data, im.GetMimetype(), StickerSourceAnimated(im.GetMimetype(), "") || IsAnimatedWebP(data), true
	}
	if vm := m.GetVideoMessage(); vm != nil {
		data, err := client.Download(ctx, vm)
		if err != nil || len(data) == 0 {
			return nil, "", false, false
		}
		return data, vm.GetMimetype(), true, true
	}
	if dm := m.GetDocumentMessage(); dm != nil {
		mt := strings.ToLower(dm.GetMimetype())
		if strings.HasPrefix(mt, "image/") || strings.HasPrefix(mt, "video/") {
			data, err := client.Download(ctx, dm)
			if err != nil || len(data) == 0 {
				return nil, "", false, false
			}
			return data, mt, StickerSourceAnimated(mt, ""), true
		}
	}
	return nil, "", false, false
}

// handleStickerTextFallback — sticker dari teks via handler lama.
func handleStickerTextFallback(client *whatsmeow.Client, msg *events.Message, text string) string {
	stickerBuf, err := createStickerFromText(text)
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat sticker teks: %v", err)
	}
	if err := sendStickerMessage(client, msg.Info.Chat, stickerBuf, msg); err != nil {
		if err2 := sendStickerMessage(client, msg.Info.Chat, stickerBuf, nil); err2 != nil {
			return fmt.Sprintf("❌ Gagal kirim sticker: %v", err2)
		}
	}
	return ""
}
