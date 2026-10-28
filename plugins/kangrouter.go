package main

// kangrouter.go — AI backend chain + Gemini circuit breaker (cooldown)
// ════════════════════════════════════════════════════════════
//
// Chain: Gemini → Pollinations (langsung, tanpa kangwifi API).
// KangRouter sudah dihapus total sesuai permintaan user.
//
// ─── Gemini Circuit Breaker ────────────────────────────────────
// When Gemini errors EVEN ONCE, the circuit breaker trips and Gemini
// enters cooldown mode. During cooldown:
//   - All AI requests skip Gemini entirely and go directly to KangRouter
//   - This prevents wasting time on a known-failing backend
//   - After cooldown expires, Gemini is given ONE "probe" request
//     - If probe succeeds → breaker resets, Gemini becomes primary again
//     - If probe fails → cooldown extends exponentially (5, 10, 20, 40 min)
//       capped at 60 min
//
// This ensures: "kalau gemini eror 1 kali maka jangan pake selama beberapa
// saat sampe benar-benar pulih" — exactly as the owner requested.
//
// ─── Fallback Chain (v2.77) ────────────────────────────────────
//   1. Gemini Free (geminiFreeAsk) — primary, with circuit breaker
//      ↓ if cooldown tripped or request fails
//   2. KangRouter (kangRouterAsk) — powerful fallback, opencode/big-pickle
//      ↓ if KangRouter fails
//   3. Pollinations (pollinationsTextAsk) — 3rd, no key required
//      ↓ if Pollinations fails
//   4. kwAIChat (kangwifi) — last resort, often unreliable
//
// AI capability is NOT reduced at all — KangRouter uses the same
// prompt (including system prompt + conversation history) and returns
// full responses with proper formatting (bold, etc.).

import (
	"fmt"
	"sync"
	"time"
)

// kangRouterAsk sends a prompt to KangRouter (OpenAI-compatible API).
//
// The prompt is treated as a single user message preserving the full
// system prompt + conversation history embedded in the prompt text.
// If the prompt contains "assistant: " marker, we split it into
// system + user roles — this ensures AI capability is NOT reduced.
//
// Returns the response text with **bold** converted to *bold*
// (WhatsApp markdown format).

// ─── Gemini Circuit Breaker ─────────────────────────────────────
//
// When Gemini errors 1 time, trip the breaker → cooldown.
// During cooldown: skip Gemini entirely, use KangRouter directly.
// After cooldown: probe Gemini once. Success → reset. Failure → extend.
//
// Cooldown progression (exponential backoff):
//   1st failure: 5 min
//   2nd failure: 10 min
//   3rd failure: 20 min
//   4th failure: 40 min
//   5th+ failure: 60 min (cap)

var (
	geminiCBMu        sync.Mutex
	geminiCBCooldownT time.Time // zero = healthy; non-zero = in cooldown
	geminiCBFailCount int       // consecutive failure count (drives exponential backoff)
)

// Gemini circuit breaker configuration
const (
	geminiCBInitialCooldown = 5 * time.Minute
	geminiCBMaxCooldown     = 60 * time.Minute
)

// geminiCBInCooldown returns true if Gemini is currently in cooldown
// (i.e., should be skipped, request should go directly to KangRouter).
func geminiCBInCooldown() bool {
	geminiCBMu.Lock()
	defer geminiCBMu.Unlock()
	if geminiCBCooldownT.IsZero() {
		return false
	}
	// Check if cooldown expired
	if time.Now().After(geminiCBCooldownT) {
		// Cooldown expired — clear it (next call will probe Gemini)
		geminiCBCooldownT = time.Time{}
		return false
	}
	return true
}

// geminiCBRemaining returns human-readable remaining cooldown duration.
// Returns "0s" if not in cooldown.
func geminiCBRemaining() string {
	geminiCBMu.Lock()
	defer geminiCBMu.Unlock()
	if geminiCBCooldownT.IsZero() || time.Now().After(geminiCBCooldownT) {
		return "0s"
	}
	d := time.Until(geminiCBCooldownT)
	if d < 0 {
		return "0s"
	}
	// Round to nearest second
	return d.Truncate(time.Second).String()
}

// geminiCBMarkFailed trips the circuit breaker.
// Called when a Gemini request fails (any reason: timeout, HTTP error, parse error).
// Increases consecutive failure count and exponentially extends cooldown.
func geminiCBMarkFailed() {
	geminiCBMu.Lock()
	defer geminiCBMu.Unlock()

	geminiCBFailCount++

	// Exponential backoff: 5, 10, 20, 40, 60 (cap)
	dur := geminiCBInitialCooldown
	for i := 1; i < geminiCBFailCount; i++ {
		dur *= 2
		if dur > geminiCBMaxCooldown {
			dur = geminiCBMaxCooldown
			break
		}
	}

	geminiCBCooldownT = time.Now().Add(dur)
	fmt.Printf("[GEMINI-CB] tripped — cooldown %v (consecutive failures: %d)\n",
		dur, geminiCBFailCount)
}

// geminiCBMarkSuccess resets the circuit breaker.
// Called when a Gemini request succeeds (after a probe or normal call).
func geminiCBMarkSuccess() {
	geminiCBMu.Lock()
	defer geminiCBMu.Unlock()
	if geminiCBFailCount > 0 {
		fmt.Printf("[GEMINI-CB] recovered after %d consecutive failures\n",
			geminiCBFailCount)
	}
	geminiCBFailCount = 0
	geminiCBCooldownT = time.Time{}
}

// geminiCBStatus returns a one-line status string for logging/debug commands.
func geminiCBStatus() string {
	geminiCBMu.Lock()
	defer geminiCBMu.Unlock()
	if geminiCBCooldownT.IsZero() {
		return fmt.Sprintf("healthy (consecutive_fail=0)")
	}
	rem := time.Until(geminiCBCooldownT)
	if rem < 0 {
		return fmt.Sprintf("healthy (cooldown just expired, fail_count=%d)", geminiCBFailCount)
	}
	return fmt.Sprintf("cooldown %v remaining (fail_count=%d)",
		rem.Truncate(time.Second), geminiCBFailCount)
}

// ─── Unified AI backend selector ─────────────────────────────────
//
// aiBackendAsk is the unified AI request entry point.
// It implements the v2.77 fallback chain:
//
//  1. Gemini (if not in cooldown) — primary
//     ↓ on cooldown or failure
//  2. KangRouter — powerful fallback (opencode/big-pickle)
//     ↓ on failure
//  3. Pollinations — no key required
//     ↓ on failure
//  4. kwAIChat — last resort
//
// Returns (responseText, backendName, error).
func aiBackendAsk(prompt string) (string, string, error) {
	// ─── Step 1: Gemini (if not in cooldown) ──────────────────
	if !geminiCBInCooldown() {
		resp, _, err := geminiFreeAsk(prompt, "")
		if err == nil {
			geminiCBMarkSuccess()
			return resp, "gemini", nil
		}
		// Gemini failed — trip the circuit breaker
		fmt.Printf("[AI-BACKEND] gemini error: %v — tripping circuit breaker\n", err)
		geminiCBMarkFailed()
	} else {
		fmt.Printf("[AI-BACKEND] gemini in cooldown (%s) — skipping to pollinations\n",
			geminiCBRemaining())
	}

	// ─── Step 2: Pollinations (langsung, tanpa kangwifi) ──────
	resp, err := pollinationsTextAsk(prompt)
	if err == nil {
		return resp, "pollinations", nil
	}
	fmt.Printf("[AI-BACKEND] pollinations error: %v — all AI backends failed\n", err)

	return "", "", fmt.Errorf("all AI backends failed: %w", err)
}
