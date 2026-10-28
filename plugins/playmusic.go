package main

// playmusic.go — Play Music + Spotify Search & Download
//
// Port dari playmusic.ts + spotify.ts (Baileys) ke Go/whatsmeow.
// Uses kangwifi API: /search/spotify, /downloader/musicfab, /downloader/spotitrack
//
// Commands:
//   .play <judul lagu>           → cari + download mp3 (auto)
//   .spotify <judul lagu>        → cari 5 lagu pertama
//   .spotify download <n>        → download lagu urutan ke-n
//   .spotify dl <n>              → alias download
//   .spotify play <n>            → alias download
//
// Uses kangwifi API for all operations.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	playMaxDownloadSize = 50 * 1024 * 1024 // 50MB
	playSearchTimeout   = 15 * time.Second
	playDownloadTimeout = 120 * time.Second
	playCacheTTL        = 10 * time.Minute
)

// ─── Types ────────────────────────────────────────────────────────

type SpotifyTrack struct {
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	URL       string `json:"url"`
	Duration  string `json:"duration"`
	Thumbnail string `json:"thumbnail"`
}

type playSearchCacheEntry struct {
	Query   string
	Results []SpotifyTrack
	TS      time.Time
}

var (
	playCacheMu sync.Mutex
	playCache   = map[string]*playSearchCacheEntry{} // key = chat JID
)

// ─── Kangwifi API helpers ─────────────────────────────────────────

func kangGet(path string, params map[string]string) ([]byte, int, error) {
	if base := kangwifiBase(); base == "" {
		return nil, 0, &envMissingError{Name: "KANGWIFI_API_BASE"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), playSearchTimeout)
	defer cancel()
	urlStr := kangwifiBase() + path
	if len(params) > 0 {
		var parts []string
		for k, v := range params {
			parts = append(parts, k+"="+v)
		}
		urlStr += "?" + strings.Join(parts, "&")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

func kangPost(path string, body interface{}) ([]byte, int, error) {
	if base := kangwifiBase(); base == "" {
		return nil, 0, &envMissingError{Name: "KANGWIFI_API_BASE"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), playDownloadTimeout)
	defer cancel()
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", kangwifiBase()+path, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body2, _ := io.ReadAll(resp.Body)
	return body2, resp.StatusCode, nil
}

// ─── Spotify search ────────────────────────────────────────────────

func spotifySearch(query string, limit int) ([]SpotifyTrack, error) {
	if limit <= 0 {
		limit = 8
	}
	// Pure Go: search YouTube via Innertube (tanpa yt-dlp)
	results, err := ytSearchGo(query, limit)
	if err != nil {
		return nil, err
	}
	var tracks []SpotifyTrack
	for _, r := range results {
		tracks = append(tracks, SpotifyTrack{
			Title:    r.Title,
			URL:      "https://www.youtube.com/watch?v=" + r.VideoID,
			Duration: r.Duration,
		})
	}
	return tracks, nil
}
func extractDlUrl(payload map[string]interface{}) string {
	if payload == nil {
		return ""
	}
	candidates := []string{
		"downloadUrl", "download_url", "url", "link", "media", "audio",
	}
	for _, k := range candidates {
		if v, ok := payload[k].(string); ok && strings.HasPrefix(v, "http") {
			return v
		}
	}
	// nested result/data
	for _, k := range []string{"result", "data"} {
		if nested, ok := payload[k].(map[string]interface{}); ok {
			for _, k2 := range candidates {
				if v, ok := nested[k2].(string); ok && strings.HasPrefix(v, "http") {
					return v
				}
			}
		}
	}
	return ""
}

// downloadTrack tries multiple providers to download mp3 buffer.
// Pure Go: pakai ytmp3DownloadAudio (Innertube, tanpa yt-dlp)
func downloadTrack(trackURL string) ([]byte, string, error) {
	buf, title, _, err := ytmp3DownloadAudio(trackURL)
	return buf, title, err
}
func downloadFile(urlStr string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ─── Command handlers ─────────────────────────────────────────────

// handlePlayCommand — .play <judul> (auto search + download)
func handlePlayCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	query := strings.TrimSpace(args)
	if query == "" {
		return "❓ *Cara pakai:* .play <judul lagu>\nContoh: .play lofi hip hop"
	}

	// Search spotify
	tracks, err := spotifySearch(query, 1)
	if err != nil || len(tracks) == 0 {
		return fmt.Sprintf("❌ Gagal cari: %v", err)
	}

	track := tracks[0]
	// Download
	buf, provider, err := downloadTrack(track.URL)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download: %v", err)
	}

	// Send as audio
	safeName := sanitizeFilename(track.Title)
	if safeName == "" {
		safeName = "audio"
	}
	err = sendAudioMessage(client, msg.Info.Chat, buf, safeName+".mp3", msg)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim audio: %v", err)
	}

	fmt.Printf("[play] OK %s - %s (%d bytes, via %s)\n", track.Artist, track.Title, len(buf), provider)
	return ""
}

// handleSpotifySearchCommand — .spotify <judul> | .spotify download <n>
func handleSpotifySearchCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	input := strings.TrimSpace(args)
	chatID := msg.Info.Chat.String()

	if input == "" {
		return `🎵 *Spotify — Search & Download*

*Cara pakai:*
• .spotify <judul lagu> — cari 5 lagu pertama (contoh: .spotify introvert)
• .spotify download <n> — download lagu urutan ke-n (contoh: .spotify download 2)
• Alias: .spotify dl 2 / .spotify play 2

*Catatan:*
• Hasil pencarian disimpan 10 menit per chat.
• Format default: file mp3.`
	}

	lower := strings.ToLower(input)

	// .spotify download/dl/play <n>
	if strings.HasPrefix(lower, "download") || strings.HasPrefix(lower, "dl") || strings.HasPrefix(lower, "play") {
		parts := strings.Fields(input)
		if len(parts) < 2 {
			return "❓ *Cara pakai:* .spotify download <nomor>\nNomor sesuai urutan hasil pencarian terakhir."
		}
		var idx int
		fmt.Sscanf(parts[1], "%d", &idx)
		if idx < 1 {
			return "❌ Nomor tidak valid. Gunakan: `.spotify download 1`"
		}

		playCacheMu.Lock()
		cached := playCache[chatID]
		playCacheMu.Unlock()

		if cached == nil || len(cached.Results) == 0 {
			return "❌ Belum ada hasil pencarian. Cari dulu: .spotify <judul lagu>"
		}

		if time.Since(cached.TS) > playCacheTTL {
			return "❌ Hasil pencarian sudah kedaluwarsa (>10 menit). Cari ulang: .spotify <judul lagu>"
		}

		if idx > len(cached.Results) {
			return fmt.Sprintf("❌ Tidak ada lagu urutan ke-%d. Pilih 1 sampai %d.", idx, len(cached.Results))
		}

		track := cached.Results[idx-1]
		if track.URL == "" {
			return "❌ Track tidak punya URL Spotify."
		}

		// Download
		buf, provider, err := downloadTrack(track.URL)
		if err != nil {
			return fmt.Sprintf("❌ Gagal download lagu: %s", truncateStr(err.Error(), 280))
		}

		// Send as audio
		safeName := sanitizeFilename(track.Title)
		if safeName == "" {
			safeName = "audio"
		}
		err = sendAudioMessage(client, msg.Info.Chat, buf, safeName+".mp3", msg)
		if err != nil {
			return fmt.Sprintf("❌ Gagal kirim audio: %v", err)
		}

		fmt.Printf("[spotify] ✅ Download: %s - %s (%d bytes, via %s)\n", track.Artist, track.Title, len(buf), provider)
		return ""
	}

	// .spotify <judul> → search
	query := input
	tracks, err := spotifySearch(query, 8)
	if err != nil {
		return fmt.Sprintf("❌ Gagal mencari: %v", err)
	}
	if len(tracks) == 0 {
		return fmt.Sprintf("❌ Tidak ada hasil untuk: \"%s\"", query)
	}

	// Save to cache
	playCacheMu.Lock()
	playCache[chatID] = &playSearchCacheEntry{
		Query:   query,
		Results: tracks,
		TS:      time.Now(),
	}
	playCacheMu.Unlock()

	// Format output
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🎵 *Spotify Search* — \"%s\"\n\n", query))
	b.WriteString(fmt.Sprintf("Total: %d hasil\n\n", len(tracks)))
	for i, t := range tracks {
		b.WriteString(fmt.Sprintf("%d. *%s* — %s\n", i+1, t.Title, t.Artist))
		if t.Duration != "" {
			b.WriteString(fmt.Sprintf("   • Durasi: %s\n", t.Duration))
		}
		b.WriteString(fmt.Sprintf("   • %s\n", t.URL))
	}
	b.WriteString("\nKetik .spotify download <nomor> untuk download.")

	return b.String()
}

// ─── Helpers ─────────────────────────────────────────────────────

// sendAudioMessage uploads and sends an audio message.
func sendAudioMessage(client *whatsmeow.Client, chat types.JID, audioData []byte, filename string, quoted *events.Message) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	up, err := client.Upload(ctx, audioData, whatsmeow.MediaAudio)
	if err != nil {
		return err
	}

	audioMsg := &waE2E.AudioMessage{
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String("audio/mpeg"),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(uint64(len(audioData))),
	}
	if quoted != nil {
		audioMsg.ContextInfo = &waE2E.ContextInfo{
			StanzaID:    proto.String(quoted.Info.ID),
			Participant: proto.String(quoted.Info.Sender.String()),
		}
	}

	_, err = client.SendMessage(ctx, chat, &waE2E.Message{AudioMessage: audioMsg})
	return err
}

// sanitizeFilename removes special chars from a string for use as filename.
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == ' ' {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
