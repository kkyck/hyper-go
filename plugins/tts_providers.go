package main

// TTS multi-provider dengan fallback chain.
// Prioritas: Edge TTS (neural, gratis, suara Indonesia natural) → Google Translate TTS.
// Edge TTS butuh WebSocket; jika gagal (misal proxy blokir), otomatis fallback ke gTTS.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ─── Provider 1: Edge TTS (Microsoft, gratis, tanpa key) ─────────────
// Suara Indonesia: id-ID-ArdiNeural (pria), id-ID-GadisNeural (wanita)

const edgeTTSURL = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1?TrustedClientToken=6A5AA1D4EAFF4B9FB9155C015B7C6F7"

func ttsEdge(text, voice string) ([]byte, error) {
	if voice == "" {
		voice = "id-ID-ArdiNeural"
	}
	// Batasi panjang teks per request (Edge limit ~ cukup besar, tapi amankan)
	if len(text) > 2000 {
		text = text[:2000]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dialOpts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0"},
			"Origin":     {"chrome-extension://jdiccldimpdaibmpdkjnbmckianbfoldhk"},
		},
	}
	conn, _, err := websocket.Dial(ctx, edgeTTSURL, dialOpts)
	if err != nil {
		return nil, fmt.Errorf("edge dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ts := time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000")

	// 1. Kirim config
	configMsg := fmt.Sprintf("X-Timestamp:%s\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n"+
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}`, ts)
	if err := conn.Write(ctx, websocket.MessageText, []byte(configMsg)); err != nil {
		return nil, fmt.Errorf("edge config: %w", err)
	}

	// 2. Kirim SSML
	ssml := fmt.Sprintf("<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='id-ID'>"+
		"<voice name='%s'><prosody rate='+0%%'>%s</prosody></voice></speak>",
		voice, xmlEscape(text))
	reqID := fmt.Sprintf("%x", time.Now().UnixNano())
	ssmlMsg := fmt.Sprintf("X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%s\r\nPath:ssml\r\n\r\n%s",
		reqID, ts, ssml)
	if err := conn.Write(ctx, websocket.MessageText, []byte(ssmlMsg)); err != nil {
		return nil, fmt.Errorf("edge ssml: %w", err)
	}

	// 3. Baca response audio
	var audio bytes.Buffer
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if msgType == websocket.MessageBinary {
			// Format: header\r\n\r\naudio-data ; cari "Path:audio"
			if idx := bytes.Index(data, []byte("Path:audio")); idx >= 0 && idx < 300 {
				if sep := bytes.Index(data, []byte("\r\n\r\n")); sep >= 0 {
					audio.Write(data[sep+4:])
				}
			}
		} else {
			if strings.Contains(string(data), "Path:turn.end") {
				break
			}
		}
	}

	if audio.Len() == 0 {
		return nil, fmt.Errorf("edge: no audio received")
	}
	return audio.Bytes(), nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"'", "&apos;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}

// ─── Provider 2: Google Translate TTS (fallback, selalu works) ───────

func ttsGoogle(text string) ([]byte, error) {
	// gTTS limit ~200 char per request; pecah jadi chunk
	var full bytes.Buffer
	for _, chunk := range splitTTSChunks(text, 180) {
		u := googleTTSBaseURL() + "/translate_tts?ie=UTF-8&q=" +
			url.QueryEscape(chunk) + "&tl=id&client=tw-ob"
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gtts request: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("gtts HTTP %d", resp.StatusCode)
		}
		full.Write(body)
	}
	return full.Bytes(), nil
}

func splitTTSChunks(s string, maxLen int) []string {
	var chunks []string
	words := strings.Fields(s)
	var cur strings.Builder
	for _, w := range words {
		if cur.Len()+len(w)+1 > maxLen && cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(w)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	if len(chunks) == 0 {
		chunks = []string{s}
	}
	return chunks
}

// ─── Entry point: coba Edge dulu, fallback ke Google ─────────────────

func ttsSynthesize(text string) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", fmt.Errorf("empty text")
	}

	// Coba Edge TTS dulu (kualitas neural terbaik)
	if audio, err := ttsEdge(text, "id-ID-ArdiNeural"); err == nil && len(audio) > 1000 {
		return audio, "edge", nil
	} else if err != nil {
		fmt.Printf("[TTS] edge failed (%v), fallback to google\n", err)
	}

	// Fallback: Google Translate TTS
	audio, err := ttsGoogle(text)
	if err != nil {
		return nil, "", fmt.Errorf("all TTS providers failed: %w", err)
	}
	return audio, "google", nil
}
