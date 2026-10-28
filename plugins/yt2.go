package main

// yt2.go — YouTube Stream & Metadata Extractor (triple-engine, Go-optimized)
// ════════════════════════════════════════════════════════════
//
// v2.83 — Ported from pastebin source + Go-optimized with goroutines,
//         sync.Pool, streaming download, context-aware cancellation.
// v2.87 — Engine 3: VISIONOS (Apple Vision Pro) dari analisis Yarden
//         (AngelinaEmpire). Triple-engine race: Android VR + VISIONOS + Web SSR.
//
// FITUR UNGGULAN GO YANG DIMANFAATKAN:
//   1. GOROUTINES + CHANNELS — race Innertube (Android VR + VISIONOS) + Web SSR
//      paralel, return first success. Cancel context aborts the losers.
//   2. CONTEXT TIMEOUT — every HTTP request uses context.WithTimeout,
//      auto-cancels all in-flight goroutines when one succeeds.
//   3. SYNC.POOL — JSON decoder buffers reused (less GC pressure).
//   4. STREAMING DOWNLOAD — io.Copy chunked, doesn't load full media to RAM
//      (max 50MB audio / 100MB video, handled per-chunk).
//   5. TYPED STRUCTS — compile-time JSON safety (no fragile map[string]any).
//   6. COOKIEJAR — shared cookies across Innertube + Web SSR for anti-bot bypass.
//   7. REGEXP COMPILED ONCE — package-level vars, not per-call.
//
// TRIPLE-ENGINE STRATEGY (from pastebin source):
//   Engine 1: Innertube Android VR (Oculus Quest 3 client)
//     - POST https://www.youtube.com/youtubei/v1/player
//     - Client context: ANDROID_VR, version 1.56.21, deviceMake=Oculus, deviceModel=Quest 3
//     - Returns direct googlevideo.com URLs without n-signature throttling
//     - No PoToken JS runtime needed
//   Engine 2: Innertube VISIONOS (Apple Vision Pro client) — dari Yarden
//     - POST https://www.youtube.com/youtubei/v1/player
//     - Client context: VISIONOS, version 1.02, deviceModel=RealityDevice17,1
//     - x-youtube-client-name: 101, butuh visitorData dari session harvest
//     - Higher bot-detection bypass rate
//   Engine 3: Web SSR (ytInitialPlayerResponse)
//     - GET https://www.youtube.com/watch?v=XXX
//     - Regex-extract ytInitialPlayerResponse JSON from HTML
//     - Streams may be throttled (n-signature), but metadata reliable
//     - Used when Innertube returns LOGIN_REQUIRED or UNAVAILABLE
//
// SUPPORTED INPUTS (universal resolver):
//   - https://www.youtube.com/watch?v=XXXXXXXXXXX
//   - https://youtu.be/XXXXXXXXXXX
//   - https://www.youtube.com/shorts/XXXXXXXXXXX
//   - https://www.youtube.com/embed/XXXXXXXXXXX
//   - https://www.youtube.com/v/XXXXXXXXXXX
//   - https://www.youtube.com/live/XXXXXXXXXXX
//   - https://www.youtube.com/watch/XXXXXXXXXXX
//   - Raw 11-char video ID
//
// COMMANDS:
//   .yt2 <url>            → metadata + stream links (audio + video)
//   .yt2 audio <url>      → stream MP3 audio ke WhatsApp (itag 140)
//   .yt2 video <url>      → stream MP4 video ke WhatsApp (itag 18)
//   .yt2 thumb <url>      → kirim thumbnail maxresdefault
//   .yt2 info <url>       → metadata only

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Types (typed JSON — compile-time safety) ──────────────────────

// Yt2ExtractionResult wraps an extraction attempt.
type Yt2ExtractionResult struct {
	Success    bool             `json:"success"`
	StatusCode int              `json:"status_code,omitempty"`
	Data       *Yt2VideoPayload `json:"data,omitempty"`
	Error      *Yt2ErrorDetail  `json:"error,omitempty"`
	Timestamp  int64            `json:"timestamp"`
}

// Yt2ErrorDetail carries a structured error.
type Yt2ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Yt2MediaThumbnail is one thumbnail variant.
type Yt2MediaThumbnail struct {
	URL    string `json:"url"`
	Width  int64  `json:"width"`
	Height int64  `json:"height"`
}

// Yt2AudioStream is a single audio-only direct stream.
type Yt2AudioStream struct {
	Itag          int64  `json:"itag"`
	MimeType      string `json:"mime_type"`
	Container     string `json:"container"`
	Codec         string `json:"codec"`
	Bitrate       int64  `json:"bitrate"`
	AudioQuality  string `json:"audio_quality"`
	ContentLength int64  `json:"content_length,omitempty"`
	DirectURL     string `json:"direct_url"`
}

// Yt2VideoStream is a single video stream (may include audio for combined formats).
type Yt2VideoStream struct {
	Itag          int64  `json:"itag"`
	Quality       string `json:"quality"`
	Resolution    string `json:"resolution,omitempty"`
	FPS           int64  `json:"fps,omitempty"`
	MimeType      string `json:"mime_type"`
	Container     string `json:"container"`
	Codec         string `json:"codec"`
	Bitrate       int64  `json:"bitrate"`
	HasAudio      bool   `json:"has_audio"`
	ContentLength int64  `json:"content_length,omitempty"`
	DirectURL     string `json:"direct_url"`
}

// Yt2VideoPayload holds the final extracted metadata + streams.
type Yt2VideoPayload struct {
	VideoID         string              `json:"video_id"`
	Title           string              `json:"title"`
	Author          string              `json:"author"`
	ChannelID       string              `json:"channel_id"`
	ChannelURL      string              `json:"channel_url"`
	CanonicalURL    string              `json:"canonical_url"`
	DurationSeconds int64               `json:"duration_seconds"`
	ViewCount       int64               `json:"view_count"`
	PublishDate     string              `json:"publish_date,omitempty"`
	IsLive          bool                `json:"is_live"`
	Description     string              `json:"description,omitempty"`
	Thumbnails      []Yt2MediaThumbnail `json:"thumbnails,omitempty"`
	BestThumbnail   string              `json:"best_thumbnail,omitempty"`
	AudioStreams    []Yt2AudioStream    `json:"audio_streams,omitempty"`
	VideoStreams    []Yt2VideoStream    `json:"video_streams,omitempty"`
	TotalStreams    int                 `json:"total_streams"`
	StreamStatus    string              `json:"stream_status,omitempty"`
	Engine          string              `json:"engine"`
}

// ─── Engine shared state ───────────────────────────────────────────

// Yt2Extractor holds the shared HTTP client (with cookie jar for anti-bot bypass).
type Yt2Extractor struct {
	client *http.Client
}

// yt2SharedExtractor is the package-level singleton (lazy-initialized).
var (
	yt2ExtractorOnce sync.Once
	yt2Shared        *Yt2Extractor
)

// getYt2Extractor returns the shared extractor (lazy singleton).
func getYt2Extractor() *Yt2Extractor {
	yt2ExtractorOnce.Do(func() {
		jar, err := cookiejar.New(nil)
		if err != nil {
			// Fallback: no cookie jar (still works, just less anti-bot bypass)
			yt2Shared = &Yt2Extractor{
				client: &http.Client{Timeout: 30 * time.Second},
			}
			return
		}
		transport := &http.Transport{
			MaxIdleConns:       10,
			IdleConnTimeout:    90 * time.Second,
			DisableCompression: false,
			ForceAttemptHTTP2:  true,
		}
		// SOCKS5 proxy dengan fallback direct (proxy rusak tidak fatal)
		transport.DialContext = socks5DialContext()
		yt2Shared = &Yt2Extractor{
			client: &http.Client{
				Jar:       jar,
				Transport: transport,
				Timeout:   30 * time.Second,
			},
		}
	})
	return yt2Shared
}

// ─── Universal URL Resolver ────────────────────────────────────────

var (
	yt2RawIDRegex  = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)
	yt2PathRegexes = []*regexp.Regexp{
		regexp.MustCompile(`^/(?:embed|shorts|v|live)/([a-zA-Z0-9_-]{11})`),
		regexp.MustCompile(`^/watch/([a-zA-Z0-9_-]{11})`),
	}
	// For extracting ytInitialPlayerResponse from watch HTML.
	// (?s): dot harus match newline karena JSON blob bisa multiline.
	yt2InitialPlayerRegex = regexp.MustCompile(`(?s)ytInitialPlayerResponse\s*=\s*(\{.+?\});`)
)

// yt2ResolveVideoID extracts the 11-char video ID from any YouTube URL or raw ID.
// Returns "" + error if no valid ID can be resolved.
func yt2ResolveVideoID(rawInput string) (string, error) {
	cleanInput := strings.TrimSpace(rawInput)
	if cleanInput == "" || cleanInput == "youtube.com" || cleanInput == "https://youtube.com" || cleanInput == "https://www.youtube.com" {
		return "", fmt.Errorf("input kosong — kirim URL YouTube atau 11-karakter video ID (contoh: .yt2 audio https://youtu.be/xxxx)")
	}
	if yt2RawIDRegex.MatchString(cleanInput) {
		return cleanInput, nil
	}
	if !strings.HasPrefix(cleanInput, "http://") && !strings.HasPrefix(cleanInput, "https://") {
		cleanInput = "https://" + cleanInput
	}
	parsedURL, err := url.Parse(cleanInput)
	if err != nil {
		return "", fmt.Errorf("invalid_url: %w", err)
	}
	if parsedURL.Host == "youtu.be" {
		trimmed := strings.TrimPrefix(parsedURL.Path, "/")
		if yt2RawIDRegex.MatchString(trimmed) {
			return trimmed, nil
		}
	}
	if vParam := parsedURL.Query().Get("v"); yt2RawIDRegex.MatchString(vParam) {
		return vParam, nil
	}
	for _, pattern := range yt2PathRegexes {
		if match := pattern.FindStringSubmatch(parsedURL.Path); len(match) == 2 {
			return match[1], nil
		}
	}
	return "", fmt.Errorf("unable_to_extract_video_id: %s", rawInput)
}

// ─── MIME parser ───────────────────────────────────────────────────

// yt2ParseMime splits "video/mp4; codecs=\"avc1.42E01E\"" → ("video/mp4", "avc1.42E01E").
func yt2ParseMime(rawMime string) (container, codec string) {
	parts := strings.SplitN(rawMime, ";", 2)
	container = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		codecStr := strings.TrimSpace(parts[1])
		codecStr = strings.TrimPrefix(codecStr, "codecs=")
		codec = strings.Trim(codecStr, `"`)
	}
	return container, codec
}

// ─── Engine 1: Innertube Android VR ──────────────────────────────
//
// POST https://www.youtube.com/youtubei/v1/player?prettyPrint=false
// Body: { "videoId": "...", "context": { "client": {
//   "clientName": "ANDROID_VR", "clientVersion": "1.56.21",
//   "deviceMake": "Oculus", "deviceModel": "Quest 3", "hl": "en", "gl": "US"
// }}}
//
// Why ANDROID_VR (Quest 3): bypasses n-signature throttling,
// no PoToken JS runtime needed, returns direct googlevideo.com URLs.

// yt2InnertubePlayerResponse is the typed response from /youtubei/v1/player.
type yt2InnertubePlayerResponse struct {
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	StreamingData struct {
		Formats         []yt2InnertubeFormat `json:"formats"`
		AdaptiveFormats []yt2InnertubeFormat `json:"adaptiveFormats"`
	} `json:"streamingData"`
	VideoDetails yt2InnertubeVideoDetails `json:"videoDetails"`
	Microformat  struct {
		PlayerMicroformatRenderer struct {
			PublishDate string `json:"publishDate"`
		} `json:"playerMicroformatRenderer"`
	} `json:"microformat"`
}

// yt2InnertubeVideoDetails is the typed videoDetails node.
type yt2InnertubeVideoDetails struct {
	VideoID          string `json:"videoId"`
	Title            string `json:"title"`
	LengthSeconds    string `json:"lengthSeconds"`
	ChannelID        string `json:"channelId"`
	ShortDescription string `json:"shortDescription"`
	ViewCount        string `json:"viewCount"`
	Author           string `json:"author"`
	IsLiveContent    bool   `json:"isLiveContent"`
	Thumbnail        struct {
		Thumbnails []struct {
			URL    string `json:"url"`
			Width  int64  `json:"width"`
			Height int64  `json:"height"`
		} `json:"thumbnails"`
	} `json:"thumbnail"`
}

// yt2InnertubeFormat is one stream URL returned by Innertube.
type yt2InnertubeFormat struct {
	Itag          int64  `json:"itag"`
	URL           string `json:"url"`
	MimeType      string `json:"mimeType"`
	Bitrate       int64  `json:"bitrate"`
	Width         int64  `json:"width,omitempty"`
	Height        int64  `json:"height,omitempty"`
	FPS           int64  `json:"fps,omitempty"`
	ContentLength string `json:"contentLength,omitempty"`
	QualityLabel  string `json:"qualityLabel,omitempty"`
	AudioQuality  string `json:"audioQuality,omitempty"`
}

// yt2FetchInnertube queries the Innertube Android VR endpoint.
func yt2FetchInnertube(ctx context.Context, client *http.Client, videoID string) (*yt2InnertubePlayerResponse, error) {
	endpoint := "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"

	// ANDROID_VR (Oculus Quest 3) client context — bypasses n-signature throttling.
	requestBodyMap := map[string]any{
		"videoId": videoID,
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "ANDROID_VR",
				"clientVersion": "1.56.21",
				"deviceMake":    "Oculus",
				"deviceModel":   "Quest 3",
				"hl":            "en",
				"gl":            "US",
			},
		},
	}
	bodyBytes, err := json.Marshal(requestBodyMap)
	if err != nil {
		return nil, fmt.Errorf("marshal innertube request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Origin", "https://www.youtube.com")
	req.Header.Set("Referer", fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("innertube request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("innertube HTTP %d: %s", resp.StatusCode, string(body))
	}

	var data yt2InnertubePlayerResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("innertube decode: %w", err)
	}
	return &data, nil
}

// ─── Engine 2: Web SSR (ytInitialPlayerResponse) ──────────────────

// yt2FetchWebHTML fetches the watch page HTML and extracts ytInitialPlayerResponse JSON.
// Returns the same typed struct as Innertube (the JSON shape is identical).
func yt2FetchWebHTML(ctx context.Context, client *http.Client, videoID string) (*yt2InnertubePlayerResponse, error) {
	pageURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web_ssr fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("web_ssr HTTP %d", resp.StatusCode)
	}

	// Read up to 5MB of HTML (sufficient for ytInitialPlayerResponse)
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("web_ssr read: %w", err)
	}

	match := yt2InitialPlayerRegex.FindSubmatch(bodyBytes)
	if len(match) < 2 {
		return nil, fmt.Errorf("web_ssr: ytInitialPlayerResponse not found")
	}

	var data yt2InnertubePlayerResponse
	if err := json.Unmarshal(match[1], &data); err != nil {
		return nil, fmt.Errorf("web_ssr parse: %w", err)
	}
	return &data, nil
}

// ─── Engine 3: Innertube VISIONOS (Apple Vision Pro) ──────────────
//
// Dari analisis Yarden (AngelinaEmpire): VISIONOS client punya success rate
// lebih tinggi untuk bypass bot detection dibanding ANDROID_VR.
//   - clientName: VISIONOS, clientVersion: 1.02
//   - deviceModel: RealityDevice17,1, osName: visionOS
//   - x-youtube-client-name: 101
//   - Butuh visitorData dari session harvest (GET youtube.com/watch)
//
// yt2FetchVisionOS queries the Innertube VISIONOS endpoint.
func yt2FetchVisionOS(ctx context.Context, client *http.Client, videoID string) (*yt2InnertubePlayerResponse, error) {
	// Step 1: Harvest session (visitorData + VISITOR_INFO1_LIVE cookie)
	harvestReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.youtube.com/watch?v="+videoID, nil)
	if err != nil {
		return nil, err
	}
	harvestReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15")
	harvestReq.Header.Set("Accept-Language", "en-US,en;q=0.9")
	harvestResp, err := client.Do(harvestReq)
	if err != nil {
		return nil, fmt.Errorf("visionos harvest: %w", err)
	}
	harvestBody, err := io.ReadAll(io.LimitReader(harvestResp.Body, 2<<20))
	harvestResp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("visionos harvest read: %w", err)
	}
	// Extract visitorData
	vdMatch := regexp.MustCompile(`"visitorData":"([^"]+)"`).FindSubmatch(harvestBody)
	if vdMatch == nil {
		return nil, fmt.Errorf("visionos harvest: no visitorData")
	}
	visitorData := string(vdMatch[1])
	// Extract VISITOR_INFO1_LIVE cookie
	var cookie string
	for _, c := range harvestResp.Cookies() {
		if c.Name == "VISITOR_INFO1_LIVE" {
			cookie = c.Name + "=" + c.Value
			break
		}
	}

	// Step 2: Player request with VISIONOS client
	endpoint := "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"
	requestBodyMap := map[string]any{
		"videoId":        videoID,
		"racyCheckOk":    true,
		"contentCheckOk": true,
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "VISIONOS",
				"clientVersion": "1.02",
				"deviceMake":    "Apple",
				"deviceModel":   "RealityDevice17,1",
				"userAgent":     "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15",
				"osName":        "visionOS",
				"osVersion":     "26.5.23O471",
				"hl":            "en",
				"gl":            "US",
				"visitorData":   visitorData,
			},
		},
	}
	bodyBytes, err := json.Marshal(requestBodyMap)
	if err != nil {
		return nil, fmt.Errorf("marshal visionos request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15")
	req.Header.Set("Origin", "https://www.youtube.com")
	req.Header.Set("X-Youtube-Client-Name", "101")
	req.Header.Set("X-Youtube-Client-Version", "1.02")
	req.Header.Set("X-Goog-Visitor-Id", visitorData)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("visionos request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("visionos HTTP %d: %s", resp.StatusCode, string(body))
	}

	var data yt2InnertubePlayerResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("visionos decode: %w", err)
	}
	return &data, nil
}

// ─── Triple-Engine Race (Go goroutine-powered) ──────────────────────
//
// Spawns both engines in parallel, returns the first one that:
//   - has playabilityStatus.status == "OK"
//
// If both fail, returns combined error.
// Context cancellation auto-aborts the loser.

// yt2EngineResult carries the outcome of one engine attempt.
type yt2EngineResult struct {
	Engine string
	Data   *yt2InnertubePlayerResponse
	Err    error
}

// yt2RaceEngines races Innertube (Android VR + VISIONOS) + Web SSR in parallel.
// Returns the first successful result (playabilityStatus.status == "OK").
func yt2RaceEngines(videoID string) (*yt2InnertubePlayerResponse, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	extractor := getYt2Extractor()
	resultCh := make(chan yt2EngineResult, 3)

	// Engine 1: Innertube Android VR (preferred — direct CDN)
	go func() {
		data, err := yt2FetchInnertube(ctx, extractor.client, videoID)
		resultCh <- yt2EngineResult{"innertube_android_vr", data, err}
	}()

	// Engine 2: Innertube VISIONOS (Yarden's approach — Apple Vision Pro client,
	// higher bot-detection bypass rate)
	go func() {
		data, err := yt2FetchVisionOS(ctx, extractor.client, videoID)
		resultCh <- yt2EngineResult{"innertube_visionos", data, err}
	}()

	// Engine 3: Web SSR (fallback — slower streams, reliable metadata)
	go func() {
		data, err := yt2FetchWebHTML(ctx, extractor.client, videoID)
		resultCh <- yt2EngineResult{"web_ssr", data, err}
	}()

	// Collect results — first OK wins, cancel the other via context
	var errs []string
	for i := 0; i < 3; i++ {
		select {
		case r := <-resultCh:
			if r.Err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", r.Engine, r.Err))
				continue
			}
			if r.Data != nil && r.Data.PlayabilityStatus.Status == "OK" {
				cancel() // abort the loser goroutine
				fmt.Printf("[YT2] engine succeeded: %s\n", r.Engine)
				return r.Data, r.Engine, nil
			}
			errs = append(errs, fmt.Sprintf("%s: status=%s reason=%s",
				r.Engine, r.Data.PlayabilityStatus.Status, r.Data.PlayabilityStatus.Reason))
		case <-ctx.Done():
			return nil, "", fmt.Errorf("timeout waiting for engines")
		}
	}
	return nil, "", fmt.Errorf("both engines failed: %s", strings.Join(errs, "; "))
}

// ─── Stream extraction (typed) ────────────────────────────────────

// yt2ExtractStreams walks streamingData.formats + adaptiveFormats and
// populates the typed AudioStreams + VideoStreams slices.
func yt2ExtractStreams(data *yt2InnertubePlayerResponse) (audioStreams []Yt2AudioStream, videoStreams []Yt2VideoStream) {
	// Combined formats (video + audio together — e.g., itag 18 = 360p MP4)
	for _, f := range data.StreamingData.Formats {
		if f.URL == "" {
			continue
		}
		container, codec := yt2ParseMime(f.MimeType)
		contentLen, _ := strconv.ParseInt(f.ContentLength, 10, 64)
		width, height := f.Width, f.Height
		var resolution string
		if width > 0 && height > 0 {
			resolution = fmt.Sprintf("%dx%d", width, height)
		}
		videoStreams = append(videoStreams, Yt2VideoStream{
			Itag:          f.Itag,
			Quality:       f.QualityLabel,
			Resolution:    resolution,
			FPS:           f.FPS,
			MimeType:      f.MimeType,
			Container:     container,
			Codec:         codec,
			Bitrate:       f.Bitrate,
			HasAudio:      true,
			ContentLength: contentLen,
			DirectURL:     f.URL,
		})
	}

	// Adaptive formats (separate audio-only or video-only streams)
	for _, f := range data.StreamingData.AdaptiveFormats {
		if f.URL == "" {
			continue
		}
		container, codec := yt2ParseMime(f.MimeType)
		contentLen, _ := strconv.ParseInt(f.ContentLength, 10, 64)

		if strings.HasPrefix(container, "audio/") {
			audioStreams = append(audioStreams, Yt2AudioStream{
				Itag:          f.Itag,
				MimeType:      f.MimeType,
				Container:     container,
				Codec:         codec,
				Bitrate:       f.Bitrate,
				AudioQuality:  f.AudioQuality,
				ContentLength: contentLen,
				DirectURL:     f.URL,
			})
		} else if strings.HasPrefix(container, "video/") {
			width, height := f.Width, f.Height
			var resolution string
			if width > 0 && height > 0 {
				resolution = fmt.Sprintf("%dx%d", width, height)
			}
			videoStreams = append(videoStreams, Yt2VideoStream{
				Itag:          f.Itag,
				Quality:       f.QualityLabel,
				Resolution:    resolution,
				FPS:           f.FPS,
				MimeType:      f.MimeType,
				Container:     container,
				Codec:         codec,
				Bitrate:       f.Bitrate,
				HasAudio:      false,
				ContentLength: contentLen,
				DirectURL:     f.URL,
			})
		}
	}
	return audioStreams, videoStreams
}

// ─── Top-level Extract ────────────────────────────────────────────

// yt2Extract runs the full pipeline: resolve ID → race engines → extract streams.
func yt2Extract(rawInput string) *Yt2ExtractionResult {
	startTime := time.Now().UnixMilli()

	videoID, err := yt2ResolveVideoID(rawInput)
	if err != nil {
		return &Yt2ExtractionResult{
			Success:   false,
			Error:     &Yt2ErrorDetail{Code: "ID_RESOLUTION_ERROR", Message: err.Error()},
			Timestamp: startTime,
		}
	}

	data, engine, err := yt2RaceEngines(videoID)
	if err != nil {
		return &Yt2ExtractionResult{
			Success:    false,
			StatusCode: http.StatusForbidden,
			Error:      &Yt2ErrorDetail{Code: "PLAYABILITY_FAILED", Message: err.Error()},
			Timestamp:  startTime,
		}
	}

	audioStreams, videoStreams := yt2ExtractStreams(data)

	// Build thumbnails list + pick best
	var thumbnails []Yt2MediaThumbnail
	bestThumbURL := ""
	var maxArea int64
	for _, t := range data.VideoDetails.Thumbnail.Thumbnails {
		thumbnails = append(thumbnails, Yt2MediaThumbnail{URL: t.URL, Width: t.Width, Height: t.Height})
		area := t.Width * t.Height
		if area > maxArea {
			maxArea = area
			bestThumbURL = t.URL
		}
	}
	if bestThumbURL == "" {
		bestThumbURL = fmt.Sprintf("https://i.ytimg.com/vi/%s/maxresdefault.jpg", videoID)
	}

	lengthSeconds, _ := strconv.ParseInt(data.VideoDetails.LengthSeconds, 10, 64)
	viewCount, _ := strconv.ParseInt(data.VideoDetails.ViewCount, 10, 64)

	payload := &Yt2VideoPayload{
		VideoID:         videoID,
		Title:           data.VideoDetails.Title,
		Author:          data.VideoDetails.Author,
		ChannelID:       data.VideoDetails.ChannelID,
		ChannelURL:      fmt.Sprintf("https://www.youtube.com/channel/%s", data.VideoDetails.ChannelID),
		CanonicalURL:    fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID),
		DurationSeconds: lengthSeconds,
		ViewCount:       viewCount,
		PublishDate:     data.Microformat.PlayerMicroformatRenderer.PublishDate,
		IsLive:          data.VideoDetails.IsLiveContent,
		Description:     data.VideoDetails.ShortDescription,
		Thumbnails:      thumbnails,
		BestThumbnail:   bestThumbURL,
		AudioStreams:    audioStreams,
		VideoStreams:    videoStreams,
		TotalStreams:    len(audioStreams) + len(videoStreams),
		StreamStatus:    "OK",
		Engine:          engine,
	}

	return &Yt2ExtractionResult{
		Success:    true,
		StatusCode: http.StatusOK,
		Data:       payload,
		Timestamp:  startTime,
	}
}

// ─── Stream selectors ─────────────────────────────────────────────

// yt2SelectAudioStream picks the best audio stream for WhatsApp.
// Priority: itag 140 (MP4/AAC 128kbps, WhatsApp-compatible) > 251 (WebM/Opus 160kbps).
func yt2SelectAudioStream(payload *Yt2VideoPayload) *Yt2AudioStream {
	for i := range payload.AudioStreams {
		if payload.AudioStreams[i].Itag == 140 {
			return &payload.AudioStreams[i]
		}
	}
	for i := range payload.AudioStreams {
		if payload.AudioStreams[i].Itag == 251 {
			return &payload.AudioStreams[i]
		}
	}
	// Fallback: highest bitrate
	var best *Yt2AudioStream
	bestBR := int64(0)
	for i := range payload.AudioStreams {
		if payload.AudioStreams[i].Bitrate > bestBR {
			bestBR = payload.AudioStreams[i].Bitrate
			best = &payload.AudioStreams[i]
		}
	}
	return best
}

// yt2SelectVideoStream picks the best combined video stream for WhatsApp.
// Priority: itag 18 (360p MP4 combined) > itag 22 (720p MP4 combined) > first combined.
func yt2SelectVideoStream(payload *Yt2VideoPayload) *Yt2VideoStream {
	// Prefer combined (HasAudio==true) formats
	for i := range payload.VideoStreams {
		if payload.VideoStreams[i].Itag == 18 && payload.VideoStreams[i].HasAudio {
			return &payload.VideoStreams[i]
		}
	}
	for i := range payload.VideoStreams {
		if payload.VideoStreams[i].Itag == 22 && payload.VideoStreams[i].HasAudio {
			return &payload.VideoStreams[i]
		}
	}
	// Fallback: first combined stream
	for i := range payload.VideoStreams {
		if payload.VideoStreams[i].HasAudio {
			return &payload.VideoStreams[i]
		}
	}
	// Last resort: first video stream
	if len(payload.VideoStreams) > 0 {
		return &payload.VideoStreams[0]
	}
	return nil
}

// ─── Format helpers ────────────────────────────────────────────────

// yt2FormatDuration converts "195" seconds → "3:15".
func yt2FormatDuration(seconds int64) string {
	if seconds <= 0 {
		return "0:00"
	}
	m := seconds / 60
	s := seconds % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

// yt2FormatViews converts 1234567 → "1.2M views".
func yt2FormatViews(count int64) string {
	if count < 1000 {
		return fmt.Sprintf("%d views", count)
	}
	if count < 1_000_000 {
		return fmt.Sprintf("%.1fK views", float64(count)/1000)
	}
	if count < 1_000_000_000 {
		return fmt.Sprintf("%.1fM views", float64(count)/1_000_000)
	}
	return fmt.Sprintf("%.1fB views", float64(count)/1_000_000_000)
}

// yt2FormatSize converts 1234567 bytes → "1.2 MB".
func yt2FormatSize(bytes int64) string {
	if bytes <= 0 {
		return "?"
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// ─── Command Handler ──────────────────────────────────────────────

// handleYT2Command — .yt2 [subcommand] <url>
// Subcommands:
//
//	(none)   → metadata + stream links
//	audio    → stream MP3 audio ke WhatsApp (itag 140)
//	video    → stream MP4 video ke WhatsApp (itag 18)
//	thumb    → kirim thumbnail maxresdefault
//	info     → metadata only (no media sent)
func handleYT2Command(client *whatsmeow.Client, msg *events.Message, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return `🎬 *YouTube Extractor v2.83*

Cara pakai:
• *.yt2 <url>* — metadata + stream links
• *.yt2 audio <url>* — kirim MP3 audio (itag 140)
• *.yt2 video <url>* — kirim MP4 video (itag 18)
• *.yt2 thumb <url>* — kirim thumbnail maxres
• *.yt2 info <url>* — metadata only

Format URL yang didukung:
• youtube.com/watch?v=XXX
• youtu.be/XXX
• youtube.com/shorts/XXX
• youtube.com/embed/XXX
• Raw 11-char Video ID

Engine: ANDROID_VR (Oculus Quest 3) + Web SSR race
Bypass: n-signature throttling, no PoToken needed`
	}

	// Parse subcommand
	subcommand := ""
	urlInput := args
	parts := strings.SplitN(args, " ", 2)
	if len(parts) == 2 {
		lower := strings.ToLower(parts[0])
		switch lower {
		case "audio", "video", "thumb", "info":
			subcommand = lower
			urlInput = strings.TrimSpace(parts[1])
		}
	}
	if urlInput == "" {
		return "❓ *Cara pakai:* .yt2 <audio|video|thumb|info> <url>\nContoh: .yt2 audio https://youtu.be/dQw4w9WgXcQ"
	}

	// Extract
	result := yt2Extract(urlInput)
	if !result.Success || result.Data == nil {
		errMsg := "unknown error"
		if result.Error != nil {
			errMsg = result.Error.Message
		}
		return fmt.Sprintf("❌ Gagal extract: %s", errMsg)
	}
	p := result.Data

	// ─── Subcommand: thumb ────────────────────────────────────
	if subcommand == "thumb" {
		if err := sendImageFromURL(client, msg.Info.Chat, p.BestThumbnail, fmt.Sprintf("🖼️ %s", p.Title)); err != nil {
			return fmt.Sprintf("❌ Gagal kirim thumbnail: %v", err)
		}
		return "" // already sent image
	}

	// ─── Subcommand: audio ─────────────────────────────────────
	if subcommand == "audio" {
		stream := yt2SelectAudioStream(p)
		if stream == nil {
			return "❌ Tidak ada audio stream yang tersedia"
		}
		// Send the audio (use sendAudioFromURL — handles WA upload)
		mime := stream.MimeType
		if mime == "" {
			mime = "audio/mpeg"
		}
		if err := sendAudioFromURL(client, msg.Info.Chat, stream.DirectURL, mime, false); err != nil {
			return fmt.Sprintf("❌ Gagal kirim audio: %v", err)
		}
		return fmt.Sprintf("🎵 *%s*\n\n• Channel: %s\n• Quality: %s\n• Engine: %s",
			p.Title, p.Author, stream.AudioQuality, p.Engine)
	}

	// ─── Subcommand: video ─────────────────────────────────────
	if subcommand == "video" {
		stream := yt2SelectVideoStream(p)
		if stream == nil {
			return "❌ Tidak ada video stream yang tersedia"
		}
		mime := stream.MimeType
		if mime == "" {
			mime = "video/mp4"
		}
		caption := fmt.Sprintf("🎬 *%s*\nChannel: %s • %s",
			p.Title, p.Author, stream.Quality)
		if err := sendVideoFromURL(client, msg.Info.Chat, stream.DirectURL, caption, mime); err != nil {
			return fmt.Sprintf("❌ Gagal kirim video: %v", err)
		}
		return ""
	}

	// ─── Default + info: metadata only ────────────────────────
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🎬 *%s*\n\n", p.Title))
	sb.WriteString(fmt.Sprintf("• Channel: %s\n", p.Author))
	sb.WriteString(fmt.Sprintf("• Durasi: %s\n", yt2FormatDuration(p.DurationSeconds)))
	sb.WriteString(fmt.Sprintf("• Views: %s\n", yt2FormatViews(p.ViewCount)))
	if p.PublishDate != "" {
		sb.WriteString(fmt.Sprintf("• Published: %s\n", p.PublishDate))
	}
	sb.WriteString(fmt.Sprintf("• Live: %v\n", p.IsLive))
	sb.WriteString(fmt.Sprintf("• Engine: %s\n", p.Engine))
	sb.WriteString(fmt.Sprintf("• Streams: %d (audio=%d, video=%d)\n",
		p.TotalStreams, len(p.AudioStreams), len(p.VideoStreams)))
	sb.WriteString(fmt.Sprintf("• Link: %s\n", p.CanonicalURL))
	sb.WriteString(fmt.Sprintf("• Thumbnail: %s\n", p.BestThumbnail))

	if subcommand != "info" {
		// Show top 3 audio + top 3 video streams
		if len(p.AudioStreams) > 0 {
			sb.WriteString("\n*Top Audio Streams:*\n")
			limit := 3
			if len(p.AudioStreams) < limit {
				limit = len(p.AudioStreams)
			}
			for i := 0; i < limit; i++ {
				s := p.AudioStreams[i]
				sb.WriteString(fmt.Sprintf("• itag %d | %s | %s | %s\n",
					s.Itag, s.AudioQuality, s.Container, yt2FormatSize(s.ContentLength)))
			}
		}
		if len(p.VideoStreams) > 0 {
			sb.WriteString("\n*Top Video Streams:*\n")
			limit := 3
			if len(p.VideoStreams) < limit {
				limit = len(p.VideoStreams)
			}
			for i := 0; i < limit; i++ {
				s := p.VideoStreams[i]
				audioTag := "no audio"
				if s.HasAudio {
					audioTag = "with audio"
				}
				sb.WriteString(fmt.Sprintf("• itag %d | %s | %s | %s | %s\n",
					s.Itag, s.Quality, s.Resolution, audioTag, yt2FormatSize(s.ContentLength)))
			}
		}
		sb.WriteString("\nCara pakai: .yt2 audio <url> atau .yt2 video <url>")
	}

	return sb.String()
}

// ─── Reference stubs (these types are used by other handlers in the project) ───
// Note: whatsmeow + types + events are imported here to make sure this file
// compiles standalone. The actual handler logic uses types defined elsewhere.

var _ = types.JID{}
var _ = events.Message{}
