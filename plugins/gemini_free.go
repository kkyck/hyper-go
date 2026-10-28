package main

// gemini_free.go — Free Gemini AI via web interface (no API key, no limits)
// ════════════════════════════════════════════════════════════
//
// v2.42 — Robust Gemini with:
//   - Cookie caching (10 min TTL, reuse across calls)
//   - bl parameter auto-fetch from the Gemini web app page
//   - Retry on failure (up to 3 attempts with fresh cookie)
//   - Multiple cookie selection (prefer __Secure-ENID, fallback NID)
//   - Better response parsing (handle different line counts)
//   - Timeout 90s (was 30s, caused 'context deadline exceeded')
//
// CARA KERJA:
// 1. Get cached cookie OR fetch fresh from $GEMINI_BASE (HEAD request)
// 2. POST prompt ke StreamGenerate endpoint
// 3. Parse response (fragile JSON nesting, multiple fallback paths)
// 4. Maintain session via base64-encoded cookie + resumeArray
//
// FALLBACK CHAIN (in ai_agentic.go):
//   1. geminiFreeAsk (this file) — primary
//   2. pollinationsTextAsk — 2nd
//   3. kwAIChat — 3rd (last resort, often 404)
//   4. Return natural error message

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// geminiFreeHTTP is the shared HTTP client.
// Timeout: 90s — Gemini's StreamGenerate can be slow on cold start.
var geminiFreeHTTP = &http.Client{
	Timeout: 90 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ─── Cookie caching ────────────────────────────────────────────────

type geminiCookieCache struct {
	Cookie    string
	FetchedAt time.Time
}

var (
	geminiCookieMu    sync.Mutex
	geminiCookieC     *geminiCookieCache
	geminiCookieTTL   = 10 * time.Minute // cache cookie for 10 min
	geminiBLCache     string
	geminiBLFetchedAt time.Time
	geminiBLMu        sync.Mutex
)

// geminiFreeGetCookie fetches a fresh cookie from the Gemini web app.
// Uses cached cookie if available and fresh (< 10 min old).
// Falls back to multiple cookie selection if first cookie is empty.
func geminiFreeGetCookie() (string, error) {
	geminiCookieMu.Lock()
	if geminiCookieC != nil && time.Since(geminiCookieC.FetchedAt) < geminiCookieTTL {
		c := geminiCookieC.Cookie
		geminiCookieMu.Unlock()
		return c, nil
	}
	geminiCookieMu.Unlock()

	// Fetch fresh cookie
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", geminiBaseURL()+"/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := geminiFreeHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("cookie request: %w", err)
	}
	defer resp.Body.Close()

	cookies := resp.Header["Set-Cookie"]
	if len(cookies) == 0 {
		return "", fmt.Errorf("no Set-Cookie header in response")
	}

	// Build full cookie string from all Set-Cookie headers
	// Prefer __Secure-ENID, NID, __Secure-1PSID, __Secure-3PSID
	var cookieParts []string
	for _, c := range cookies {
		idx := strings.Index(c, ";")
		if idx > 0 {
			c = c[:idx]
		}
		c = strings.TrimSpace(c)
		if c != "" {
			cookieParts = append(cookieParts, c)
		}
	}
	if len(cookieParts) == 0 {
		return "", fmt.Errorf("empty cookie values")
	}

	// Join all cookies with "; " — send them all
	fullCookie := strings.Join(cookieParts, "; ")

	// Cache it
	geminiCookieMu.Lock()
	geminiCookieC = &geminiCookieCache{
		Cookie:    fullCookie,
		FetchedAt: time.Now(),
	}
	geminiCookieMu.Unlock()

	return fullCookie, nil
}

// geminiFreeInvalidateCookie clears the cached cookie (force refresh on next call).
func geminiFreeInvalidateCookie() {
	geminiCookieMu.Lock()
	geminiCookieC = nil
	geminiCookieMu.Unlock()
}

// geminiFreeGetBL fetches the bl parameter from the Gemini web app page HTML.
// Falls back to hardcoded value if fetch fails.
// bl = boq_assistant-bard-web-server_YYYYMMDD.NN_pN
func geminiFreeGetBL() string {
	geminiBLMu.Lock()
	defer geminiBLMu.Unlock()

	// Cache for 1 hour
	if geminiBLCache != "" && time.Since(geminiBLFetchedAt) < time.Hour {
		return geminiBLCache
	}

	// Try to fetch from page
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", geminiBaseURL()+"/app", nil)
	if err == nil {
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")

		resp, err := geminiFreeHTTP.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB max
				if err == nil {
					// Look for bl= pattern in HTML/JS
					// Pattern: "boq_assistant-bard-web-server_YYYYMMDD.NN_pN"
					re := regexp.MustCompile(`boq_assistant-bard-web-server_\d{8}\.\d+_p\d+`)
					if match := re.FindString(string(body)); match != "" {
						geminiBLCache = match
						geminiBLFetchedAt = time.Now()
						fmt.Printf("[GEMINI] bl parameter auto-fetched: %s\n", match)
						return match
					}
				}
			}
		}
	}

	// Fallback to hardcoded (update periodically)
	fallback := "boq_assistant-bard-web-server_20260917.13_p0"
	if geminiBLCache == "" {
		geminiBLCache = fallback
		geminiBLFetchedAt = time.Now()
	}
	return geminiBLCache
}

// geminiFreeAsk sends a prompt to Gemini with retry logic.
// On failure, invalidates cookie + retries with fresh cookie (up to 3 attempts).
func geminiFreeAsk(prompt, prevSession string) (string, string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		resp, session, err := geminiFreeAskOnce(prompt, prevSession)
		if err == nil {
			return resp, session, nil
		}
		lastErr = err
		fmt.Printf("[GEMINI] attempt %d failed: %v\n", attempt, err)
		// Invalidate cookie + retry
		geminiFreeInvalidateCookie()
		// Also clear prevSession (maybe session expired)
		prevSession = ""
		// Small delay before retry
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	return "", "", fmt.Errorf("after 3 attempts: %w", lastErr)
}

// geminiFreeAskOnce does a single Gemini request (no retry).
func geminiFreeAskOnce(prompt, prevSession string) (string, string, error) {
	var resumeArray []interface{}
	var cookie string

	// Parse previous session if provided
	if prevSession != "" {
		sessionBytes, err := base64.StdEncoding.DecodeString(prevSession)
		if err == nil {
			var sessionObj struct {
				Cookie      string        `json:"cookie"`
				ResumeArray []interface{} `json:"resumeArray"`
			}
			if json.Unmarshal(sessionBytes, &sessionObj) == nil {
				cookie = sessionObj.Cookie
				resumeArray = sessionObj.ResumeArray
			}
		}
	}

	if cookie == "" {
		c, err := geminiFreeGetCookie()
		if err != nil {
			return "", "", fmt.Errorf("get cookie: %w", err)
		}
		cookie = c
		resumeArray = []interface{}{}
	}

	// Build request body
	innerReq := []interface{}{
		[]string{prompt},
		[]string{"en-US"},
		resumeArray,
	}
	innerJSON, err := json.Marshal(innerReq)
	if err != nil {
		return "", "", err
	}
	outerReq := []interface{}{
		nil,
		string(innerJSON),
	}
	outerJSON, err := json.Marshal(outerReq)
	if err != nil {
		return "", "", err
	}

	body := "f.req=" + urlEscape(string(outerJSON))

	// Build StreamGenerate URL with auto-fetched bl parameter
	bl := geminiFreeGetBL()
	streamURL := geminiBaseURL() + "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate" +
		"?bl=" + bl +
		"&hl=en-US" +
		"&_reqid=" + fmt.Sprintf("%d", time.Now().UnixMilli()%1000000) +
		"&rt=c"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", streamURL, strings.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Origin", geminiBaseURL())
	req.Header.Set("Referer", geminiBaseURL()+"/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := geminiFreeHTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	// Parse response — try multiple strategies since line count varies
	responseText, newSession, err := geminiFreeParseResponse(respBody, cookie)
	if err != nil {
		return "", "", err
	}

	// Convert **bold** to *bold* (WhatsApp markdown)
	responseText = strings.ReplaceAll(responseText, "**", "*")

	return responseText, newSession, nil
}

// geminiFreeParseResponse parses the Gemini StreamGenerate response.
// Tries multiple line positions since the format can vary.
func geminiFreeParseResponse(respBody []byte, cookie string) (string, string, error) {
	bodyStr := strings.TrimSpace(string(respBody))
	lines := strings.Split(bodyStr, "\n")

	// Strategy 1: Try line at position -11 (original logic)
	// Strategy 2: Try line at position -2 (sometimes used)
	// Strategy 3: Scan all lines for valid JSON arrays
	tryPositions := []int{-11, -2, -1, -3, -4, -5}

	for _, pos := range tryPositions {
		idx := len(lines) + pos
		if idx < 0 || idx >= len(lines) {
			continue
		}
		s1 := lines[idx]
		if s1 == "" || s1 == ")]}'" {
			continue
		}

		text, session, err := geminiFreeParseLine(s1, cookie)
		if err == nil {
			return text, session, nil
		}
	}

	// Strategy 4: Scan all lines for any valid JSON with response text
	for _, line := range lines {
		if line == "" || line == ")]}'" {
			continue
		}
		text, session, err := geminiFreeParseLine(line, cookie)
		if err == nil && text != "" {
			return text, session, nil
		}
	}

	return "", "", fmt.Errorf("could not parse response: %d lines, first 200 chars: %s",
		len(lines), truncateStr(bodyStr, 200))
}

// geminiFreeParseLine parses a single JSON line from the response.
func geminiFreeParseLine(line, cookie string) (string, string, error) {
	// First JSON parse: outer array
	var o1 []interface{}
	if err := json.Unmarshal([]byte(line), &o1); err != nil {
		return "", "", fmt.Errorf("parse outer JSON: %w", err)
	}
	if len(o1) < 1 {
		return "", "", fmt.Errorf("outer array empty")
	}

	// o1[0] should be an array, [2] is a JSON string
	o1Arr, ok := o1[0].([]interface{})
	if !ok || len(o1Arr) < 3 {
		return "", "", fmt.Errorf("outer[0] not array or too short")
	}
	innerStr, ok := o1Arr[2].(string)
	if !ok {
		return "", "", fmt.Errorf("outer[0][2] not string")
	}

	// Second JSON parse: inner array
	var a1 []interface{}
	if err := json.Unmarshal([]byte(innerStr), &a1); err != nil {
		return "", "", fmt.Errorf("parse inner JSON: %w", err)
	}

	// a1[4][0][1][0] is the response text
	responseText, err := geminiFreeExtractString(a1, 4, 0, 1, 0)
	if err != nil {
		return "", "", fmt.Errorf("extract response text: %w", err)
	}

	// Build new session
	c, _ := geminiFreeExtractValue(a1, 1, 0)
	r, _ := geminiFreeExtractValue(a1, 1, 1)
	rc, _ := geminiFreeExtractValue(a1, 4, 0, 0)
	newResumeArray := []interface{}{c, r, rc}

	sessionObj := map[string]interface{}{
		"cookie":      cookie,
		"resumeArray": newResumeArray,
	}
	sessionJSON, _ := json.Marshal(sessionObj)
	newSession := base64.StdEncoding.EncodeToString(sessionJSON)

	return responseText, newSession, nil
}

// geminiFreeExtractString navigates nested JSON arrays to extract a string.
func geminiFreeExtractString(a1 []interface{}, path ...int) (string, error) {
	current := interface{}(a1)
	for _, idx := range path {
		arr, ok := current.([]interface{})
		if !ok {
			return "", fmt.Errorf("not array at index %d", idx)
		}
		if idx >= len(arr) {
			return "", fmt.Errorf("index %d out of range", idx)
		}
		current = arr[idx]
	}
	str, ok := current.(string)
	if !ok {
		return "", fmt.Errorf("not string at final position")
	}
	return str, nil
}

// geminiFreeExtractValue navigates nested JSON arrays to extract any value.
func geminiFreeExtractValue(a1 []interface{}, path ...int) (interface{}, error) {
	current := interface{}(a1)
	for _, idx := range path {
		arr, ok := current.([]interface{})
		if !ok {
			return nil, fmt.Errorf("not array")
		}
		if idx >= len(arr) {
			return nil, fmt.Errorf("index out of range")
		}
		current = arr[idx]
	}
	return current, nil
}

// ─── Per-user session storage ─────────────────────────────────────

var (
	geminiFreeSessionMu sync.Mutex
	geminiFreeSessions  = map[string]string{} // userKey → base64 session
)

// geminiFreeAskWithMemory asks Gemini with per-user session memory.
func geminiFreeAskWithMemory(userKey, prompt string) (string, error) {
	geminiFreeSessionMu.Lock()
	prevSession := geminiFreeSessions[userKey]
	geminiFreeSessionMu.Unlock()

	response, newSession, err := geminiFreeAsk(prompt, prevSession)
	if err != nil {
		return "", err
	}

	geminiFreeSessionMu.Lock()
	geminiFreeSessions[userKey] = newSession
	geminiFreeSessionMu.Unlock()

	return response, nil
}

// geminiFreeClearSession clears conversation memory for a user.
func geminiFreeClearSession(userKey string) bool {
	geminiFreeSessionMu.Lock()
	defer geminiFreeSessionMu.Unlock()
	_, existed := geminiFreeSessions[userKey]
	delete(geminiFreeSessions, userKey)
	return existed
}

// geminiFreeSessionCount returns number of active sessions.
func geminiFreeSessionCount() int {
	geminiFreeSessionMu.Lock()
	defer geminiFreeSessionMu.Unlock()
	return len(geminiFreeSessions)
}
