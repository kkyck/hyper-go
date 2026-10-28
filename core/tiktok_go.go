package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// tiktokInfo holds extracted TikTok video info (pure Go)
type tiktokInfo struct {
	Title     string
	Author    string
	VideoURL  string // no-watermark preferred
	MusicURL  string
	Thumbnail string
}

var tiktokRehydrationRe = regexp.MustCompile(`<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__" type="application/json">(.*?)</script>`)

// tiktokExtractGo fetches a TikTok URL and extracts direct video URL (pure Go, no yt-dlp)
func tiktokExtractGo(pageURL string) (*tiktokInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Resolve short links (vm.tiktok.com, vt.tiktok.com)
	resolved := pageURL
	if strings.Contains(pageURL, "vm.tiktok.com") || strings.Contains(pageURL, "vt.tiktok.com") {
		req, _ := http.NewRequestWithContext(ctx, "HEAD", pageURL, nil)
		req.Header.Set("User-Agent", tt2DesktopUA)
		resp, err := socks5HTTPClient(30 * time.Second).Do(req)
		if err == nil {
			resp.Body.Close()
			resolved = resp.Request.URL.String()
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", resolved, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", tt2DesktopUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := socks5HTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, err
	}
	html := string(body)

	m := tiktokRehydrationRe.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("data video tidak ditemukan (TikTok memblokir/batasi)")
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(m[1]), &data); err != nil {
		return nil, fmt.Errorf("parse gagal: %w", err)
	}

	info := &tiktokInfo{}
	// Navigate: __DEFAULT_SCOPE__.webapp.video-detail.itemInfo.itemStruct
	var findItem func(v interface{}) map[string]interface{}
	findItem = func(v interface{}) map[string]interface{} {
		switch t := v.(type) {
		case map[string]interface{}:
			if _, ok := t["id"]; ok {
				if _, ok2 := t["video"]; ok2 {
					if _, ok3 := t["author"]; ok3 {
						return t
					}
				}
			}
			for _, val := range t {
				if r := findItem(val); r != nil {
					return r
				}
			}
		case []interface{}:
			for _, item := range t {
				if r := findItem(item); r != nil {
					return r
				}
			}
		}
		return nil
	}
	item := findItem(data)
	if item == nil {
		return nil, fmt.Errorf("item video tidak ditemukan")
	}

	if d, ok := item["desc"].(string); ok {
		info.Title = d
	}
	if author, ok := item["author"].(map[string]interface{}); ok {
		if nick, ok := author["nickname"].(string); ok {
			info.Author = nick
		}
	}
	if video, ok := item["video"].(map[string]interface{}); ok {
		// Prefer downloadAddr (no watermark), fallback playAddr
		if dl, ok := video["downloadAddr"].(string); ok && dl != "" {
			info.VideoURL = dl
		}
		if info.VideoURL == "" {
			if pl, ok := video["playAddr"].(string); ok {
				info.VideoURL = pl
			}
		}
		if cov, ok := video["cover"].(string); ok {
			info.Thumbnail = cov
		}
	}
	if music, ok := item["music"].(map[string]interface{}); ok {
		if pl, ok := music["playUrl"].(string); ok {
			info.MusicURL = pl
		}
	}
	if info.VideoURL == "" {
		return nil, fmt.Errorf("URL video tidak ditemukan")
	}
	return info, nil
}

// tiktokDownloadGo downloads bytes from a URL with TikTok-friendly headers (pure Go)
func tiktokDownloadGo(urlStr string, maxBytes int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", tt2DesktopUA)
	req.Header.Set("Referer", "https://www.tiktok.com/")
	resp, err := socks5HTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}
