package main

// ytmp3_cmd.go — handler .ytmp3 via YTMusicEngine (plugins/ytmusic_engine.go).
//
// .ytmp3 <judul> → Search (YouTube Music InnerTube) → lagu pertama
// .ytmp3 <url>   → video ID langsung
//   → Download mp3 via savenow.to API → kirim sebagai audio.
//
// Tetap owner-only seperti sebelumnya.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// ytIsURL menentukan apakah argumen adalah URL YouTube.
func ytIsURL(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return true
	case strings.HasPrefix(lower, "youtu.be"), strings.HasPrefix(lower, "youtube.com"),
		strings.HasPrefix(lower, "www.youtube.com"), strings.HasPrefix(lower, "m.youtube.com"),
		strings.HasPrefix(lower, "music.youtube.com"):
		return true
	default:
		return false
	}
}

// ytParseDuration mengubah "m:ss" / "h:mm:ss" menjadi detik.
func ytParseDuration(d string) uint32 {
	d = strings.TrimSpace(d)
	if d == "" {
		return 0
	}
	parts := strings.Split(d, ":")
	var total uint64
	for _, p := range parts {
		n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return uint32(total)
}

// ytDownloadBytes mengunduh URL stream audio (batasi ukuran).
func ytDownloadBytes(engine *YTMusicEngine, ctx context.Context, urlStr string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	resp, err := engine.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

// handleYTMP3New — entry point .ytmp3 via YTMusicEngine.
func handleYTMP3New(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	// Parse flags: -c (call mode) + @tag (target) — direstore dari versi lama
	trimmed := strings.TrimSpace(args)
	callMode := false
	var targetMention string
	var queryParts []string
	for _, p := range strings.Fields(trimmed) {
		switch {
		case p == "-c", p == "-C":
			callMode = true
		case strings.HasPrefix(p, "@"):
			targetMention = p
		default:
			queryParts = append(queryParts, p)
		}
	}
	trimmed = strings.Join(queryParts, " ")

	if trimmed == "" && !callMode {
		return yts3Help()
	}
	if trimmed == "" && callMode {
		// .ytmp3 -c (tanpa query) → reply audio mode via .call
		callArgs := targetMention
		if callArgs == "" {
			callArgs = "self"
		}
		return handleCallSendCommand(client, msg, callArgs)
	}

	engine, err := NewYTMusicEngine(45 * time.Second)
	if err != nil {
		return fmt.Sprintf("❌ Gagal init engine: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	videoID := ""
	title := ""
	var durationSec uint32

	if ytIsURL(trimmed) {
		videoID = engine.extractVideoID(trimmed)
		if len(videoID) != 11 {
			return "❌ URL YouTube tidak valid"
		}
		title = videoID
	} else {
		sr, err := engine.Search(ctx, trimmed)
		if err != nil {
			return fmt.Sprintf("❌ Gagal search: %v", err)
		}
		if len(sr.Songs) == 0 {
			return "❌ Tidak ada hasil untuk: " + trimmed
		}
		top := sr.Songs[0]
		videoID = top.VideoID
		title = top.Title
		if top.Artist != "" {
			title = top.Title + " - " + top.Artist
		}
		durationSec = ytParseDuration(top.Duration)
	}

	dl, err := engine.Download(ctx, videoID, "mp3")
	if err != nil {
		return fmt.Sprintf("❌ Gagal konversi audio: %v", err)
	}
	if len(dl.Streams) == 0 {
		return "❌ Stream audio tidak tersedia"
	}
	streamURL := dl.Streams[0].DownloadURL
	for _, s := range dl.Streams {
		if s.Format == "mp3" && s.DownloadURL != "" {
			streamURL = s.DownloadURL
			break
		}
	}

	audioData, err := ytDownloadBytes(engine, ctx, streamURL, 30*1024*1024)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download audio: %v", err)
	}
	if len(audioData) == 0 {
		return "❌ Audio kosong"
	}

	// Branch: call mode (-c) → telepon + putar, bukan kirim file
	if callMode {
		target := resolveCallTarget(msg, targetMention)
		if target == "" {
			return "❌ Tidak bisa resolve target call"
		}
		return ytmp3CallAndPlay(client, target, audioData, title, durationSec)
	}

	return ytmp3SendAsDocument(client, msg, audioData, title, durationSec)
}

// resolveCallTarget menentukan target telepon: @tag > sender.
func resolveCallTarget(msg *events.Message, targetMention string) string {
	if targetMention != "" {
		mentions := extractMentionedJIDsDual(msg)
		if len(mentions) == 0 {
			return ""
		}
		j, err := parseAnyJID(mentions[0])
		if err != nil || j.IsEmpty() {
			return ""
		}
		return j.ToNonAD().String()
	}
	sender := msg.Info.Sender.ToNonAD()
	if sender.IsEmpty() {
		return ""
	}
	return sender.String()
}
