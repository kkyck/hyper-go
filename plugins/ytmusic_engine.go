package main

// ytmusic_engine.go — YouTube Music search & audio-download engine (pure Go).
//
// Dipakai oleh .ytmp3:
//   .ytmp3 <judul> → Search (InnerTube music.youtube.com) → lagu pertama
//   .ytmp3 <url>   → Download langsung via video ID
//   → resolveDLStream (savenow.to API) → download MP3 → kirim sebagai audio.
//
// Catatan: ExtractionResult/ErrorDetail dipakai ulang dari tiktok_engine.go.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type ThumbnailItem struct {
	URL    string `json:"url"`
	Width  int64  `json:"width,omitempty"`
	Height int64  `json:"height,omitempty"`
}

type TrackItem struct {
	VideoID    string          `json:"video_id,omitempty"`
	Title      string          `json:"title"`
	Artist     string          `json:"artist,omitempty"`
	Album      string          `json:"album,omitempty"`
	Duration   string          `json:"duration,omitempty"`
	Thumbnails []ThumbnailItem `json:"thumbnails,omitempty"`
	URL        string          `json:"url,omitempty"`
}

type AlbumItem struct {
	BrowseID   string          `json:"browse_id,omitempty"`
	PlaylistID string          `json:"playlist_id,omitempty"`
	Title      string          `json:"title"`
	Artist     string          `json:"artist,omitempty"`
	Year       string          `json:"year,omitempty"`
	Thumbnails []ThumbnailItem `json:"thumbnails,omitempty"`
	URL        string          `json:"url,omitempty"`
}

type ArtistItem struct {
	BrowseID    string          `json:"browse_id,omitempty"`
	Name        string          `json:"name"`
	Subscribers string          `json:"subscribers,omitempty"`
	Thumbnails  []ThumbnailItem `json:"thumbnails,omitempty"`
	URL         string          `json:"url,omitempty"`
}

type PlaylistItem struct {
	BrowseID   string          `json:"browse_id,omitempty"`
	PlaylistID string          `json:"playlist_id,omitempty"`
	Title      string          `json:"title"`
	Author     string          `json:"author,omitempty"`
	ItemCount  string          `json:"item_count,omitempty"`
	Thumbnails []ThumbnailItem `json:"thumbnails,omitempty"`
	URL        string          `json:"url,omitempty"`
}

type SearchResult struct {
	Query     string         `json:"query"`
	TopResult any            `json:"top_result,omitempty"`
	Songs     []TrackItem    `json:"songs,omitempty"`
	Albums    []AlbumItem    `json:"albums,omitempty"`
	Artists   []ArtistItem   `json:"artists,omitempty"`
	Playlists []PlaylistItem `json:"playlists,omitempty"`
}

type AudioStreamItem struct {
	Format      string `json:"format"`
	Quality     string `json:"quality"`
	FileType    string `json:"file_type"`
	DownloadURL string `json:"download_url"`
	Status      string `json:"status"`
}

type PlayResult struct {
	VideoID      string            `json:"video_id"`
	Title        string            `json:"title"`
	Artist       string            `json:"artist,omitempty"`
	Album        string            `json:"album,omitempty"`
	Duration     string            `json:"duration,omitempty"`
	Thumbnails   []ThumbnailItem   `json:"thumbnails,omitempty"`
	CanonicalURL string            `json:"canonical_url"`
	Lyrics       string            `json:"lyrics,omitempty"`
	Streams      []AudioStreamItem `json:"streams,omitempty"`
	UpNext       []TrackItem       `json:"up_next,omitempty"`
}

type DownloadResult struct {
	VideoID      string            `json:"video_id"`
	Title        string            `json:"title"`
	CanonicalURL string            `json:"canonical_url"`
	Thumbnails   []ThumbnailItem   `json:"thumbnails,omitempty"`
	Streams      []AudioStreamItem `json:"streams"`
}

type AlbumResult struct {
	BrowseID   string          `json:"browse_id"`
	Title      string          `json:"title"`
	Artist     string          `json:"artist,omitempty"`
	Year       string          `json:"year,omitempty"`
	TrackCount int             `json:"track_count"`
	Thumbnails []ThumbnailItem `json:"thumbnails,omitempty"`
	Tracks     []TrackItem     `json:"tracks"`
}

type PlaylistResult struct {
	PlaylistID string          `json:"playlist_id"`
	Title      string          `json:"title"`
	Author     string          `json:"author,omitempty"`
	TrackCount int             `json:"track_count"`
	Thumbnails []ThumbnailItem `json:"thumbnails,omitempty"`
	Tracks     []TrackItem     `json:"tracks"`
}

type YTMusicEngine struct {
	client        *http.Client
	dlAPIKey      string
	primaryDLDom  string
	fallbackDLDom string
}

func NewYTMusicEngine(timeout time.Duration) (*YTMusicEngine, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
		Proxy:              http.ProxyFromEnvironment,
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: false,
		ForceAttemptHTTP2:  true,
	}

	client := &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   timeout,
	}

	return &YTMusicEngine{
		client:        client,
		dlAPIKey:      getenvDefault("YTMUSIC_DL_API_KEY", ""),
		primaryDLDom:  "p.savenow.to",
		fallbackDLDom: "p.lbserver.xyz",
	}, nil
}

func (y *YTMusicEngine) applyHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "id-ID,id;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Origin", "https://music.youtube.com")
	req.Header.Set("Referer", "https://music.youtube.com/")
	req.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
}

func (y *YTMusicEngine) postInnerTube(ctx context.Context, endpoint string, payload map[string]any) (gjson.Result, error) {
	fullURL := "https://music.youtube.com/youtubei/v1/" + strings.TrimPrefix(endpoint, "/")

	if _, ok := payload["context"]; !ok {
		payload["context"] = map[string]any{
			"client": map[string]any{
				"clientName":    "WEB_REMIX",
				"clientVersion": "1.20241001.01.00",
				"hl":            "id",
				"gl":            "ID",
			},
		}
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return gjson.Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return gjson.Result{}, err
	}
	y.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := y.client.Do(req)
	if err != nil {
		return gjson.Result{}, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return gjson.Result{}, err
	}

	return gjson.ParseBytes(respBytes), nil
}

func (y *YTMusicEngine) parseThumbnails(raw gjson.Result) []ThumbnailItem {
	var list []ThumbnailItem
	for _, item := range raw.Array() {
		u := item.Get("url").String()
		if u != "" {
			list = append(list, ThumbnailItem{
				URL:    u,
				Width:  item.Get("width").Int(),
				Height: item.Get("height").Int(),
			})
		}
	}
	return list
}

func (y *YTMusicEngine) Search(ctx context.Context, query string) (*SearchResult, error) {
	res, err := y.postInnerTube(ctx, "search", map[string]any{
		"query": query,
	})
	if err != nil {
		return nil, err
	}

	result := &SearchResult{
		Query: query,
	}

	sections := res.Get("contents.tabbedSearchResultsRenderer.tabs.0.tabRenderer.content.sectionListRenderer.contents").Array()
	for _, sec := range sections {
		if card := sec.Get("musicCardShelfRenderer"); card.Exists() {
			title := card.Get("title.runs.0.text").String()
			videoID := card.Get("title.runs.0.navigationEndpoint.watchEndpoint.videoId").String()
			subtitle := card.Get("subtitle.runs").Array()
			var subTexts []string
			for _, st := range subtitle {
				t := strings.TrimSpace(st.Get("text").String())
				if t != "" && t != "•" {
					subTexts = append(subTexts, t)
				}
			}

			topItem := map[string]any{
				"title":      title,
				"video_id":   videoID,
				"url":        "https://music.youtube.com/watch?v=" + videoID,
				"subtitles":  subTexts,
				"thumbnails": y.parseThumbnails(card.Get("thumbnail.musicThumbnailRenderer.thumbnail.thumbnails")),
			}
			result.TopResult = topItem
		}

		items := sec.Get("itemSectionRenderer.contents").Array()
		if len(items) == 0 {
			items = sec.Get("musicShelfRenderer.contents").Array()
		}

		for _, item := range items {
			renderer := item.Get("musicResponsiveListItemRenderer")
			if !renderer.Exists() {
				continue
			}

			title := renderer.Get("flexColumns.0.musicResponsiveListItemFlexColumnRenderer.text.runs.0.text").String()
			navEndpoint := renderer.Get("flexColumns.0.musicResponsiveListItemFlexColumnRenderer.text.runs.0.navigationEndpoint")
			watchVideoID := navEndpoint.Get("watchEndpoint.videoId").String()
			if watchVideoID == "" {
				watchVideoID = renderer.Get("playlistItemData.videoId").String()
			}
			browseID := navEndpoint.Get("browseEndpoint.browseId").String()

			subtitleRuns := renderer.Get("flexColumns.1.musicResponsiveListItemFlexColumnRenderer.text.runs").Array()
			var subParts []string
			for _, r := range subtitleRuns {
				t := strings.TrimSpace(r.Get("text").String())
				if t != "" && t != "•" {
					subParts = append(subParts, t)
				}
			}

			itemType := ""
			artistName := ""
			durationStr := ""

			if len(subParts) > 0 {
				itemType = strings.ToLower(subParts[0])
			}
			if len(subParts) > 1 {
				artistName = subParts[1]
			}
			if len(subParts) > 2 && regexp.MustCompile(`^\d+:\d+`).MatchString(subParts[len(subParts)-1]) {
				durationStr = subParts[len(subParts)-1]
			}

			thumbs := y.parseThumbnails(renderer.Get("thumbnail.musicThumbnailRenderer.thumbnail.thumbnails"))

			if watchVideoID != "" || itemType == "lagu" || itemType == "song" || itemType == "video" {
				result.Songs = append(result.Songs, TrackItem{
					VideoID:    watchVideoID,
					Title:      title,
					Artist:     artistName,
					Duration:   durationStr,
					Thumbnails: thumbs,
					URL:        "https://music.youtube.com/watch?v=" + watchVideoID,
				})
			} else if strings.HasPrefix(browseID, "MPREb") || itemType == "album" || itemType == "ep" || itemType == "single" {
				yearStr := ""
				if len(subParts) > 2 {
					yearStr = subParts[2]
				}
				result.Albums = append(result.Albums, AlbumItem{
					BrowseID:   browseID,
					Title:      title,
					Artist:     artistName,
					Year:       yearStr,
					Thumbnails: thumbs,
					URL:        "https://music.youtube.com/browse/" + browseID,
				})
			} else if itemType == "artis" || itemType == "artist" {
				subs := ""
				if len(subParts) > 1 {
					subs = subParts[1]
				}
				result.Artists = append(result.Artists, ArtistItem{
					BrowseID:    browseID,
					Name:        title,
					Subscribers: subs,
					Thumbnails:  thumbs,
					URL:         "https://music.youtube.com/browse/" + browseID,
				})
			} else if strings.HasPrefix(browseID, "VL") || itemType == "playlist" || itemType == "daftar putar" {
				author := artistName
				result.Playlists = append(result.Playlists, PlaylistItem{
					BrowseID:   browseID,
					PlaylistID: strings.TrimPrefix(browseID, "VL"),
					Title:      title,
					Author:     author,
					Thumbnails: thumbs,
					URL:        "https://music.youtube.com/playlist?list=" + strings.TrimPrefix(browseID, "VL"),
				})
			}
		}
	}

	return result, nil
}

func (y *YTMusicEngine) resolveDLStream(ctx context.Context, targetURL string, format string) (*AudioStreamItem, error) {
	formatQuality := "320kbps High Quality Audio"
	fileType := "mp3"
	if strings.ToLower(format) == "m4a" {
		formatQuality = "AAC M4A Audio"
		fileType = "m4a"
	} else if strings.ToLower(format) == "flac" {
		formatQuality = "Lossless Audio"
		fileType = "flac"
	} else if strings.ToLower(format) == "wav" {
		formatQuality = "Uncompressed Wave Audio"
		fileType = "wav"
	}

	q := url.Values{}
	q.Set("format", format)
	q.Set("url", targetURL)
	q.Set("apikey", y.dlAPIKey)

	domains := []string{y.primaryDLDom, y.fallbackDLDom}
	var initRes gjson.Result
	var lastErr error

	for _, d := range domains {
		ep := fmt.Sprintf("https://%s/api/v2/download?%s", d, q.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		req.Header.Set("Origin", ytLoaderBaseURL())
		req.Header.Set("Referer", ytLoaderBaseURL()+"/")

		resp, err := y.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		res := gjson.ParseBytes(b)
		if res.Get("success").Bool() || res.Get("progress_url").String() != "" {
			initRes = res
			break
		}
	}

	if !initRes.Exists() {
		return nil, fmt.Errorf("dl_service_init_failed: %v", lastErr)
	}

	progressURL := initRes.Get("progress_url").String()
	if progressURL == "" {
		progressURL = fmt.Sprintf("https://%s/api/v2/progress?id=%s", y.primaryDLDom, initRes.Get("id").String())
	}

	for i := 0; i < 25; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, progressURL, nil)
		if err == nil {
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Header.Set("Origin", ytLoaderBaseURL())
			req.Header.Set("Referer", ytLoaderBaseURL()+"/")
			resp, err := y.client.Do(req)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				pRes := gjson.ParseBytes(b)
				dlURL := pRes.Get("download_url").String()
				if pRes.Get("success").Int() == 1 && dlURL != "" {
					return &AudioStreamItem{
						Format:      format,
						Quality:     formatQuality,
						FileType:    fileType,
						DownloadURL: dlURL,
						Status:      "Available",
					}, nil
				}
			}
		}
		time.Sleep(1200 * time.Millisecond)
	}

	return nil, fmt.Errorf("dl_stream_generation_timeout")
}

func (y *YTMusicEngine) Play(ctx context.Context, videoID string) (*PlayResult, error) {
	cleanID := y.extractVideoID(videoID)
	if cleanID == "" {
		return nil, fmt.Errorf("invalid_video_id: %s", videoID)
	}

	nextRes, err := y.postInnerTube(ctx, "next", map[string]any{
		"videoId": cleanID,
	})
	if err != nil {
		return nil, err
	}

	tabs := nextRes.Get("contents.singleColumnMusicWatchNextResultsRenderer.tabbedRenderer.watchNextTabbedResultsRenderer.tabs").Array()
	var currentQueueItem gjson.Result
	var upNextList []TrackItem
	var lyricsBrowseID string

	for _, tab := range tabs {
		tabRenderer := tab.Get("tabRenderer")
		endpointID := tabRenderer.Get("endpoint.browseEndpoint.browseId").String()
		if strings.HasPrefix(endpointID, "MPLY") {
			lyricsBrowseID = endpointID
		}

		queueContents := tabRenderer.Get("content.musicQueueRenderer.content.playlistPanelRenderer.contents").Array()
		if len(queueContents) > 0 {
			currentQueueItem = queueContents[0].Get("playlistPanelVideoRenderer")
			for i := 1; i < len(queueContents) && i <= 15; i++ {
				qv := queueContents[i].Get("playlistPanelVideoRenderer")
				if qv.Exists() {
					upNextList = append(upNextList, TrackItem{
						VideoID:    qv.Get("videoId").String(),
						Title:      qv.Get("title.runs.0.text").String(),
						Artist:     qv.Get("longBylineText.runs.0.text").String(),
						Duration:   qv.Get("lengthText.runs.0.text").String(),
						Thumbnails: y.parseThumbnails(qv.Get("thumbnail.thumbnails")),
						URL:        "https://music.youtube.com/watch?v=" + qv.Get("videoId").String(),
					})
				}
			}
		}
	}

	title := currentQueueItem.Get("title.runs.0.text").String()
	artist := currentQueueItem.Get("longBylineText.runs.0.text").String()
	album := currentQueueItem.Get("longBylineText.runs.2.text").String()
	duration := currentQueueItem.Get("lengthText.runs.0.text").String()
	thumbnails := y.parseThumbnails(currentQueueItem.Get("thumbnail.thumbnails"))
	if len(thumbnails) == 0 {
		thumbnails = append(thumbnails, ThumbnailItem{
			URL: fmt.Sprintf("https://i.ytimg.com/vi/%s/hqdefault.jpg", cleanID),
		})
	}

	lyricsText := ""
	if lyricsBrowseID != "" {
		lyrRes, err := y.postInnerTube(ctx, "browse", map[string]any{
			"browseId": lyricsBrowseID,
		})
		if err == nil {
			lyricsText = lyrRes.Get("contents.sectionListRenderer.contents.0.musicDescriptionShelfRenderer.description.runs.0.text").String()
		}
	}

	var audioStreams []AudioStreamItem
	var wg sync.WaitGroup
	var mu sync.Mutex

	formats := []string{"mp3", "m4a"}
	for _, fmtStr := range formats {
		wg.Add(1)
		go func(f string) {
			defer wg.Done()
			stream, err := y.resolveDLStream(ctx, "https://music.youtube.com/watch?v="+cleanID, f)
			if err == nil && stream != nil {
				mu.Lock()
				audioStreams = append(audioStreams, *stream)
				mu.Unlock()
			}
		}(fmtStr)
	}
	wg.Wait()

	return &PlayResult{
		VideoID:      cleanID,
		Title:        title,
		Artist:       artist,
		Album:        album,
		Duration:     duration,
		Thumbnails:   thumbnails,
		CanonicalURL: "https://music.youtube.com/watch?v=" + cleanID,
		Lyrics:       lyricsText,
		Streams:      audioStreams,
		UpNext:       upNextList,
	}, nil
}

func (y *YTMusicEngine) Download(ctx context.Context, videoID string, requestedFormat string) (*DownloadResult, error) {
	cleanID := y.extractVideoID(videoID)
	if cleanID == "" {
		return nil, fmt.Errorf("invalid_video_id: %s", videoID)
	}

	targetURL := "https://music.youtube.com/watch?v=" + cleanID
	formats := []string{"mp3", "m4a"}
	if requestedFormat != "" {
		formats = []string{requestedFormat}
	}

	var streams []AudioStreamItem
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, f := range formats {
		wg.Add(1)
		go func(fmtCode string) {
			defer wg.Done()
			s, err := y.resolveDLStream(ctx, targetURL, fmtCode)
			if err == nil && s != nil {
				mu.Lock()
				streams = append(streams, *s)
				mu.Unlock()
			}
		}(f)
	}
	wg.Wait()

	if len(streams) == 0 {
		return nil, fmt.Errorf("audio_conversion_failed_for_all_formats")
	}

	return &DownloadResult{
		VideoID:      cleanID,
		Title:        "YouTube Music Audio Track",
		CanonicalURL: targetURL,
		Thumbnails: []ThumbnailItem{
			{URL: fmt.Sprintf("https://i.ytimg.com/vi/%s/hqdefault.jpg", cleanID)},
		},
		Streams: streams,
	}, nil
}

func (y *YTMusicEngine) Album(ctx context.Context, input string) (*AlbumResult, error) {
	browseID := y.extractAlbumID(input)
	if browseID == "" {
		return nil, fmt.Errorf("invalid_album_id_or_url: %s", input)
	}

	res, err := y.postInnerTube(ctx, "browse", map[string]any{
		"browseId": browseID,
	})
	if err != nil {
		return nil, err
	}

	header := res.Get("contents.twoColumnBrowseResultsRenderer.tabs.0.tabRenderer.content.sectionListRenderer.contents.0.musicResponsiveHeaderRenderer")
	title := header.Get("title.runs.0.text").String()
	if title == "" {
		title = res.Get("microformat.microformatDataRenderer.title").String()
	}

	artist := header.Get("straplineTextOne.runs.0.text").String()
	year := ""
	subRuns := header.Get("subtitle.runs").Array()
	for _, sr := range subRuns {
		t := strings.TrimSpace(sr.Get("text").String())
		if regexp.MustCompile(`^\d{4}$`).MatchString(t) {
			year = t
		}
	}

	thumbs := y.parseThumbnails(header.Get("thumbnail.musicThumbnailRenderer.thumbnail.thumbnails"))
	trackShelf := res.Get("contents.twoColumnBrowseResultsRenderer.secondaryContents.sectionListRenderer.contents.0.musicShelfRenderer.contents").Array()
	if len(trackShelf) == 0 {
		trackShelf = res.Get("contents.twoColumnBrowseResultsRenderer.secondaryContents.sectionListRenderer.contents.0.musicPlaylistShelfRenderer.contents").Array()
	}

	var tracks []TrackItem
	for _, tr := range trackShelf {
		rend := tr.Get("musicResponsiveListItemRenderer")
		if !rend.Exists() {
			continue
		}

		tTitle := rend.Get("flexColumns.0.musicResponsiveListItemFlexColumnRenderer.text.runs.0.text").String()
		tVid := rend.Get("playlistItemData.videoId").String()
		if tVid == "" {
			tVid = rend.Get("flexColumns.0.musicResponsiveListItemFlexColumnRenderer.text.runs.0.navigationEndpoint.watchEndpoint.videoId").String()
		}

		tArtist := rend.Get("flexColumns.1.musicResponsiveListItemFlexColumnRenderer.text.runs.0.text").String()
		if tArtist == "" {
			tArtist = artist
		}

		tDur := rend.Get("fixedColumns.0.musicResponsiveListItemFixedColumnRenderer.text.runs.0.text").String()
		tThumbs := y.parseThumbnails(rend.Get("thumbnail.musicThumbnailRenderer.thumbnail.thumbnails"))

		tracks = append(tracks, TrackItem{
			VideoID:    tVid,
			Title:      tTitle,
			Artist:     tArtist,
			Album:      title,
			Duration:   tDur,
			Thumbnails: tThumbs,
			URL:        "https://music.youtube.com/watch?v=" + tVid,
		})
	}

	return &AlbumResult{
		BrowseID:   browseID,
		Title:      title,
		Artist:     artist,
		Year:       year,
		TrackCount: len(tracks),
		Thumbnails: thumbs,
		Tracks:     tracks,
	}, nil
}

func (y *YTMusicEngine) Playlist(ctx context.Context, input string) (*PlaylistResult, error) {
	playlistID := y.extractPlaylistID(input)
	if playlistID == "" {
		return nil, fmt.Errorf("invalid_playlist_id_or_url: %s", input)
	}

	browseID := "VL" + playlistID
	res, err := y.postInnerTube(ctx, "browse", map[string]any{
		"browseId": browseID,
	})
	if err != nil {
		return nil, err
	}

	header := res.Get("contents.twoColumnBrowseResultsRenderer.tabs.0.tabRenderer.content.sectionListRenderer.contents.0.musicResponsiveHeaderRenderer")
	title := header.Get("title.runs.0.text").String()
	if title == "" {
		title = res.Get("microformat.microformatDataRenderer.title").String()
	}

	author := header.Get("straplineTextOne.runs.0.text").String()
	thumbs := y.parseThumbnails(header.Get("thumbnail.musicThumbnailRenderer.thumbnail.thumbnails"))

	shelf := res.Get("contents.twoColumnBrowseResultsRenderer.secondaryContents.sectionListRenderer.contents.0.musicPlaylistShelfRenderer.contents").Array()
	if len(shelf) == 0 {
		shelf = res.Get("contents.twoColumnBrowseResultsRenderer.secondaryContents.sectionListRenderer.contents.0.musicShelfRenderer.contents").Array()
	}

	var tracks []TrackItem
	for _, tr := range shelf {
		rend := tr.Get("musicResponsiveListItemRenderer")
		if !rend.Exists() {
			continue
		}

		tTitle := rend.Get("flexColumns.0.musicResponsiveListItemFlexColumnRenderer.text.runs.0.text").String()
		tVid := rend.Get("playlistItemData.videoId").String()
		if tVid == "" {
			tVid = rend.Get("flexColumns.0.musicResponsiveListItemFlexColumnRenderer.text.runs.0.navigationEndpoint.watchEndpoint.videoId").String()
		}

		tArtist := rend.Get("flexColumns.1.musicResponsiveListItemFlexColumnRenderer.text.runs.0.text").String()
		tAlbum := rend.Get("flexColumns.2.musicResponsiveListItemFlexColumnRenderer.text.runs.0.text").String()
		tDur := rend.Get("fixedColumns.0.musicResponsiveListItemFixedColumnRenderer.text.runs.0.text").String()
		tThumbs := y.parseThumbnails(rend.Get("thumbnail.musicThumbnailRenderer.thumbnail.thumbnails"))

		tracks = append(tracks, TrackItem{
			VideoID:    tVid,
			Title:      tTitle,
			Artist:     tArtist,
			Album:      tAlbum,
			Duration:   tDur,
			Thumbnails: tThumbs,
			URL:        "https://music.youtube.com/watch?v=" + tVid,
		})
	}

	return &PlaylistResult{
		PlaylistID: playlistID,
		Title:      title,
		Author:     author,
		TrackCount: len(tracks),
		Thumbnails: thumbs,
		Tracks:     tracks,
	}, nil
}

func (y *YTMusicEngine) extractVideoID(raw string) string {
	clean := strings.TrimSpace(raw)
	if len(clean) == 11 && regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`).MatchString(clean) {
		return clean
	}
	reV := regexp.MustCompile(`[?&]v=([a-zA-Z0-9_-]{11})`)
	if m := reV.FindStringSubmatch(clean); len(m) > 1 {
		return m[1]
	}
	reShort := regexp.MustCompile(`youtu\.be/([a-zA-Z0-9_-]{11})`)
	if m := reShort.FindStringSubmatch(clean); len(m) > 1 {
		return m[1]
	}
	return clean
}

func (y *YTMusicEngine) extractPlaylistID(raw string) string {
	clean := strings.TrimSpace(raw)
	if strings.HasPrefix(clean, "VL") {
		return strings.TrimPrefix(clean, "VL")
	}
	reList := regexp.MustCompile(`[?&]list=([a-zA-Z0-9_-]+)`)
	if m := reList.FindStringSubmatch(clean); len(m) > 1 {
		return m[1]
	}
	return clean
}

func (y *YTMusicEngine) extractAlbumID(raw string) string {
	clean := strings.TrimSpace(raw)
	if strings.HasPrefix(clean, "MPREb") {
		return clean
	}
	reBrowse := regexp.MustCompile(`/browse/(MPREb[a-zA-Z0-9_-]+)`)
	if m := reBrowse.FindStringSubmatch(clean); len(m) > 1 {
		return m[1]
	}
	reList := regexp.MustCompile(`[?&]list=(OLAK5uy_[a-zA-Z0-9_-]+)`)
	if m := reList.FindStringSubmatch(clean); len(m) > 1 {
		return "VL" + m[1]
	}
	if strings.HasPrefix(clean, "OLAK5uy_") {
		return "VL" + clean
	}
	return clean
}
