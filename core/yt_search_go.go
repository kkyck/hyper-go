package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// ytSearchResult holds a single YouTube search result
type ytSearchResult struct {
	VideoID  string
	Title    string
	Duration string
	Channel  string
}

var ytSearchVidRe = regexp.MustCompile(`"videoId":"([a-zA-Z0-9_-]{11})"`)
var ytSearchTitleRe = regexp.MustCompile(`"title":\{"runs":\[\{"text":"([^"\\]+)"`)

// ytSearchGo searches YouTube via search page HTML (pure Go, no yt-dlp)
func ytSearchGo(query string, maxResults int) ([]ytSearchResult, error) {
	if maxResults <= 0 || maxResults > 10 {
		maxResults = 5
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	searchURL := "https://www.youtube.com/results?search_query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := socks5HTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	html := string(body)

	// Extract video IDs (dedupe, keep order)
	seen := map[string]bool{}
	var ids []string
	for _, m := range ytSearchVidRe.FindAllStringSubmatch(html, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
			if len(ids) >= maxResults {
				break
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("tidak ada hasil untuk: %s", query)
	}

	// Get titles via oEmbed (lightweight)
	var results []ytSearchResult
	for _, vid := range ids {
		title := vid
		// Try oEmbed for title
		octx, ocancel := context.WithTimeout(context.Background(), 10*time.Second)
		oreq, _ := http.NewRequestWithContext(octx, "GET",
			"https://www.youtube.com/oembed?url=https://www.youtube.com/watch?v="+vid+"&format=json", nil)
		if oresp, err := socks5HTTPClient(10 * time.Second).Do(oreq); err == nil {
			obody, _ := io.ReadAll(io.LimitReader(oresp.Body, 64*1024))
			oresp.Body.Close()
			var oe struct {
				Title      string `json:"title"`
				AuthorName string `json:"author_name"`
			}
			if json.Unmarshal(obody, &oe) == nil && oe.Title != "" {
				title = oe.Title
				results = append(results, ytSearchResult{VideoID: vid, Title: title, Channel: oe.AuthorName})
				ocancel()
				continue
			}
		}
		ocancel()
		results = append(results, ytSearchResult{VideoID: vid, Title: title})
	}
	return results, nil
}

// ytmp3SearchYouTubeGo replaces ytmp3SearchYouTube (pure Go)
func ytmp3SearchYouTubeGo(query string) (string, string, string, error) {
	results, err := ytSearchGo(query, 1)
	if err != nil {
		return "", "", "", err
	}
	r := results[0]
	videoURL := "https://www.youtube.com/watch?v=" + r.VideoID
	return videoURL, r.Title, r.Duration, nil
}
