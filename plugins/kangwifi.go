package main

/**
 * 🌐 kangwifi.go — HTTP client for the kangwifi-style media API
 * ════════════════════════════════════════════════════════════
 * Base URL: $KANGWIFI_API_BASE (no default)
 * Free API, no API key needed.
 *
 * Supports both GET (query) and POST (JSON body) endpoints.
 */

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"google.golang.org/protobuf/proto"
)

// kangwifiBase returns the configured media API base URL.
//
// KANGWIFI_API_BASE has no default: the integration stays disabled until
// the operator points it at their own deployment.
func kangwifiBase() string { return kangwifiBaseURL() }

// kwHTTPClient is the shared HTTP client with timeout.
var kwHTTPClient = &http.Client{Timeout: 60 * time.Second}

// kwGetJSON performs a GET request to the media API and decodes JSON into v.
// path should start with "/" (e.g. "/ai/gemini").
// query is optional query parameters.

// kwPostJSON performs a POST request with JSON body and decodes response JSON into v.
func kwPostJSON(path string, body any, v any) error {
	if base := kangwifiBase(); base == "" {
		return &envMissingError{Name: "KANGWIFI_API_BASE"}
	}
	u := kangwifiBase() + path
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "hyper-go/1.0")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := kwHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}
	return json.Unmarshal(respBody, v)
}

// kwGetRaw downloads raw bytes from a GET endpoint (used for media downloads).
func kwGetRaw(path string, query url.Values, maxBytes int) ([]byte, string, error) {
	if base := kangwifiBase(); base == "" {
		return nil, "", &envMissingError{Name: "KANGWIFI_API_BASE"}
	}
	if maxBytes <= 0 {
		maxBytes = 50 * 1024 * 1024 // 50MB default
	}
	u := kangwifiBase() + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "hyper-go/1.0")

	resp, err := kwHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	mime := resp.Header.Get("Content-Type")
	buf, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return nil, mime, err
	}
	return buf, mime, nil
}

// kwGetImageURL downloads an image from an API field that contains a URL
// (some Kangwifi endpoints return {"url": "..."} or {"result": "..."}).
// Returns the raw bytes of the image.
func kwGetImageURL(imageURL string, maxBytes int) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = 10 * 1024 * 1024 // 10MB
	}
	if imageURL == "" {
		return nil, "", fmt.Errorf("empty URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", imageURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "hyper-go/1.0")
	req.Header.Set("Referer", kangwifiBase()+"/")

	resp, err := kwHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return nil, mime, err
	}
	return buf, mime, nil
}

// truncateStr shortens a string for error messages.
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ─── Generic response shapes used across Kangwifi endpoints ───

// kwResult is a flexible response that matches most Kangwifi endpoints:
//
//	{ "status": true, "result": {...} or "..." }
//
// Some endpoints use "data" instead of "result"; we provide both.
type kwResult struct {
	Status  bool            `json:"status"`
	Code    int             `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Author  string          `json:"author,omitempty"`
}

// kwAIChatResponse is the standard response from /ai/* chat endpoints.
type kwAIChatResponse struct {
	Status bool   `json:"status"`
	Result string `json:"result"`
}

// kwAIChat sends a prompt to a /ai/<model> endpoint and returns the result text.

// kwAIDownloadable represents endpoints that return a media URL to be downloaded.
type kwDownloadableResponse struct {
	Status bool `json:"status"`
	Result struct {
		URL       string `json:"url"`
		Title     string `json:"title"`
		Author    string `json:"author"`
		Duration  string `json:"duration"`
		Source    string `json:"source"`
		Media     string `json:"media"`
		Image     string `json:"image"`
		Thumbnail string `json:"thumbnail"`
		Type      string `json:"type"`
	} `json:"result"`
}

// extractFirstURL finds the first http(s) URL in a string.
func extractFirstURL(s string) string {
	for i := 0; i < len(s); i++ {
		if strings.HasPrefix(strings.ToLower(s[i:]), "http://") ||
			strings.HasPrefix(strings.ToLower(s[i:]), "https://") {
			end := i
			for end < len(s) {
				c := s[end]
				if c == ' ' || c == '\n' || c == '\t' || c == '"' || c == '\'' || c == ']' || c == ')' || c == '}' || c == ',' {
					break
				}
				end++
			}
			return s[i:end]
		}
	}
	return ""
}

// sendImageFromURL downloads an image from a URL and sends it as a WhatsApp image message.
// (Langsung, tanpa media API)
func sendImageFromURL(client interface{}, chat interface{}, imageURL string, caption string) error {
	c, ok := client.(interface {
		Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
		SendMessage(ctx context.Context, chat types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error)
	})
	if !ok {
		return fmt.Errorf("client type tidak didukung")
	}
	jid, ok := chat.(types.JID)
	if !ok {
		return fmt.Errorf("chat type tidak didukung")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", imageURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}
	up, err := c.Upload(ctx, buf, whatsmeow.MediaImage)
	if err != nil {
		return err
	}
	out := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:           proto.String(up.URL),
			FileSHA256:    up.FileSHA256,
			FileEncSHA256: up.FileEncSHA256,
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			DirectPath:    proto.String(up.DirectPath),
			FileLength:    proto.Uint64(uint64(len(buf))),
			Caption:       proto.String(caption),
		},
	}
	_, err = c.SendMessage(context.Background(), jid, out)
	return err
}

// sendAudioFromURL downloads audio dari URL dan kirim sebagai pesan audio WhatsApp.
func sendAudioFromURL(client *whatsmeow.Client, chat types.JID, audioURL, mime string, asVoiceNote bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", audioURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024))
	if err != nil {
		return err
	}
	mediaType := whatsmeow.MediaAudio
	up, err := client.Upload(ctx, buf, mediaType)
	if err != nil {
		return err
	}
	out := &waE2E.Message{
		AudioMessage: &waE2E.AudioMessage{
			URL:           proto.String(up.URL),
			FileSHA256:    up.FileSHA256,
			FileEncSHA256: up.FileEncSHA256,
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			DirectPath:    proto.String(up.DirectPath),
			FileLength:    proto.Uint64(uint64(len(buf))),
			PTT:           proto.Bool(asVoiceNote),
		},
	}
	_, err = client.SendMessage(context.Background(), chat, out)
	return err
}

// sendVideoFromURL downloads video dari URL dan kirim sebagai pesan video WhatsApp.
func sendVideoFromURL(client *whatsmeow.Client, chat types.JID, videoURL, caption, mime string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", videoURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 100*1024*1024))
	if err != nil {
		return err
	}
	if mime == "" {
		mime = "video/mp4"
	}
	up, err := client.Upload(ctx, buf, whatsmeow.MediaVideo)
	if err != nil {
		return err
	}
	out := &waE2E.Message{
		VideoMessage: &waE2E.VideoMessage{
			URL:           proto.String(up.URL),
			FileSHA256:    up.FileSHA256,
			FileEncSHA256: up.FileEncSHA256,
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			DirectPath:    proto.String(up.DirectPath),
			FileLength:    proto.Uint64(uint64(len(buf))),
			Caption:       proto.String(caption),
		},
	}
	_, err = client.SendMessage(context.Background(), chat, out)
	return err
}
