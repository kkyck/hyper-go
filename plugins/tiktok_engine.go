package main

// tiktok_engine.go — TikTok search & extraction engine (pure Go).
//
// Dipakai oleh .tt:
//   - .tt <tiktok-url> → enrichItem (mobile API → web SSR fallback), download
//     video no-watermark, kirim ke chat.
//   - .tt <keyword>    → Search via headless Chrome (chromedp) + enrichItem,
//     balas daftar hasil.
//
// Engine memakai Chrome headless-shell di ~/.local/chrome (persisten).
// Bisa di-override lewat env CHROME_HEADLESS_SHELL.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"
	"github.com/tidwall/gjson"
)

type ExtractionResult struct {
	Success    bool         `json:"success"`
	StatusCode int          `json:"status_code,omitempty"`
	Data       any          `json:"data,omitempty"`
	Error      *ErrorDetail `json:"error,omitempty"`
	Timestamp  int64        `json:"timestamp"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type TikTokAuthorStats struct {
	FollowerCount  int64 `json:"follower_count"`
	FollowingCount int64 `json:"following_count"`
	HeartCount     int64 `json:"heart_count"`
	VideoCount     int64 `json:"video_count"`
}

type TikTokAuthor struct {
	ID           string             `json:"id"`
	UniqueID     string             `json:"unique_id"`
	Nickname     string             `json:"nickname"`
	Avatar       string             `json:"avatar"`
	AvatarLarger string             `json:"avatar_larger,omitempty"`
	Signature    string             `json:"signature,omitempty"`
	Verified     bool               `json:"verified"`
	SecUID       string             `json:"sec_uid,omitempty"`
	Stats        *TikTokAuthorStats `json:"stats,omitempty"`
}

type TikTokStats struct {
	Plays     int64 `json:"plays"`
	Likes     int64 `json:"likes"`
	Comments  int64 `json:"comments"`
	Shares    int64 `json:"shares"`
	Bookmarks int64 `json:"bookmarks"`
}

type TikTokMusic struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Author   string `json:"author"`
	PlayURL  string `json:"play_url"`
	Duration int64  `json:"duration_seconds"`
	Cover    string `json:"cover,omitempty"`
	Original bool   `json:"original"`
}

type BitrateStream struct {
	QualityType string `json:"quality_type"`
	Bitrate     int64  `json:"bitrate"`
	CodecType   string `json:"codec_type"`
	URL         string `json:"url"`
}

type SubtitleInfo struct {
	Language string `json:"language"`
	URL      string `json:"url"`
	Format   string `json:"format"`
}

type TikTokVideo struct {
	NoWatermarkURL   string          `json:"no_watermark_url"`
	WatermarkURL     string          `json:"watermark_url,omitempty"`
	Duration         int64           `json:"duration_seconds"`
	Width            int64           `json:"width"`
	Height           int64           `json:"height"`
	Ratio            string          `json:"ratio,omitempty"`
	Format           string          `json:"format,omitempty"`
	CodecType        string          `json:"codec_type,omitempty"`
	Definition       string          `json:"definition,omitempty"`
	AdaptiveBitrates []BitrateStream `json:"adaptive_bitrates,omitempty"`
	Subtitles        []SubtitleInfo  `json:"subtitles,omitempty"`
}

type TikTokChallenge struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Desc  string `json:"desc,omitempty"`
}

type TikTokVideoMetadata struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Description  string            `json:"description"`
	CreatedAt    int64             `json:"created_at"`
	Region       string            `json:"region,omitempty"`
	Cover        string            `json:"cover"`
	DynamicCover string            `json:"dynamic_cover,omitempty"`
	CardViews    string            `json:"card_views,omitempty"`
	Author       TikTokAuthor      `json:"author"`
	Stats        TikTokStats       `json:"stats"`
	Music        TikTokMusic       `json:"music"`
	Video        *TikTokVideo      `json:"video,omitempty"`
	Images       []string          `json:"images,omitempty"`
	Challenges   []TikTokChallenge `json:"challenges,omitempty"`
	CanonicalURL string            `json:"canonical_url"`
}

type SearchResponse struct {
	Keyword string                `json:"keyword"`
	Total   int                   `json:"total"`
	Videos  []TikTokVideoMetadata `json:"videos"`
}

type RawCardItem struct {
	URL   string
	Views string
}

type TikTokSearchEngine struct {
	client *http.Client
}

func randomIPv4() string {
	return fmt.Sprintf("%d.%d.%d.%d", rand.IntN(220)+1, rand.IntN(254)+1, rand.IntN(254)+1, rand.IntN(254)+1)
}

// chromeHeadlessShellPath mengembalikan path ke chrome-headless-shell.
// Urutan: env CHROME_HEADLESS_SHELL → ~/.local/chrome/... → "" (pakai default chromedp).
func chromeHeadlessShellPath() string {
	if p := os.Getenv("CHROME_HEADLESS_SHELL"); p != "" {
		return p
	}
	if h, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(h, ".local", "chrome", "chrome-headless-shell-linux64", "chrome-headless-shell")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func NewTikTokSearchEngine(timeout time.Duration) (*TikTokSearchEngine, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookiejar_init_failed: %w", err)
	}

	transport := &http.Transport{
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: false,
		ForceAttemptHTTP2:  true,
		// Pakai proxy SOCKS5 bila SOCKS5_PROXY diset (untuk IP yang kena blokir
		// TikTok); fallback direct otomatis bila proxy bermasalah.
		DialContext: socks5DialContext(),
	}

	client := &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   timeout,
	}

	return &TikTokSearchEngine{client: client}, nil
}

func (s *TikTokSearchEngine) applyWebHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,id;q=0.8")
	req.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")

	spoofedIP := randomIPv4()
	req.Header.Set("X-Forwarded-For", spoofedIP)
	req.Header.Set("Client-IP", spoofedIP)
	req.Header.Set("X-Real-IP", spoofedIP)

	if referer != "" {
		req.Header.Set("Referer", referer)
	} else {
		req.Header.Set("Referer", "https://www.tiktok.com/")
	}
}

func (s *TikTokSearchEngine) applyMobileHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "com.zhiliaoapp.musically/2022600030 (Linux; U; Android 13; en_US; Pixel 7; Build/TQ3A.230901.001;tt-ok/3.12.13.1)")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Connection", "keep-alive")

	spoofedIP := randomIPv4()
	req.Header.Set("X-Forwarded-For", spoofedIP)
	req.Header.Set("Client-IP", spoofedIP)
	req.Header.Set("X-Real-IP", spoofedIP)
}

func (s *TikTokSearchEngine) solveSlardarChallenge(html string) (string, string, bool) {
	reCS := regexp.MustCompile(`<p\s+id=["']cs["'][^>]*class=["']([^"'>]+)`)
	reWCI := regexp.MustCompile(`<p\s+id=["']wci["'][^>]*class=["']([^"'>]+)`)

	matchCS := reCS.FindStringSubmatch(html)
	matchWCI := reWCI.FindStringSubmatch(html)

	if len(matchCS) < 2 || len(matchWCI) < 2 {
		return "", "", false
	}

	rawCS := matchCS[1]
	wciName := matchWCI[1]

	padLen := (4 - len(rawCS)%4) % 4
	paddedCS := rawCS + strings.Repeat("=", padLen)

	decodedBytes, err := base64.StdEncoding.DecodeString(paddedCS)
	if err != nil {
		return "", "", false
	}

	var challengeMap map[string]any
	if err := json.Unmarshal(decodedBytes, &challengeMap); err != nil {
		return "", "", false
	}

	vMap, ok := challengeMap["v"].(map[string]any)
	if !ok {
		return "", "", false
	}

	strA, okA := vMap["a"].(string)
	strC, okC := vMap["c"].(string)
	if !okA || !okC {
		return "", "", false
	}

	seed, err := base64.StdEncoding.DecodeString(strA)
	if err != nil {
		return "", "", false
	}
	expectedDigest, err := base64.StdEncoding.DecodeString(strC)
	if err != nil {
		return "", "", false
	}

	solved := false
	for i := 0; i <= 1000000; i++ {
		numBytes := []byte(strconv.Itoa(i))
		candidate := append(append([]byte(nil), seed...), numBytes...)
		digest := sha256.Sum256(candidate)
		if bytes.Equal(digest[:], expectedDigest) {
			challengeMap["d"] = base64.StdEncoding.EncodeToString(numBytes)
			solved = true
			break
		}
	}

	if !solved {
		return "", "", false
	}

	serialized, err := json.Marshal(challengeMap)
	if err != nil {
		return "", "", false
	}

	cookieVal := base64.StdEncoding.EncodeToString(serialized)
	return wciName, cookieVal, true
}

func (s *TikTokSearchEngine) extractVideoID(rawURL string) string {
	idRe := regexp.MustCompile(`/(?:video|photo)/(\d+)`)
	matches := idRe.FindStringSubmatch(rawURL)
	if len(matches) > 1 {
		return matches[1]
	}
	numRe := regexp.MustCompile(`^\d+$`)
	if numRe.MatchString(rawURL) {
		return rawURL
	}
	return ""
}

func (s *TikTokSearchEngine) resolveCanonicalInfo(ctx context.Context, rawInput string) (string, string) {
	clean := strings.TrimSpace(rawInput)

	reDigits := regexp.MustCompile(`^\d{17,21}$`)
	if reDigits.MatchString(clean) {
		return fmt.Sprintf("https://www.tiktok.com/@user/video/%s", clean), clean
	}

	if !strings.HasPrefix(clean, "http://") && !strings.HasPrefix(clean, "https://") {
		clean = "https://" + clean
	}

	finalURL := clean
	headCtx, headCancel := context.WithTimeout(ctx, 10*time.Second)
	defer headCancel()
	req, err := http.NewRequestWithContext(headCtx, http.MethodHead, clean, nil)
	if err == nil {
		s.applyWebHeaders(req, "")
		resp, err := s.client.Do(req)
		if err == nil {
			if u := resp.Request.URL.String(); u != "" {
				finalURL = u
			}
			resp.Body.Close()
		}
	}

	reVideoID := regexp.MustCompile(`(?:video|photo)/(\d{17,21})`)
	match := reVideoID.FindStringSubmatch(finalURL)
	if len(match) > 1 {
		return finalURL, match[1]
	}

	return finalURL, clean
}

func (s *TikTokSearchEngine) extractViaMobileAPI(ctx context.Context, awemeID string, canonicalURL string) (*TikTokVideoMetadata, error) {
	hosts := []string{
		"api16-normal-c-useast1a.tiktokv.com",
		"api22-normal-c-useast1a.tiktokv.com",
		"api.tiktokv.com",
	}

	deviceID := fmt.Sprintf("%d", 7250000000000000000+rand.Int64N(100000000000000000))

	q := url.Values{}
	q.Set("aweme_id", awemeID)
	q.Set("version_code", "260103")
	q.Set("app_name", "musical_ly")
	q.Set("channel", "googleplay")
	q.Set("device_platform", "android")
	q.Set("aid", "1233")
	q.Set("device_id", deviceID)
	q.Set("os_version", "13")

	for _, h := range hosts {
		endpoint := fmt.Sprintf("https://%s/aweme/v1/feed/?%s", h, q.Encode())
		// Timeout per-host pendek: host TikTok sering gantung dari datacenter IP.
		hctx, hcancel := context.WithTimeout(ctx, 12*time.Second)
		req, err := http.NewRequestWithContext(hctx, http.MethodGet, endpoint, nil)
		if err != nil {
			hcancel()
			continue
		}
		s.applyMobileHeaders(req)

		resp, err := s.client.Do(req)
		if err != nil {
			hcancel()
			continue
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		hcancel()
		if err != nil || len(bodyBytes) == 0 {
			continue
		}

		parsed := gjson.ParseBytes(bodyBytes)
		awemeList := parsed.Get("aweme_list").Array()
		if len(awemeList) == 0 {
			continue
		}

		item := awemeList[0]
		desc := item.Get("desc").String()
		createTime := item.Get("create_time").Int()
		region := item.Get("region").String()

		authorNode := item.Get("author")
		statsNode := item.Get("statistics")
		musicNode := item.Get("music")
		videoNode := item.Get("video")

		var authorStats *TikTokAuthorStats
		if authorNode.Exists() {
			authorStats = &TikTokAuthorStats{
				FollowerCount:  authorNode.Get("follower_count").Int(),
				FollowingCount: authorNode.Get("following_count").Int(),
				HeartCount:     authorNode.Get("total_favorited").Int(),
				VideoCount:     authorNode.Get("aweme_count").Int(),
			}
		}

		author := TikTokAuthor{
			ID:           authorNode.Get("uid").String(),
			UniqueID:     authorNode.Get("unique_id").String(),
			Nickname:     authorNode.Get("nickname").String(),
			Avatar:       authorNode.Get("avatar_thumb.url_list.0").String(),
			AvatarLarger: authorNode.Get("avatar_larger.url_list.0").String(),
			Signature:    authorNode.Get("signature").String(),
			Verified:     authorNode.Get("custom_verify").String() != "" || authorNode.Get("enterprise_verify_reason").String() != "",
			SecUID:       authorNode.Get("sec_uid").String(),
			Stats:        authorStats,
		}

		stats := TikTokStats{
			Plays:     statsNode.Get("play_count").Int(),
			Likes:     statsNode.Get("digg_count").Int(),
			Comments:  statsNode.Get("comment_count").Int(),
			Shares:    statsNode.Get("share_count").Int(),
			Bookmarks: statsNode.Get("collect_count").Int(),
		}

		musicPlay := musicNode.Get("play_url.url_list.0").String()
		if musicPlay == "" {
			musicPlay = musicNode.Get("play_url.uri").String()
		}

		music := TikTokMusic{
			ID:       musicNode.Get("id_str").String(),
			Title:    musicNode.Get("title").String(),
			Author:   musicNode.Get("author").String(),
			PlayURL:  musicPlay,
			Duration: musicNode.Get("duration").Int(),
			Cover:    musicNode.Get("cover_large.url_list.0").String(),
			Original: musicNode.Get("is_original").Bool(),
		}

		var images []string
		imagePostList := item.Get("image_post_info.images").Array()
		if len(imagePostList) > 0 {
			for _, imgItem := range imagePostList {
				displayURL := imgItem.Get("display_image.url_list.0").String()
				if displayURL == "" {
					displayURL = imgItem.Get("thumbnail.url_list.0").String()
				}
				if displayURL != "" {
					images = append(images, displayURL)
				}
			}
		}

		mediaType := "video"
		var videoObj *TikTokVideo

		if len(images) > 0 {
			mediaType = "photo"
		} else if videoNode.Exists() {
			var adaptiveBitrates []BitrateStream
			for _, b := range videoNode.Get("bit_rate").Array() {
				adaptiveBitrates = append(adaptiveBitrates, BitrateStream{
					QualityType: b.Get("gear_name").String(),
					Bitrate:     b.Get("bit_rate").Int(),
					CodecType:   b.Get("play_addr.data_size").String(),
					URL:         b.Get("play_addr.url_list.0").String(),
				})
			}

			videoObj = &TikTokVideo{
				NoWatermarkURL:   videoNode.Get("play_addr.url_list.0").String(),
				WatermarkURL:     videoNode.Get("download_addr.url_list.0").String(),
				Duration:         item.Get("duration").Int() / 1000,
				Width:            videoNode.Get("width").Int(),
				Height:           videoNode.Get("height").Int(),
				Ratio:            videoNode.Get("ratio").String(),
				Format:           videoNode.Get("format").String(),
				Definition:       videoNode.Get("definition").String(),
				AdaptiveBitrates: adaptiveBitrates,
			}
		}

		var challenges []TikTokChallenge
		for _, ch := range item.Get("cha_list").Array() {
			challenges = append(challenges, TikTokChallenge{
				ID:    ch.Get("cid").String(),
				Title: ch.Get("cha_name").String(),
				Desc:  ch.Get("desc").String(),
			})
		}

		return &TikTokVideoMetadata{
			ID:           item.Get("aweme_id").String(),
			Type:         mediaType,
			Description:  desc,
			CreatedAt:    createTime,
			Region:       region,
			Cover:        videoNode.Get("cover.url_list.0").String(),
			DynamicCover: videoNode.Get("dynamic_cover.url_list.0").String(),
			Author:       author,
			Stats:        stats,
			Music:        music,
			Video:        videoObj,
			Images:       images,
			Challenges:   challenges,
			CanonicalURL: canonicalURL,
		}, nil
	}

	return nil, fmt.Errorf("mobile_feed_exhausted")
}

func (s *TikTokSearchEngine) extractViaWebSSR(ctx context.Context, targetURL, awemeID string) (*TikTokVideoMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	s.applyWebHeaders(req, targetURL)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	html := string(bodyBytes)
	if !strings.Contains(html, "__UNIVERSAL_DATA_FOR_REHYDRATION__") {
		wciName, cookieVal, ok := s.solveSlardarChallenge(html)
		if ok {
			req2, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
			if err == nil {
				s.applyWebHeaders(req2, targetURL)
				req2.Header.Set("Cookie", fmt.Sprintf("%s=%s", wciName, cookieVal))

				resp2, err := s.client.Do(req2)
				if err == nil {
					bodyBytes2, err := io.ReadAll(resp2.Body)
					resp2.Body.Close()
					if err == nil {
						html = string(bodyBytes2)
					}
				}
			}
		}
	}

	reScript := regexp.MustCompile(`<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__"[^>]*>([\s\S]*?)</script>`)
	match := reScript.FindStringSubmatch(html)
	if len(match) < 2 {
		return nil, fmt.Errorf("hydration_script_not_found")
	}

	parsed := gjson.Parse(match[1])
	var item gjson.Result
	scope := parsed.Get("__DEFAULT_SCOPE__")
	if scope.Exists() {
		for k, v := range scope.Map() {
			if strings.Contains(k, "video-detail") {
				item = v.Get("itemInfo.itemStruct")
				break
			}
		}
	}
	if !item.Exists() {
		item = parsed.Get(`__DEFAULT_SCOPE__.webapp\.video-detail.itemInfo.itemStruct`)
	}
	if !item.Exists() {
		return nil, fmt.Errorf("item_struct_empty")
	}

	id := item.Get("id").String()
	if id == "" {
		id = awemeID
	}
	desc := item.Get("desc").String()
	createTime, _ := strconv.ParseInt(item.Get("createTime").String(), 10, 64)
	locationCreated := item.Get("locationCreated").String()

	videoNode := item.Get("video")
	authorNode := item.Get("author")
	authorStatsNode := item.Get("authorStats")
	musicNode := item.Get("music")
	statsNode := item.Get("stats")

	var authorStats *TikTokAuthorStats
	if authorStatsNode.Exists() {
		authorStats = &TikTokAuthorStats{
			FollowerCount:  authorStatsNode.Get("followerCount").Int(),
			FollowingCount: authorStatsNode.Get("followingCount").Int(),
			HeartCount:     authorStatsNode.Get("heartCount").Int(),
			VideoCount:     authorStatsNode.Get("videoCount").Int(),
		}
	}

	author := TikTokAuthor{
		ID:           authorNode.Get("id").String(),
		UniqueID:     authorNode.Get("uniqueId").String(),
		Nickname:     authorNode.Get("nickname").String(),
		Avatar:       authorNode.Get("avatarThumb").String(),
		AvatarLarger: authorNode.Get("avatarLarger").String(),
		Signature:    authorNode.Get("signature").String(),
		Verified:     authorNode.Get("verified").Bool(),
		SecUID:       authorNode.Get("secUid").String(),
		Stats:        authorStats,
	}

	stats := TikTokStats{
		Plays:     statsNode.Get("playCount").Int(),
		Likes:     statsNode.Get("diggCount").Int(),
		Comments:  statsNode.Get("commentCount").Int(),
		Shares:    statsNode.Get("shareCount").Int(),
		Bookmarks: statsNode.Get("collectCount").Int(),
	}

	music := TikTokMusic{
		ID:       musicNode.Get("id").String(),
		Title:    musicNode.Get("title").String(),
		Author:   musicNode.Get("authorName").String(),
		PlayURL:  musicNode.Get("playUrl").String(),
		Duration: musicNode.Get("duration").Int(),
		Cover:    musicNode.Get("coverLarge").String(),
		Original: musicNode.Get("original").Bool(),
	}

	var images []string
	imagePostList := item.Get("imagePost.images").Array()
	for _, imgItem := range imagePostList {
		imgURL := imgItem.Get("imageURL.urlList.0").String()
		if imgURL != "" {
			images = append(images, imgURL)
		}
	}

	mediaType := "video"
	var videoObj *TikTokVideo

	if len(images) > 0 {
		mediaType = "photo"
	} else if videoNode.Exists() {
		var adaptiveBitrates []BitrateStream
		for _, b := range videoNode.Get("bitrateInfo").Array() {
			firstURL := b.Get("PlayAddr.UrlList.0").String()
			if firstURL != "" {
				adaptiveBitrates = append(adaptiveBitrates, BitrateStream{
					QualityType: b.Get("QualityType").String(),
					Bitrate:     b.Get("Bitrate").Int(),
					CodecType:   b.Get("CodecType").String(),
					URL:         firstURL,
				})
			}
		}

		var subtitles []SubtitleInfo
		for _, subItem := range videoNode.Get("subtitleInfos").Array() {
			subURL := subItem.Get("Url").String()
			if subURL != "" {
				subtitles = append(subtitles, SubtitleInfo{
					Language: subItem.Get("LanguageCodeName").String(),
					URL:      subURL,
					Format:   subItem.Get("Format").String(),
				})
			}
		}

		videoObj = &TikTokVideo{
			NoWatermarkURL:   videoNode.Get("playAddr").String(),
			WatermarkURL:     videoNode.Get("downloadAddr").String(),
			Duration:         videoNode.Get("duration").Int(),
			Width:            videoNode.Get("width").Int(),
			Height:           videoNode.Get("height").Int(),
			Ratio:            videoNode.Get("ratio").String(),
			Format:           videoNode.Get("format").String(),
			CodecType:        videoNode.Get("codecType").String(),
			Definition:       videoNode.Get("definition").String(),
			AdaptiveBitrates: adaptiveBitrates,
			Subtitles:        subtitles,
		}
	}

	var challenges []TikTokChallenge
	for _, ch := range item.Get("challenges").Array() {
		challenges = append(challenges, TikTokChallenge{
			ID:    ch.Get("id").String(),
			Title: ch.Get("title").String(),
			Desc:  ch.Get("desc").String(),
		})
	}

	return &TikTokVideoMetadata{
		ID:           id,
		Type:         mediaType,
		Description:  desc,
		CreatedAt:    createTime,
		Region:       locationCreated,
		Cover:        videoNode.Get("cover").String(),
		DynamicCover: videoNode.Get("dynamicCover").String(),
		Author:       author,
		Stats:        stats,
		Music:        music,
		Video:        videoObj,
		Images:       images,
		Challenges:   challenges,
		CanonicalURL: targetURL,
	}, nil
}

// extractVideoURLViaChrome memuat halaman video TikTok di headless Chrome dan
// mengambil src elemen <video> yang sebenarnya — URL ini sudah mengandung token
// valid (hasil eksekusi JS player TikTok), tidak seperti playAddr di SSR yang
// masih placeholder tk=tt_chain_token dan selalu 403.
func (s *TikTokSearchEngine) extractVideoURLViaChrome(ctx context.Context, pageURL string) (string, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("window-size", "1280,720"),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		chromedp.Flag("ignore-certificate-errors", true),
	)
	if ep := chromeHeadlessShellPath(); ep != "" {
		opts = append(opts, chromedp.ExecPath(ep))
	}
	if ra := ensureProxyRelay(); ra != "" {
		opts = append(opts, chromedp.ProxyServer("http://"+ra))
	}

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	allocCtx, allocCancel := chromedp.NewExecAllocator(cctx, opts...)
	defer allocCancel()
	cdpCtx, cdpCancel := chromedp.NewContext(allocCtx)
	defer cdpCancel()

	stealthJS := `
		delete Object.getPrototypeOf(navigator).webdriver;
		Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		window.chrome = { runtime: {} };
		Object.defineProperty(navigator, 'plugins', { get: () => [1, 2, 3] });
		Object.defineProperty(navigator, 'languages', { get: () => ['en-US', 'en'] });
	`

	var videoSrc string
	getSrcJS := `
		(function() {
			const vids = document.querySelectorAll('video');
			for (const v of vids) {
				const src = v.currentSrc || v.src;
				if (src && src.startsWith('http') && !src.startsWith('blob:')) return src;
				const sources = v.querySelectorAll('source');
				for (const sc of sources) {
					if (sc.src && sc.src.startsWith('http')) return sc.src;
				}
			}
			return '';
		})()
	`

	if _, err := chromedp.Run(cdpCtx, chromedp.Navigate("https://www.tiktok.com/")); err != nil {
		return "", err
	}
	_, _ = chromedp.Run(cdpCtx, chromedp.Sleep(1500*time.Millisecond))
	_, _ = chromedp.Run(cdpCtx, chromedp.Evaluate[any](stealthJS))
	if _, err := chromedp.Run(cdpCtx, chromedp.Navigate(pageURL)); err != nil {
		return "", err
	}
	_, _ = chromedp.Run(cdpCtx, chromedp.Sleep(6*time.Second))

	videoSrc, _ = chromedp.Run(cdpCtx, chromedp.Evaluate[string](getSrcJS))
	if videoSrc == "" {
		// Coba sekali lagi setelah scroll/interaksi ringan
		_, _ = chromedp.Run(cdpCtx, chromedp.Evaluate[chromedp.Void](`window.scrollBy(0, 300)`))
		_, _ = chromedp.Run(cdpCtx, chromedp.Sleep(3*time.Second))
		videoSrc, _ = chromedp.Run(cdpCtx, chromedp.Evaluate[string](getSrcJS))
	}
	if videoSrc == "" {
		return "", fmt.Errorf("video_src_not_found")
	}
	return videoSrc, nil
}

// extractViaAIOFallback memakai layanan videodownloadertools.com sebagai
// alternatif bila metode utama gagal. Port dari scrape-aio.js (XZEERH):
// POST form-urlencoded {url} ke /fetch_api.php, parse JSON medias/links.
// Catatan: backend-nya tergantung kuota RapidAPI mereka — bisa "failed"
// kalau kuota habis, jadi ini murni fallback, bukan utama.
func (s *TikTokSearchEngine) extractViaAIOFallback(ctx context.Context, pageURL string) (videoURL, title string, err error) {
	form := url.Values{}
	form.Set("url", pageURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		videoDownloaderToolsBaseURL()+"/fetch_api.php",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", videoDownloaderToolsBaseURL()+"/")
	req.Header.Set("Origin", videoDownloaderToolsBaseURL())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		return "", "", fmt.Errorf("aio http %d", resp.StatusCode)
	}
	parsed := gjson.ParseBytes(body)
	if parsed.Get("status").String() == "error" {
		return "", "", fmt.Errorf("aio: %s", parsed.Get("message").String())
	}
	title = parsed.Get("title").String()
	var medias []gjson.Result
	for _, k := range []string{"medias", "links", "data"} {
		if a := parsed.Get(k).Array(); len(a) > 0 {
			medias = a
			break
		}
	}
	// Pilih URL video MP4 kualitas terbaik
	best := ""
	for _, m := range medias {
		u := m.Get("url").String()
		if u == "" {
			u = m.Get("link").String()
		}
		if u == "" {
			u = m.Get("download_url").String()
		}
		if u == "" {
			continue
		}
		typ := strings.ToLower(m.Get("type").String() + m.Get("mediaType").String())
		ext := strings.ToLower(m.Get("extension").String() + m.Get("ext").String() + m.Get("format").String())
		isVideo := strings.Contains(typ, "video") || strings.Contains(ext, "mp4") ||
			(!strings.Contains(typ, "audio") && !strings.Contains(typ, "image"))
		if isVideo {
			// Prefer yang ada label HD/1080p
			q := strings.ToLower(m.Get("quality").String() + m.Get("label").String())
			if strings.Contains(q, "1080") || strings.Contains(q, "hd") {
				return u, title, nil
			}
			if best == "" {
				best = u
			}
		}
	}
	if best == "" {
		msg := parsed.Get("message").String()
		if msg == "" {
			msg = "no media found"
		}
		return "", "", fmt.Errorf("aio: %s", msg)
	}
	return best, title, nil
}

// downloadMedia mengunduh URL media memakai client milik engine.
// PENTING: CDN TikTok (v16-webapp-prime) menolak request tanpa cookie sesi
// ekstraksi (403). Memakai s.client yang cookie jar-nya sudah terisi dari
// mobile API / web SSR membuat download berhasil (HTTP 200).
func (s *TikTokSearchEngine) downloadMedia(ctx context.Context, urlStr string, maxBytes int64) ([]byte, error) {
	// Coba dengan beberapa set header (TikTok CDN kadang rewel soal header).
	headerSets := []map[string]string{
		{
			"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			"Referer":         "https://www.tiktok.com/",
			"Accept":          "*/*",
			"Accept-Language": "en-US,en;q=0.9",
			"Sec-Fetch-Dest":  "video",
			"Sec-Fetch-Mode":  "no-cors",
			"Sec-Fetch-Site":  "cross-site",
		},
		{
			"User-Agent":      "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			"Referer":         "https://www.tiktok.com/",
			"Accept":          "*/*",
			"Accept-Language": "en-US,en;q=0.9",
		},
	}

	var lastErr error
	for i, headers := range headerSets {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		statusCode := resp.StatusCode
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if statusCode == 200 {
			if maxBytes > 0 && int64(len(body)) > maxBytes {
				return nil, fmt.Errorf("file kebesaran (limit %d MB)", maxBytes/1024/1024)
			}
			return body, nil
		}
		lastErr = fmt.Errorf("HTTP %d", statusCode)
		// 403/429 → coba header set berikutnya; error lain langsung gagal
		if statusCode != 403 && statusCode != 429 && i == 0 {
			// Coba satu set lagi sebelum menyerah
			continue
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("gagal download")
	}
	return nil, lastErr
}

func (s *TikTokSearchEngine) enrichItem(ctx context.Context, targetURL string) (*TikTokVideoMetadata, error) {
	canonicalURL, videoID := s.resolveCanonicalInfo(ctx, targetURL)

	if videoID != "" {
		payload, err := s.extractViaMobileAPI(ctx, videoID, canonicalURL)
		if err == nil && payload != nil {
			return payload, nil
		}
	}

	ssrPayload, ssrErr := s.extractViaWebSSR(ctx, canonicalURL, videoID)
	if ssrErr == nil && ssrPayload != nil {
		return ssrPayload, nil
	}

	return nil, fmt.Errorf("metadata_extraction_failed")
}

func (s *TikTokSearchEngine) fetchSearchCards(ctx context.Context, keyword string, count int) ([]RawCardItem, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("window-size", "1920,1080"),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		// Egress proxy VM me-MITM TLS dengan CA runtime yang tidak dikenal
		// Chrome; abaikan validasi sertifikat untuk sesi scraping ini.
		chromedp.Flag("ignore-certificate-errors", true),
	)
	if ep := chromeHeadlessShellPath(); ep != "" {
		opts = append(opts, chromedp.ExecPath(ep))
	}
	// Chrome tidak bisa memakai kredensial proxy dari env secara andal;
	// lewatkan via relay localhost tanpa auth (menyuntik Proxy-Authorization).
	if ra := ensureProxyRelay(); ra != "" {
		opts = append(opts, chromedp.ProxyServer("http://"+ra))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	cdpCtx, cdpCancel := chromedp.NewContext(allocCtx)
	defer cdpCancel()

	stealthJS := `
		delete Object.getPrototypeOf(navigator).webdriver;
		Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		window.chrome = { runtime: {} };
		Object.defineProperty(navigator, 'plugins', { get: () => [1, 2, 3] });
		Object.defineProperty(navigator, 'languages', { get: () => ['en-US', 'en'] });
		const _getParameter = WebGLRenderingContext.prototype.getParameter;
		WebGLRenderingContext.prototype.getParameter = function(p) {
			if (p === 37445) return 'Intel Inc.';
			if (p === 37446) return 'Intel Iris OpenGL Engine';
			return _getParameter.apply(this, arguments);
		};
	`

	_, err := chromedp.Run(cdpCtx, chromedp.Steps(
		chromedp.Navigate("https://www.tiktok.com/"),
		chromedp.Sleep(1500*time.Millisecond),
	))
	if err != nil {
		return nil, err
	}

	_, _ = chromedp.Run(cdpCtx, chromedp.Evaluate[any](stealthJS))

	searchURL := "https://www.tiktok.com/search?q=" + url.QueryEscape(keyword)

	// Coba hingga 2x: navigasi → tunggu → scroll untuk trigger lazy-load →
	// ambil HTML. Kalau 0 hasil, reload sekali lalu coba lagi.
	var htmlContent string
	for attempt := 0; attempt < 2; attempt++ {
		_, err = chromedp.Run(cdpCtx, chromedp.Steps(
			chromedp.Navigate(searchURL),
			chromedp.Sleep(5*time.Second),
			// Scroll bertahap untuk memicu lazy-load hasil pencarian
			chromedp.Evaluate[chromedp.Void](`window.scrollBy(0, 800)`),
			chromedp.Sleep(1500*time.Millisecond),
			chromedp.Evaluate[chromedp.Void](`window.scrollBy(0, 800)`),
			chromedp.Sleep(1500*time.Millisecond),
		))
		if err != nil {
			return nil, err
		}

		htmlContent, _ = chromedp.Run(cdpCtx, chromedp.Evaluate[string](`document.documentElement.outerHTML`))

		// Cek cepat apakah ada hasil sebelum parsing penuh
		if strings.Contains(htmlContent, "search_top-item") || strings.Contains(htmlContent, "/video/") {
			break
		}
		// 0 hasil → tunggu sebentar lalu reload untuk attempt berikutnya
		time.Sleep(2 * time.Second)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return nil, err
	}

	var items []RawCardItem
	seen := make(map[string]bool)

	doc.Find("div[data-e2e='search_top-item']").Each(func(i int, sel *goquery.Selection) {
		link, _ := sel.Find("a[href*='/video/'], a[href*='/photo/']").Attr("href")
		if link == "" {
			return
		}
		if strings.HasPrefix(link, "/") {
			link = "https://www.tiktok.com" + link
		}
		if seen[link] {
			return
		}
		seen[link] = true

		views := strings.TrimSpace(sel.Find("[data-e2e='video-views']").Text())
		items = append(items, RawCardItem{
			URL:   link,
			Views: views,
		})
	})

	// Fallback: kalau selector utama 0 hasil (TikTok ganti DOM), cari semua
	// link /video/ dan /photo/ di halaman.
	if len(items) == 0 {
		doc.Find("a[href*='/video/'], a[href*='/photo/']").Each(func(i int, sel *goquery.Selection) {
			link, _ := sel.Attr("href")
			if link == "" || strings.Contains(link, "/search") {
				return
			}
			if strings.HasPrefix(link, "/") {
				link = "https://www.tiktok.com" + link
			}
			// Hanya ambil URL video/photo yang valid
			if !strings.Contains(link, "tiktok.com/@") {
				return
			}
			if seen[link] {
				return
			}
			seen[link] = true
			items = append(items, RawCardItem{URL: link})
		})
	}

	if len(items) > count {
		items = items[:count]
	}

	return items, nil
}

func (s *TikTokSearchEngine) Search(ctx context.Context, keyword string, count int) ExtractionResult {
	startTime := time.Now().UnixMilli()

	if count <= 0 {
		count = 10
	}

	rawItems, err := s.fetchSearchCards(ctx, keyword, count)
	if err != nil {
		return ExtractionResult{
			Success:   false,
			Error:     &ErrorDetail{Code: "SEARCH_DISPATCH_FAILED", Message: err.Error()},
			Timestamp: startTime,
		}
	}

	if len(rawItems) == 0 {
		return ExtractionResult{
			Success:    true,
			StatusCode: http.StatusOK,
			Data: SearchResponse{
				Keyword: keyword,
				Total:   0,
				Videos:  []TikTokVideoMetadata{},
			},
			Timestamp: startTime,
		}
	}

	finalVideos := make([]TikTokVideoMetadata, len(rawItems))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	for idx, it := range rawItems {
		wg.Add(1)
		go func(i int, item RawCardItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			meta, enrichErr := s.enrichItem(ctx, item.URL)
			if enrichErr == nil && meta != nil {
				meta.CardViews = item.Views
				finalVideos[i] = *meta
			} else {
				vidID := s.extractVideoID(item.URL)
				finalVideos[i] = TikTokVideoMetadata{
					ID:           vidID,
					CanonicalURL: item.URL,
					CardViews:    item.Views,
				}
			}
		}(idx, it)
	}
	wg.Wait()

	return ExtractionResult{
		Success:    true,
		StatusCode: http.StatusOK,
		Data: SearchResponse{
			Keyword: keyword,
			Total:   len(finalVideos),
			Videos:  finalVideos,
		},
		Timestamp: startTime,
	}
}
