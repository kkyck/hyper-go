package main

// tt2.go — .tt2 <url> via https://dl.kangwifi.eu.org/ (aiodl).
//
// .tt2 <url>       → video (kualitas best)
// .tt2 audio <url> → audio MP3
//
// 2026-10-18: Mengganti seluruh pipeline TikTok native extractor
// (tiktokExtractGo + tiktokDownloadGo) dengan satu panggilan API
// ke dl.kangwifi.eu.org. API aiodl mendukung TikTok/YouTube/Facebook
// tanpa signing kompleks / Chrome headless.

import (
	"context"
	"fmt"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// tt2DesktopUA tidak dipakai lagi (dl.kangwifi menangani UA/signing).
// Dibiarkan untuk backward-compat reference bila ada plugin lain baca.
const tt2DesktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

type tt2YtDlpFormat struct {
	URL      string `json:"url"`
	FormatID string `json:"format_id"`
	Ext      string `json:"ext"`
}

type tt2YtDlpInfo struct {
	Title    string           `json:"title"`
	Uploader string           `json:"uploader"`
	Formats  []tt2YtDlpFormat `json:"formats"`
}

func handleTT2(client *whatsmeow.Client, msg *events.Message, args string) string {
	isAudio := false
	trimmed := strings.TrimSpace(strings.ToLower(args))
	if strings.HasPrefix(trimmed, "audio ") {
		isAudio = true
	}

	urlStr := extractFirstURL(args)
	if urlStr == "" {
		return "❓ *Cara pakai:* .tt2 <tiktok-url>\nAtau: .tt2 audio <tiktok-url>"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	quality := "best"

	data, _, _, title, err := dlKangwifiFull(ctx, urlStr, quality, 100*1024*1024)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download via dl.kangwifi: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()

	// Caption dari title yang di-extract aiodl (level=head), bila ada.
	caption := "🎵 TikTok"
	if title != "" {
		caption = "🎵 " + title
	}

	var sendErr error
	if isAudio {
		sendErr = sprSendAudio(client, ctx2, msg.Info.Chat, data, "audio/mpeg", "tiktok.mp3")
	} else {
		sendErr = sprSendVideo(client, ctx2, msg.Info.Chat, data, "video/mp4", caption)
	}
	if sendErr != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", sendErr)
	}
	return ""

}

func init() {
	RegisterPlugin(Plugin{
		Command:     "tt2",
		Description: "tt2",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTT2(client, msg, args)
		},
	})
}
