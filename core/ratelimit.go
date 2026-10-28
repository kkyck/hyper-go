package main

/**
 * ⏱️ ratelimit.go — Per-user command rate limiter (anti-abuse)
 * ════════════════════════════════════════════════════════════
 * Sliding-window rate limiter keyed on phone (or JID fallback).
 *
 * Two buckets:
 *   - global: max N commands per minute from a single user (any command)
 *   - heavy:  max M commands per minute for expensive commands
 *     (AI, downloader, sticker maker, etc.)
 *
 * Owner is exempt (trusted).
 *
 * Storage: in-memory map; no persistence needed.
 */

import (
	"sync"
	"time"

	"github.com/polymorfa/hypermeow/types/events"
)

type rlBucket struct {
	timestamps []int64
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rlBucket
	max     int
	window  time.Duration
}

var (
	rlGlobal = newRateLimiter(30, 60*time.Second)  // 30 cmd/min/user
	rlHeavy  = newRateLimiter(10, 60*time.Second)  // 10 heavy cmd/min/user
	rlOwner  = newRateLimiter(120, 60*time.Second) // 120 cmd/min/owner (still capped)
)

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		buckets: map[string]*rlBucket{},
		max:     max,
		window:  window,
	}
}

// Allow returns true if the key is within the rate limit (and records the attempt).
// Returns false if the key is rate-limited.
func (rl *rateLimiter) Allow(key string) bool {
	if key == "" {
		key = "_anon"
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now().UnixMilli()
	windowMs := int64(rl.window / time.Millisecond)

	b, ok := rl.buckets[key]
	if !ok {
		b = &rlBucket{}
		rl.buckets[key] = b
	}
	// drop expired
	fresh := b.timestamps[:0]
	for _, t := range b.timestamps {
		if now-t < windowMs {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= rl.max {
		b.timestamps = fresh
		return false
	}
	fresh = append(fresh, now)
	b.timestamps = fresh
	return true
}

// rlUserKey picks the best key for the sender. Prefers phone (so PN/LID merge).
func rlUserKey(msg *events.Message) string {
	if msg == nil {
		return ""
	}
	// senderPhoneBest hits client; fallback to sender.User
	if botClient != nil {
		if ph := senderPhoneBest(botClient, msg); ph != "" {
			return ph
		}
	}
	if !msg.Info.Sender.IsEmpty() {
		return msg.Info.Sender.User
	}
	return "_anon"
}

// rlCheckGlobal returns (ok, retryAfterHumanReadable). Owner bypasses.
func rlCheckGlobal(msg *events.Message) (bool, string) {
	if isOwnerMsg(msg) {
		return true, ""
	}
	key := rlUserKey(msg)
	if !rlGlobal.Allow(key) {
		return false, "⏳ Terlalu banyak perintah. Tunggu sebentar (limit 30/menit)."
	}
	return true, ""
}

// rlCheckHeavy is called for expensive commands (AI, DL, maker).
func rlCheckHeavy(msg *events.Message) (bool, string) {
	if isOwnerMsg(msg) {
		return true, ""
	}
	key := rlUserKey(msg)
	if !rlHeavy.Allow(key) {
		return false, "⏳ Fitur berat terlalu sering dipakai. Tunggu sebentar (limit 10/menit)."
	}
	return true, ""
}

// isHeavyCmd returns true if the command hits external APIs (AI / downloader / maker).
// These are subject to a tighter per-user rate limit.
func isHeavyCmd(cmd string) bool {
	switch cmd {
	// AI
	case ".mistral", ".qwen", ".deepsek", ".deepseek", ".copilot", ".felo",
		".chatday", ".chatdeep", ".chatilm", ".deepai", ".deepai2",
		".quillbot", ".quillbotai":
		return true
	// Downloader
	case ".tt", ".tiktok", ".yt", ".youtube", ".ig", ".instagram":
		return true
	}
	return false
}
