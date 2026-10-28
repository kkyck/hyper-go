package main

// ai_fast.go — Optimized AI response layer
// ════════════════════════════════════════════════════════════
//
// PROBLEM:
//   handleAICommand original: panggil 1 model → tunggu response → return.
//   Worst case: 60s timeout. Average: 3-8s.
//
// SOLUTION — Race + Cache + Stream:
//   1. RACE: panggil 2 model paralel (gemini + chatgpt), return yang duluan
//      → worst case tetap 60s tapi average jadi 1-4s
//   2. CACHE: simpan response untuk prompt yang sama (TTL 5 menit)
//      → repeat query instan
//   3. SHORTER TIMEOUT: 30s per model (bukan 60s) — ganti model lebih cepat
//   4. PRE-FLIGHT: cek cache dulu sebelum race
//
// Command:
//   .ai <prompt>          — race gemini + chatgpt, return winner
//   .aifast <prompt>      — alias .ai
//   .aistat               — lihat cache stat + race winner
//   .aiclear              — clear cache

import (
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── AI Cache ─────────────────────────────────────────────────────

type aiCacheEntry struct {
	Response string
	Model    string
	Time     time.Time
}

var (
	aiCacheMu  sync.RWMutex
	aiCache    = map[string]aiCacheEntry{} // key = model+":"+prompt (lowercased)
	aiCacheTTL = 5 * time.Minute

	// race stats
	aiRaceMu      sync.Mutex
	aiRaceStats   = map[string]int{} // model → win count
	aiRaceTotal   int
	aiCacheHits   int
	aiCacheMisses int
)

const aiCacheMaxEntries = 200

func aiCacheKey(model, prompt string) string {
	return strings.ToLower(model) + ":" + strings.ToLower(strings.TrimSpace(prompt))
}

func aiCacheGet(model, prompt string) (aiCacheEntry, bool) {
	aiCacheMu.RLock()
	defer aiCacheMu.RUnlock()
	e, ok := aiCache[aiCacheKey(model, prompt)]
	if !ok {
		return aiCacheEntry{}, false
	}
	if time.Since(e.Time) > aiCacheTTL {
		return aiCacheEntry{}, false
	}
	return e, true
}

func aiCachePut(model, prompt, response string) {
	aiCacheMu.Lock()
	defer aiCacheMu.Unlock()
	// Trim cache if too large (LRU-ish: just delete random old entries)
	if len(aiCache) >= aiCacheMaxEntries {
		for k, v := range aiCache {
			if time.Since(v.Time) > aiCacheTTL {
				delete(aiCache, k)
			}
		}
		// If still too big, evict oldest 20%
		if len(aiCache) >= aiCacheMaxEntries {
			var oldest []string
			oldestTime := time.Now()
			for k, v := range aiCache {
				if v.Time.Before(oldestTime) {
					oldestTime = v.Time
					oldest = append([]string{k}, oldest...)
				}
			}
			for i, k := range oldest {
				if i >= 20 {
					break
				}
				delete(aiCache, k)
			}
		}
	}
	aiCache[aiCacheKey(model, prompt)] = aiCacheEntry{
		Response: response,
		Model:    model,
		Time:     time.Now(),
	}
}

func aiCacheClear() {
	aiCacheMu.Lock()
	defer aiCacheMu.Unlock()
	aiCache = map[string]aiCacheEntry{}
}

// ─── Race: parallel model queries, return first winner ────────────

type aiRaceResult struct {
	Model    string
	Response string
	Error    error
	Elapsed  time.Duration
}

// aiRaceModels queries multiple models in parallel and returns the FIRST
// successful response (true race). Remaining in-flight queries are abandoned
// (their results are discarded via buffered channel — no goroutine leak).
//
// If all fail, returns the last error seen.

// kwAIChatSingle is a single-model AI query with a REAL custom timeout.
// kwAIChat itself hardcodes 60s, so we wrap it and enforce our own deadline;
// the loser goroutine finishes in background (buffered channel = no leak).

// ─── Fast AI Command Handler ─────────────────────────────────────

// handleAIFastCommand — .ai <prompt> or .aifast <prompt>
// Strategy:
//  1. Check cache → return immediately if hit
//  2. Race gemini + chatgpt in parallel → return winner
//  3. Cache the result for 5 minutes
func handleAIFastCommand(args string, msg *events.Message) string {
	if isSelfMode() && !isOwnerMsg(msg) {
		return "❌ Self mode aktif — hanya owner yang bisa pakai AI"
	}
	prompt := strings.TrimSpace(args)
	if prompt == "" {
		return "❓ *Cara pakai:* .ai <pertanyaan>\nContoh: .ai jelaskan blockchain\n\nRace: gemini + chatgpt paralel, return yang lebih cepat."
	}

	// Check cache first
	if e, ok := aiCacheGet("race", prompt); ok {
		aiRaceMu.Lock()
		aiCacheHits++
		aiRaceMu.Unlock()
		return fmt.Sprintf("🤖 *%s* (cache hit)\n\n%s", strings.ToUpper(e.Model), e.Response)
	}

	aiRaceMu.Lock()
	aiCacheMisses++
	aiRaceMu.Unlock()

	// Race 2 fastest models
	// Langsung ke Pollinations (tanpa kangwifi)
	resp, err := pollinationsTextAsk(prompt)
	if err != nil {
		return fmt.Sprintf("❌ AI error: %v", err)
	}

	// Cache for next time
	aiCachePut("race", prompt, resp)

	return fmt.Sprintf("🤖 *AI*\n\n%s", resp)
}

// ─── Stats & Management Commands ──────────────────────────────────

func handleAIStatCommand() string {
	aiRaceMu.Lock()
	aiCacheMu.RLock()
	defer aiRaceMu.Unlock()
	defer aiCacheMu.RUnlock()

	var sb strings.Builder
	sb.WriteString("📊 *AI Stats*\n\n")
	sb.WriteString(fmt.Sprintf("• Cache hits: %d\n", aiCacheHits))
	sb.WriteString(fmt.Sprintf("• Cache misses: %d\n", aiCacheMisses))
	sb.WriteString(fmt.Sprintf("• Cache size: %d / %d\n", len(aiCache), aiCacheMaxEntries))
	hitRate := 0.0
	total := aiCacheHits + aiCacheMisses
	if total > 0 {
		hitRate = float64(aiCacheHits) / float64(total) * 100
	}
	sb.WriteString(fmt.Sprintf("• Hit rate: %.1f%%\n\n", hitRate))

	sb.WriteString("*Race winners:*\n")
	if aiRaceTotal == 0 {
		sb.WriteString("• (belum ada race)\n")
	} else {
		// Sort by win count
		type kv struct {
			Model string
			Wins  int
		}
		var stats []kv
		for m, w := range aiRaceStats {
			stats = append(stats, kv{m, w})
		}
		// Simple sort
		for i := 0; i < len(stats); i++ {
			for j := i + 1; j < len(stats); j++ {
				if stats[j].Wins > stats[i].Wins {
					stats[i], stats[j] = stats[j], stats[i]
				}
			}
		}
		for _, s := range stats {
			pct := float64(s.Wins) / float64(aiRaceTotal) * 100
			sb.WriteString(fmt.Sprintf("• %s: %d wins (%.0f%%)\n", s.Model, s.Wins, pct))
		}
	}
	return sb.String()
}

func handleAIClearCommand() string {
	aiCacheClear()
	aiRaceMu.Lock()
	aiRaceStats = map[string]int{}
	aiRaceTotal = 0
	aiCacheHits = 0
	aiCacheMisses = 0
	aiRaceMu.Unlock()
	return "✅ AI cache + stats dibersihkan"
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "aifast",
		Description: "aifast",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAIFastCommand(args, msg)
			// ─── Hermes Agent (owner-only, KangRouter) ─────────────
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "aistat",
		Description: "aistat",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAIStatCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "aiclear",
		Description: "aiclear",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAIClearCommand()
			// ─── Playcall (PTT voice note to target — auto-play) ──────
		},
	})
}
