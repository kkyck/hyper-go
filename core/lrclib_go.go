package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// lrclibSearchGo searches LRCLIB for lyrics (pure Go, no Python)
func lrclibSearchGo(query string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// LRCLIB search API
	searchURL := lrclibBaseURL() + "/api/search?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "bot-wa/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", err
	}

	var results []struct {
		TrackName    string `json:"trackName"`
		ArtistName   string `json:"artistName"`
		PlainLyrics  string `json:"plainLyrics"`
		SyncedLyrics string `json:"syncedLyrics"`
	}
	if err := json.Unmarshal(body, &results); err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "", fmt.Errorf("lirik tidak ditemukan untuk: %s", query)
	}

	// Ambil yang pertama, prefer synced tapi bersihkan timestamp
	lyrics := results[0].PlainLyrics
	if lyrics == "" && results[0].SyncedLyrics != "" {
		// Bersihkan timestamp dari synced lyrics
		var lines []string
		for _, ln := range strings.Split(results[0].SyncedLyrics, "\n") {
			ln = strings.TrimSpace(ln)
			if i := strings.LastIndex(ln, "]"); i >= 0 {
				ln = strings.TrimSpace(ln[i+1:])
			}
			if ln != "" {
				lines = append(lines, ln)
			}
		}
		lyrics = strings.Join(lines, "\n")
	}
	if lyrics == "" {
		return "", fmt.Errorf("lirik kosong untuk: %s", query)
	}
	return lyrics, nil
}
