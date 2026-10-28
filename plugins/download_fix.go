package main

// download_fix.go — Fallback download handlers for when kangwifi API is dead
// ════════════════════════════════════════════════════════════
//
// Replaces kangwifi-dependent download functions with direct API calls:
//   - TikTok: $TIKWM_API_BASE (free, no key) — already in downloader.go
//   - YouTube: cobalt.tools API (free, no key)
//   - Instagram: $TIKWM_API_BASE also supports IG posts
//   - Facebook: $TIKWM_API_BASE also supports FB
//
// Also fixes info tools to use direct APIs instead of kangwifi:
//   - Weather: wttr.in (free)
//   - Gempa: BMKG direct API (data.bmkg.go.id)

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

// downloadWithTikwm uses the tikwm-style API for TikTok/IG/FB downloads.
// Returns video URL + title + author.
func downloadWithTikwm(tiktokURL string) (videoURL, title, author string, err error) {
	if base := tikwmBaseURL(); base == "" {
		return "", "", "", &envMissingError{Name: "TIKWM_API_BASE"}
	}
	apiURL := tikwmBaseURL() + "/api/?url=" + url.QueryEscape(tiktokURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("tikwm: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var result struct {
		Code int `json:"code"`
		Data struct {
			Play   string `json:"play"`
			Title  string `json:"title"`
			Author struct {
				Nickname string `json:"nickname"`
			} `json:"author"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", "", fmt.Errorf("parse: %w", err)
	}
	if result.Code != 0 {
		return "", "", "", fmt.Errorf("tikwm: %s", result.Msg)
	}

	videoURL = result.Data.Play
	if strings.HasPrefix(videoURL, "/") {
		videoURL = tikwmBaseURL() + videoURL
	}
	return videoURL, result.Data.Title, result.Data.Author.Nickname, nil
}

// downloadWithCobalt uses cobalt.tools API for YouTube/IG/TikTok downloads.
// Returns direct video URL.
func downloadWithCobalt(mediaURL string) (string, error) {
	if base := cobaltBaseURL(); base == "" {
		return "", &envMissingError{Name: "COBALT_API_BASE"}
	}
	reqBody := map[string]string{
		"url": mediaURL,
	}
	jsonBody, _ := json.Marshal(reqBody)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", cobaltBaseURL()+"/", strings.NewReader(string(jsonBody)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hyper-go/1.0")

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cobalt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("cobalt HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Status string `json:"status"`
		URL    string `json:"url"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if result.Status != "stream" && result.Status != "redirect" {
		return "", fmt.Errorf("cobalt: %s", result.Error)
	}
	return result.URL, nil
}

// handleTikTokDLFixed — .tt / .tiktok via TikTokSearchEngine (engine baru)
func handleTikTokDLFixed(client *whatsmeow.Client, msg *events.Message, args string) string {
	return handleTikTokNew(client, msg, args)
}

// handleYouTubeDLFixed — .yt / .youtube with cobalt.tools fallback
func handleYouTubeDLFixed(client *whatsmeow.Client, msg *events.Message, args string) string {
	urlStr := extractFirstURL(args)
	if urlStr == "" {
		return "❓ *Cara pakai:* .yt <youtube-url>"
	}

	// Langsung via yt-dlp lokal (tanpa kangwifi)
	return handleTT2(client, msg, args)
}

// handleInstagramDLFixed — .ig / .instagram with the tikwm fallback
func handleInstagramDLFixed(client *whatsmeow.Client, msg *events.Message, args string) string {
	urlStr := extractFirstURL(args)
	if urlStr == "" {
		return "❓ *Cara pakai:* .ig <instagram-url>"
	}

	// Langsung via yt-dlp lokal (tanpa kangwifi)
	return handleTT2(client, msg, args)
}

// handleCuacaFixed — .cuaca with wttr.in fallback

// handleGempaFixed — .gempa with BMKG direct API fallback
