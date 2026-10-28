package main

// tiktok_cmd.go — handler .tt via TikTokSearchEngine (plugins/tiktok_engine.go).
//
// .tt <tiktok-url> → enrichItem (mobile API → web SSR) → download video
//                   no-watermark → kirim ke chat. Postingan foto → kirim
//                   gambar (maks 5).
// .tt <kata-kunci> → Search via headless Chrome → daftar hasil + metadata.

import (
	"context"
	"fmt"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

func fmtCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func ttCaption(m *TikTokVideoMetadata) string {
	var b strings.Builder
	desc := strings.TrimSpace(m.Description)
	if len(desc) > 220 {
		desc = desc[:220] + "…"
	}
	if desc == "" {
		desc = "TikTok"
	}
	b.WriteString("🎵 *" + desc + "*")
	author := m.Author.Nickname
	if author == "" {
		author = m.Author.UniqueID
	}
	if author != "" {
		b.WriteString("\n👤 " + author)
		if m.Author.Verified {
			b.WriteString(" ✓")
		}
	}
	if m.Stats.Plays > 0 || m.Stats.Likes > 0 {
		fmt.Fprintf(&b, "\n▶ %s • ❤ %s • 💬 %s",
			fmtCount(m.Stats.Plays), fmtCount(m.Stats.Likes), fmtCount(m.Stats.Comments))
	}
	return b.String()
}

func ttDownloadAndSend(client *whatsmeow.Client, msg *events.Message, engine *TikTokSearchEngine, urlStr string) string {
	// 2026-10-18: pakai https://dl.kangwifi.eu.org/ (aiodl) untuk semua .tt download.
	// Engine tetap diterima untuk backward-compat signature, tapi TIDAK dipakai
	// di sini — search tetap pakai engine (lihat ttSearchReply).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	data, mime, _, title, err := dlKangwifiFull(ctx, urlStr, "best", 100*1024*1024)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download via dl.kangwifi: %v", err)
	}

	// Caption dari title yang di-extract aiodl (level=head), bila ada.
	caption := "🎵 TikTok"
	if title != "" {
		caption = "🎵 " + title
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()

	// Jika mime audio, kirim sebagai audio
	if strings.HasPrefix(mime, "audio/") {
		if serr := sprSendAudio(client, ctx2, msg.Info.Chat, data, mime, "tiktok.mp3"); serr != nil {
			return fmt.Sprintf("❌ Gagal kirim: %v", serr)
		}
		return ""
	}
	// Default: video
	if serr := sprSendVideo(client, ctx2, msg.Info.Chat, data, "video/mp4", caption); serr != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", serr)
	}
	return ""
}

func ttSearchReply(engine *TikTokSearchEngine, keyword string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	res := engine.Search(ctx, keyword, 5)
	if !res.Success {
		errMsg := "gagal"
		if res.Error != nil && res.Error.Message != "" {
			errMsg = res.Error.Message
		}
		return "❌ Pencarian gagal: " + errMsg
	}
	sr, ok := res.Data.(SearchResponse)
	if !ok || sr.Total == 0 {
		return "❌ Tidak ada hasil untuk *" + keyword + "*"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔍 *%d hasil:* %s", sr.Total, keyword)
	for i, v := range sr.Videos {
		desc := strings.TrimSpace(v.Description)
		if len(desc) > 90 {
			desc = desc[:90] + "…"
		}
		if desc == "" {
			desc = "(tanpa deskripsi)"
		}
		author := v.Author.Nickname
		if author == "" {
			author = v.Author.UniqueID
		}
		if author == "" {
			author = "-"
		}
		fmt.Fprintf(&b, "\n\n%d. %s\n👤 %s • ▶ %s • ❤ %s\n%s",
			i+1, desc, author, fmtCount(v.Stats.Plays), fmtCount(v.Stats.Likes), v.CanonicalURL)
	}
	return b.String()
}

// handleTikTokNew — entry point .tt / .tiktok.
func handleTikTokNew(client *whatsmeow.Client, msg *events.Message, args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return "❓ *Cara pakai:*\n.tt <tiktok-url>\n.tt <kata-kunci>"
	}

	engine, err := NewTikTokSearchEngine(60 * time.Second)
	if err != nil {
		return fmt.Sprintf("❌ Gagal init engine: %v", err)
	}

	if urlStr := extractFirstURL(args); urlStr != "" {
		return ttDownloadAndSend(client, msg, engine, urlStr)
	}
	return ttSearchReply(engine, trimmed)
}
