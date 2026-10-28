// dl_kangwifi.go — HTTP client untuk the aiodl-style download worker.
//
// Base URL comes from the DL_BASE_URL environment variable (no default).
// API contract:
//
//	POST /api/download       {url, quality, [fps], [codec]} → {id, job: {status:"running",...}}
//	GET  /api/job?id=<id>     → {id, status, lines:[{text,level}], progress:{done,total}|null, file, size, ...}
//	GET  /d?name=<n>           → bytes (video file)
//
// Field "lines" adalah array of object {text, level}.
// Field "progress" bisa null ATAU object {done, total} — gunakan json.RawMessage.
// Field "formats" bisa null ATAU object/array — gunakan json.RawMessage.
//
// STATUS VALUES: API aiodl inconsistent — gunakan English di response awal
// ("running") tapi Indonesia saat selesai ("selesai"). Kita accept keduanya.
//
// NETWORK: worker biasanya ada di belakang Cloudflare yang sering reset koneksi
// idle. HTTP transport dilimit IdleConnTimeout=10s (avoid stale conn) +
// retry transient error (reset by peer, EOF, timeout) up to 3x dengan
// backoff 2s.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dlKangwifiBase returns the configured download worker base URL.
//
// DL_BASE_URL is intentionally empty by default: the integration stays
// disabled until the operator points it at their own worker.
func dlKangwifiBase() string { return dlBaseURL() }

// dlKangwifiLine — satu baris log dari aiodl (ada text + level).
type dlKangwifiLine struct {
	Text  string `json:"text"`
	Level string `json:"level"` // head, dim, warn, dll.
}

// dlKangwifiJob merepresentasikan job aiodl (response dari /api/job dan /api/download).
type dlKangwifiJob struct {
	ID        string           `json:"id"`
	Input     string           `json:"input"`
	Provider  string           `json:"provider"`
	Status    string           `json:"status"` // running, completed, selesai, error, gagal
	Lines     []dlKangwifiLine `json:"lines"`
	Progress  json.RawMessage  `json:"progress"` // null OR {done,total}
	File      *string          `json:"file"`
	Size      int64            `json:"size"`
	Error     *string          `json:"error"`
	StartedAt int64            `json:"startedAt"`
	EndedAt   *int64           `json:"endedAt"`
	Formats   json.RawMessage  `json:"formats"`
}

type dlKangwifiStartResponse struct {
	ID  string        `json:"id"`
	Job dlKangwifiJob `json:"job"`
}

type dlKangwifiErrorResponse struct {
	Error string `json:"error"`
}

// dlKangwifiHTTPClient — shared HTTP client dengan transport yang diset untuk
// avoid Cloudflare idle connection reset issue:
//   - IdleConnTimeout=10s: koneksi idle lebih dari 10 detik di-evict dari pool,
//     jadi tidak akan dipakai ulang setelah CF menutup di sisi server (~30-60s).
//   - MaxIdleConns=2: pool kecil, jarang ada stale conn.
var dlKangwifiHTTPClient = &http.Client{
	Timeout: 90 * time.Second,
	Transport: &http.Transport{
		IdleConnTimeout:       10 * time.Second,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// dlKangwifiIsTransientError melaporkan apakah error ini transient (network
// glitch / CF reset / timeout). Error seperti ini boleh di-retry.
func dlKangwifiIsTransientError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "no route to host") ||
		strings.Contains(s, "request canceled") ||
		strings.Contains(s, "dial tcp") ||
		strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "connection refused")
}

// dlKangwifiStatusDone reports whether the status means "job complete successfully".
// API aiodl pakai "selesai" (Indonesia) untuk status sukses, tapi untuk amannya
// accept "completed" / "selesai" / "done" / "finished".
func dlKangwifiStatusDone(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "selesai", "done", "finished", "ok", "success":
		return true
	}
	return false
}

// dlKangwifiStatusFailed reports whether the status means "job failed".
// Accept "error" / "gagal" / "failed" / "fail".
func dlKangwifiStatusFailed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "gagal", "failed", "fail":
		return true
	}
	return false
}

// dlKangwifiStart POST /api/download untuk mulai job.
func dlKangwifiStart(ctx context.Context, videoURL, quality string) (string, error) {
	if base := dlKangwifiBase(); base == "" {
		return "", &envMissingError{Name: "DL_BASE_URL"}
	}
	if quality == "" {
		quality = "best"
	}
	body := map[string]any{
		"url":     videoURL,
		"quality": quality,
	}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", dlKangwifiBase()+"/api/download", strings.NewReader(string(bodyBytes)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hyper-go/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := dlKangwifiHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e dlKangwifiErrorResponse
		_ = json.Unmarshal(respBody, &e)
		if e.Error != "" {
			return "", fmt.Errorf("dl service: %s", e.Error)
		}
		return "", fmt.Errorf("dl service: HTTP %d", resp.StatusCode)
	}
	var sr dlKangwifiStartResponse
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return "", fmt.Errorf("dl service: parse response: %w", err)
	}
	if sr.ID == "" {
		var e dlKangwifiErrorResponse
		_ = json.Unmarshal(respBody, &e)
		if e.Error != "" {
			return "", fmt.Errorf("dl service: %s", e.Error)
		}
		raw := strings.TrimSpace(string(respBody))
		if len(raw) > 200 {
			raw = raw[:200]
		}
		return "", fmt.Errorf("dl service: empty job id (raw: %s)", raw)
	}
	return sr.ID, nil
}

// dlKangwifiPoll GET /api/job?id=<id> untuk poll status.
func dlKangwifiPoll(ctx context.Context, jobID string) (*dlKangwifiJob, error) {
	if base := dlKangwifiBase(); base == "" {
		return nil, &envMissingError{Name: "DL_BASE_URL"}
	}
	u := dlKangwifiBase() + "/api/job?id=" + url.QueryEscape(jobID)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hyper-go/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := dlKangwifiHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e dlKangwifiErrorResponse
		_ = json.Unmarshal(respBody, &e)
		if e.Error != "" {
			return nil, fmt.Errorf("dl service: %s", e.Error)
		}
		return nil, fmt.Errorf("dl service: HTTP %d", resp.StatusCode)
	}
	var job dlKangwifiJob
	if err := json.Unmarshal(respBody, &job); err != nil {
		return nil, fmt.Errorf("dl service: parse job: %w", err)
	}
	return &job, nil
}

// dlKangwifiPollWithRetry wraps dlKangwifiPoll with retry logic untuk
// transient network errors (connection reset, EOF, timeout). Cloudflare
// kadang reset koneksi idle, dan retry sederhana bisa recover.
// Max 3 attempts dengan backoff 2 detik.
func dlKangwifiPollWithRetry(ctx context.Context, jobID string) (*dlKangwifiJob, error) {
	const maxAttempts = 3
	const backoff = 2 * time.Second
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		job, err := dlKangwifiPoll(ctx, jobID)
		if err == nil {
			return job, nil
		}
		lastErr = err
		// Hanya retry transient errors. Permanent errors (4xx, parse) langsung return.
		if !dlKangwifiIsTransientError(err) {
			return nil, err
		}
		// Transient — tunggu backoff lalu retry
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("dl service: poll failed after %d retries: %w", maxAttempts, lastErr)
}

// dlKangwifiWaitDone poll tiap 1 detik sampai status = done atau failed.
// Pakai pollWithRetry untuk handle transient network errors (CF reset).
func dlKangwifiWaitDone(ctx context.Context, jobID string) (*dlKangwifiJob, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	// Poll pertama langsung
	job, err := dlKangwifiPollWithRetry(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if dlKangwifiStatusDone(job.Status) {
		return job, nil
	}
	if dlKangwifiStatusFailed(job.Status) || (job.Error != nil && *job.Error != "") {
		errMsg := "unknown error"
		if job.Error != nil {
			errMsg = *job.Error
		}
		return nil, fmt.Errorf("dl service: job error: %s", errMsg)
	}
	for {
		select {
		case <-ticker.C:
			job, err := dlKangwifiPollWithRetry(ctx, jobID)
			if err != nil {
				return nil, err
			}
			if dlKangwifiStatusDone(job.Status) {
				return job, nil
			}
			if dlKangwifiStatusFailed(job.Status) || (job.Error != nil && *job.Error != "") {
				errMsg := "unknown error"
				if job.Error != nil {
					errMsg = *job.Error
				}
				return nil, fmt.Errorf("dl service: job error: %s", errMsg)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// dlKangwifiFetchFile GET /d?name=<name> untuk download bytes file hasil.
// Returns (data, mimeType, error).
// 2026-10-20: route diperbaiki dari /api/file?name= (404) ke /d?name= (200).
func dlKangwifiFetchFile(ctx context.Context, name string, maxBytes int64) ([]byte, string, error) {
	if base := dlKangwifiBase(); base == "" {
		return nil, "", &envMissingError{Name: "DL_BASE_URL"}
	}
	u := dlKangwifiBase() + "/d?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "hyper-go/1.0")
	resp, err := dlKangwifiHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, "", fmt.Errorf("dl service: file HTTP %d (%s)", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, mime, err
	}
	if int64(len(data)) > maxBytes {
		return nil, mime, fmt.Errorf("dl service: file kebesaran (limit %d MB)", maxBytes/1024/1024)
	}
	return data, mime, nil
}

// dlKangwifiHeadTitle ambil judul dari lines[].text di mana level="head".
func dlKangwifiHeadTitle(job *dlKangwifiJob) string {
	if job == nil {
		return ""
	}
	for _, ln := range job.Lines {
		if ln.Level == "head" && ln.Text != "" {
			return ln.Text
		}
	}
	return ""
}

// dlKangwifiFull jalankan end-to-end: start job → wait completed → fetch file.
// Returns (data, mimeType, fileSize, title, error).
func dlKangwifiFull(parentCtx context.Context, videoURL, quality string, maxBytes int64) ([]byte, string, int64, string, error) {
	jobID, err := dlKangwifiStart(parentCtx, videoURL, quality)
	if err != nil {
		return nil, "", 0, "", err
	}
	job, err := dlKangwifiWaitDone(parentCtx, jobID)
	if err != nil {
		return nil, "", 0, "", err
	}
	if job.File == nil || *job.File == "" {
		return nil, "", 0, "", fmt.Errorf("dl service: job completed but no file produced (status=%s)", job.Status)
	}
	data, mime, err := dlKangwifiFetchFile(parentCtx, *job.File, maxBytes)
	if err != nil {
		return nil, "", 0, "", err
	}
	return data, mime, job.Size, dlKangwifiHeadTitle(job), nil
}
