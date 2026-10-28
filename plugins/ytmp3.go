package main

// ytmp3.go — .ytmp3 command: download YouTube audio + kirim sebagai file
// ════════════════════════════════════════════════════════════
//
// USAGE:
//   .ytmp3 <youtube-url>           → download audio MP3 + kirim sebagai document
//   .ytmp3 <kata kunci>             → cari di YouTube + download + kirim
//
// Audio format: MP3 (dari y2meta API audio format)
//
// Examples:
//   .ytmp3 https://youtu.be/dQw4w9WgXcQ
//     → Bot download MP3 + kirim sebagai file document
//
//   .ytmp3 https://youtu.be/dQw4w9WgXcQ -c
//     → Bot download MP3 + telepon Anda + putar lagu saat Anda angkat
//
//   .ytmp3 https://youtu.be/dQw4w9WgXcQ -c @kangwifi
//     → Bot download MP3 + telepon kangwifi + putar lagu
//
//   .ytmp3 -c  (reply voice note)
//     → Bot telepon Anda + putar voice note yang di-reply (tanpa download YouTube)

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	meowcaller "github.com/purpshell/meowcaller"
	"google.golang.org/protobuf/proto"
)

// ─── .ytmp3 command ───────────────────────────────────────────────

func handleYTMP3Command(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	// Parse args: url-or-query (flag -c/call mode sudah dihapus)
	args = strings.TrimSpace(args)
	urlOrQuery := args

	// Validate: must have URL/query
	if urlOrQuery == "" {
		return yts3Help()
	}

	// Determine if input is URL or search query
	ytURL := ""
	isSearch := false
	lower := strings.ToLower(urlOrQuery)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		ytURL = urlOrQuery
	case strings.HasPrefix(lower, "youtu.be"), strings.HasPrefix(lower, "youtube.com"),
		strings.HasPrefix(lower, "www.youtube.com"), strings.HasPrefix(lower, "m.youtube.com"):
		ytURL = "https://" + urlOrQuery
	case strings.Contains(lower, "watch?v="), strings.Contains(lower, "watch?"):
		// Already a URL with watch param but no scheme
		ytURL = "https://" + urlOrQuery
	default:
		// Not a URL → treat as search query
		isSearch = true
	}

	// If search mode: pakai yt-dlp search langsung
	if isSearch {
		fmt.Printf("[YTMP3] Search mode: %q\n", urlOrQuery)
		searchURL, searchTitle, searchDuration, err := ytmp3SearchYouTube(urlOrQuery)
		if err != nil {
			return fmt.Sprintf("❌ Gagal search YouTube: %v", err)
		}
		if searchURL == "" {
			return "❌ Tidak ada hasil untuk: " + urlOrQuery
		}
		fmt.Printf("[YTMP3] Search found: %q → %s\n", searchTitle, searchURL)
		ytURL = searchURL
		// title/duration will be re-fetched from y2meta in download step
		// (we could skip y2meta by passing these, but y2meta also gives us audio URL)
		_ = searchTitle
		_ = searchDuration
	}

	// Download audio from YouTube (URL mode)
	fmt.Printf("[YTMP3] Downloading audio from: %s\n", ytURL)
	audioData, title, duration, err := ytmp3DownloadAudio(ytURL)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download YouTube audio: %v", err)
	}
	if len(audioData) == 0 {
		return "❌ Audio kosong dari YouTube API"
	}
	fmt.Printf("[YTMP3] Got audio: %d bytes, title=%q, duration=%ds\n", len(audioData), title, duration)

	// Send audio as document
	return ytmp3SendAsDocument(client, msg, audioData, title, duration)
}

// ytmp3SearchYouTube searches YouTube via yt-dlp langsung (tanpa kangwifi).
// Returns (videoURL, title, duration string, error).
func ytmp3SearchYouTube(query string) (string, string, string, error) {
	// Pure Go via Innertube (tanpa yt-dlp)
	return ytmp3SearchYouTubeGo(query)
}

// ytmp3ValidateDownloadURL memvalidasi URL audio dari respons API sebelum di-GET.
// Mencegah SSRF: hanya http/https, tanpa userinfo, dan host tidak boleh
// resolve ke IP privat/loopback/link-local/reserved/metadata (mis. 169.254.169.254).
func ytmp3ValidateDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL tidak valid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("skema URL ditolak: %q (hanya http/https)", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("URL dengan userinfo ditolak")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL tanpa host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		lower == "metadata.google.internal" {
		return fmt.Errorf("host ditolak: %q", host)
	}

	isBlockedIP := func(ip net.IP) bool {
		if ip == nil {
			return true
		}
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("IP literal ditolak: %q", host)
		}
		return nil
	}

	// Hostname: resolve dan pastikan tidak ada IP hasil resolve yang diblokir
	// (mencegah DNS rebinding ke jaringan internal).
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("gagal resolve host %q: %v", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("host %q resolve ke IP internal/diblokir", host)
		}
	}
	return nil
}

func ytmp3DownloadValidated(audioURL, quality string) (dlResp *http.Response, err error) {
	fmt.Printf("[YTMP3] Audio URL found (quality=%s): %s\n", quality, audioURL[:min(80, len(audioURL))])

	// Validasi SSRF sebelum GET ke URL dari respons API
	if err := ytmp3ValidateDownloadURL(audioURL); err != nil {
		return nil, fmt.Errorf("URL audio ditolak (%v)", err)
	}

	// Download the actual audio file
	return httpClient.Get(audioURL)
}

// ytmp3DownloadAudio downloads MP3 audio dari YouTube via yt-dlp langsung.
// Returns (audio bytes, title, duration seconds, error).
// extractYouTubeID extracts video ID from YouTube URL (pure Go)
func extractYouTubeID(ytURL string) string {
	// youtu.be/ID
	if strings.Contains(ytURL, "youtu.be/") {
		parts := strings.Split(ytURL, "youtu.be/")
		if len(parts) > 1 {
			id := strings.Split(parts[1], "?")[0]
			id = strings.Split(id, "&")[0]
			id = strings.Split(id, "/")[0]
			if len(id) == 11 {
				return id
			}
		}
	}
	// youtube.com/watch?v=ID
	if strings.Contains(ytURL, "v=") {
		parts := strings.Split(ytURL, "v=")
		if len(parts) > 1 {
			id := strings.Split(parts[1], "&")[0]
			if len(id) == 11 {
				return id
			}
		}
	}
	// youtube.com/shorts/ID atau /embed/ID
	for _, prefix := range []string{"/shorts/", "/embed/", "/v/"} {
		if strings.Contains(ytURL, prefix) {
			parts := strings.Split(ytURL, prefix)
			if len(parts) > 1 {
				id := strings.Split(parts[1], "?")[0]
				id = strings.Split(id, "/")[0]
				if len(id) == 11 {
					return id
				}
			}
		}
	}
	return ""
}

func ytmp3DownloadAudio(ytURL string) ([]byte, string, uint32, error) {
	// Pure Go: pakai yt2 Innertube untuk dapat audio URL, download via HTTP
	videoID := extractYouTubeID(ytURL)
	if videoID == "" {
		return nil, "", 0, fmt.Errorf("video ID tidak valid")
	}

	playerResp, _, err := yt2RaceEngines(videoID)
	if err != nil {
		return nil, "", 0, fmt.Errorf("innertube: %w", err)
	}

	// Cari audio stream (itag 140 = m4a 128k)
	var audioURL, title string
	title = playerResp.VideoDetails.Title
	for _, f := range playerResp.StreamingData.AdaptiveFormats {
		if strings.HasPrefix(f.MimeType, "audio/mp4") && f.URL != "" {
			audioURL = f.URL
			break
		}
	}
	if audioURL == "" {
		return nil, "", 0, fmt.Errorf("audio stream tidak ditemukan")
	}

	// Download via Go HTTP
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", audioURL, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := socks5HTTPClient(120 * time.Second).Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024))
	if err != nil {
		return nil, "", 0, err
	}
	return buf, title, 0, nil
}

// ytmp3SendAsDocument sends audio bytes as a WhatsApp document message.
func ytmp3SendAsDocument(client *whatsmeow.Client, msg *events.Message, audioData []byte, title string, duration uint32) string {
	// Upload to WhatsApp servers
	upCtx, upCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer upCancel()

	up, err := client.Upload(upCtx, audioData, whatsmeow.MediaAudio)
	if err != nil {
		return fmt.Sprintf("❌ Upload gagal: %v", err)
	}

	// Filename
	filename := strings.TrimSpace(title)
	if filename == "" {
		filename = "audio"
	}
	// Sanitize filename — remove illegal chars
	filename = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, filename)
	if !strings.HasSuffix(strings.ToLower(filename), ".mp3") {
		filename += ".mp3"
	}

	// Build document message
	docMsg := &waE2E.Message{
		AudioMessage: &waE2E.AudioMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String("audio/mpeg"),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(audioData))),
			Seconds:       proto.Uint32(duration),
			PTT:           proto.Bool(false), // not voice note — regular audio
		},
	}
	_ = filename // WhatsApp doesn't use filename for audio messages

	// Send to same chat
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()

	_, err = client.SendMessage(sendCtx, msg.Info.Chat, docMsg)
	if err != nil {
		return fmt.Sprintf("❌ Kirim audio gagal: %v", err)
	}

	// Silent on success
	return ""
}

// yts3Help returns the .ytmp3 help text.
func yts3Help() string {
	return `🎵 *.ytmp3 — YouTube Audio*

❓ *Cara pakai:*
• .ytmp3 <url> — download MP3 dari URL, kirim sebagai file
• .ytmp3 <judul> — search YouTube, kirim MP3 pertama

Contoh:
• .ytmp3 https://youtu.be/dQw4w9WgXcQ
• .ytmp3 teh hijau

Format: MP3 (128kbps dari YouTube)`
}

// urlEscape and min are defined in lacak.go and playmusic.go respectively.
// We reuse them via package-level scope.

func init() {
	RegisterPlugin(Plugin{
		Command:     "ytmp3",
		Description: "ytmp3",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleYTMP3New(client, msg, args)
		},
	})
}
func ytmp3CallAndPlay(client *whatsmeow.Client, target string, audioData []byte, title string, duration uint32) string {
	// Check meowcaller ready
	meowcallerMu.Lock()
	mc := meowcallerClient
	ready := meowcallerReady
	meowcallerMu.Unlock()
	if !ready || mc == nil {
		return "❌ Meowcaller belum siap. Cek: .callmeow status"
	}

	// Guard UDP: media call WA wajib UDP — tolak langsung biar tidak nyangkut.
	if !callCheckUDP() {
		return callUDPBlockMsg
	}

	// Save audio to temp file (meowcaller needs file path for MP3File)
	tmpFile, err := os.CreateTemp("", "wabot-ytmp3-*.mp3")
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(audioData); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Sprintf("❌ Gagal tulis temp file: %v", err)
	}
	tmpFile.Close()

	fmt.Printf("[YTMP3] Audio saved: %s (%d bytes)\n", tmpPath, len(audioData))

	// Open as meowcaller MP3 source
	src, err := meowcaller.MP3File(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Sprintf("❌ Gagal decode MP3: %v", err)
	}

	// Place the call
	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	fmt.Printf("[YTMP3] Placing call to %s (title=%q)...\n", target, title)

	var call *meowcaller.Call
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("panic during call: %v", r)
				fmt.Printf("[YTMP3] PANIC: %v\n", r)
			}
		}()
		call, callErr = mc.Call(callCtx, target)
	}()
	if callErr != nil {
		callCancel()
		_ = src.Close()
		_ = os.Remove(tmpPath)
		return fmt.Sprintf("❌ Gagal place call: %v", callErr)
	}
	if call == nil {
		callCancel()
		_ = src.Close()
		_ = os.Remove(tmpPath)
		return "❌ meowcaller.Call returned nil call"
	}

	callID := call.ID()
	if callID == "" {
		callID = fmt.Sprintf("ytmp3-%d", time.Now().UnixNano())
	}

	// Register in active calls
	activeCallsMu.Lock()
	activeCalls[callID] = call
	activeCallsMu.Unlock()

	// Cleanup on call end
	call.OnEnd(func(reason string) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[YTMP3] PANIC in OnEnd: %v\n", r)
			}
		}()
		fmt.Printf("[YTMP3] Call %s ended: %s — cleaning up\n", callID, reason)
		if src != nil {
			_ = src.Close()
		}
		callCancel()
		_ = os.Remove(tmpPath)
		activeCallsMu.Lock()
		delete(activeCalls, callID)
		activeCallsMu.Unlock()
	})

	// Stream audio when ready
	call.OnReady(func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[YTMP3] PANIC in OnReady/Play: %v\n", r)
			}
		}()
		fmt.Printf("[YTMP3] Media ready, streaming audio (call %s, title=%q)...\n", callID, title)
		player := call.Play(src)
		if player != nil {
			player.OnFinish(func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("[YTMP3] PANIC in OnFinish: %v\n", r)
					}
				}()
				fmt.Printf("[YTMP3] Audio finished for call %s — hanging up\n", callID)
				_ = call.Hangup()
			})
		}
	})

	// Silent on success
	return ""
}
