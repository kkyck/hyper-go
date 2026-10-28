package main

/**
 * ⬇️ kangwifi_dl.go — Downloader (PURE LOCAL via yt-dlp)
 * ════════════════════════════════════════════════════════════
 * Semua downloader langsung pakai yt-dlp lokal. Tanpa api.kangwifi.eu.org.
 * yt-dlp support: TikTok, YouTube, Instagram, Facebook, Threads,
 * Spotify, SoundCloud, Twitter/X, dan 1000+ situs lain.
 */

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// Semua handler delegasi ke handleTT2 (yt-dlp lokal)

func handleTikTokDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTT2(client, msg, args)
}

func handleYouTubeDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTT2(client, msg, args)
}

func handleInstagramDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTT2(client, msg, args)
}

func handleFacebookDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTT2(client, msg, args)
}

func handleThreadsDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTT2(client, msg, args)
}

// spotifyTrackInfo mengambil info track dari Spotify URL via oEmbed (pure Go, tanpa auth)
func spotifyTrackInfo(spotifyURL string) (title, thumbnail string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	oembedURL := spotifyEmbedBaseURL() + "/oembed?url=" + url.QueryEscape(spotifyURL)
	req, err := http.NewRequestWithContext(ctx, "GET", oembedURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := socks5HTTPClient(15 * time.Second).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", "", err
	}
	var oe struct {
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if err := json.Unmarshal(body, &oe); err != nil {
		return "", "", err
	}
	if oe.Title == "" {
		return "", "", fmt.Errorf("track tidak ditemukan")
	}
	return oe.Title, oe.ThumbnailURL, nil
}

func handleSpotifyDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return handleSpotifySearchCommand(client, msg, "")
	}
	// Kalau URL Spotify, ambil info track lalu download via YouTube
	if strings.Contains(args, "open.spotify.com") {
		title, _, err := spotifyTrackInfo(extractFirstURL(args))
		if err != nil {
			return "❌ Gagal ambil info Spotify: " + err.Error()
		}
		// Search YouTube dan download audio (pure Go)
		videoURL, _, _, err := ytmp3SearchYouTube(title)
		if err != nil {
			return "❌ Search YouTube gagal: " + err.Error()
		}
		buf, ytTitle, _, err := ytmp3DownloadAudio(videoURL)
		if err != nil {
			return "❌ Download gagal: " + err.Error()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		caption := "🎵 " + title + "\n📀 via YouTube: " + ytTitle
		if err := sprSendAudio(client, ctx, msg.Info.Chat, buf, "audio/mpeg", caption); err != nil {
			return "❌ Kirim gagal: " + err.Error()
		}
		return ""
	}
	// Bukan URL = search
	return handleSpotifySearchCommand(client, msg, args)
}

func handleSoundCloudDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTT2(client, msg, "audio "+args)
}

func handleGenericDL(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTT2(client, msg, args)
}
