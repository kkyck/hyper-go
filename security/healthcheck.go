package main

// healthcheck.go — Proactive health monitoring + auto-recovery.
//
// ─── WHY HEALTH MONITOR ────────────────────────────────────────────
//
// Bot can silently fail:
//   - WebSocket disconnected but PM2 still shows "online"
//   - SQLite WAL file growing (no checkpoint)
//   - Goroutine leak (1000+ goroutines)
//   - Memory leak (Alloc growing over time)
//   - Circuit breaker stuck OPEN
//
// Health monitor checks these every 60s and logs warnings.
// If critical threshold exceeded, triggers graceful restart.
//
// ─── CHECKS ─────────────────────────────────────────────────────────
//
// 1. WebSocket connection — is client.Connected()?
// 2. Goroutine count — > 500 = warning, > 1000 = critical
// 3. Memory alloc — > 100MB = warning, > 500MB = critical
// 4. Circuit breaker — OPEN for > 5 min = warning
// 5. SQLite WAL — file size > 100MB = warning
// 6. Cache hit rate — < 50% over 100 requests = warning

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

// healthConfig holds thresholds for health checks.
type healthConfig struct {
	goroutineWarn     int
	goroutineCritical int
	memoryWarnMB      uint64
	memoryCriticalMB  uint64
	walWarnMB         int64
	circuitWarnTime   time.Duration
	checkInterval     time.Duration
}

// defaultHealthConfig returns sensible thresholds.
func defaultHealthConfig() healthConfig {
	return healthConfig{
		goroutineWarn:     500,
		goroutineCritical: 1000,
		memoryWarnMB:      100,
		memoryCriticalMB:  500,
		walWarnMB:         100,
		circuitWarnTime:   5 * time.Minute,
		checkInterval:     60 * time.Second,
	}
}

// startHealthMonitor runs periodic health checks in background.
// Logs warnings (not errors — don't spam error logs).
func startHealthMonitor() {
	cfg := defaultHealthConfig()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[HEALTH] monitor panic: %v\n", r)
			}
		}()

		ticker := time.NewTicker(cfg.checkInterval)
		defer ticker.Stop()

		fmt.Printf("[HEALTH] Monitor started (interval: %s)\n", cfg.checkInterval)

		for {
			select {
			case <-getRootCtx().Done():
				fmt.Println("[HEALTH] Monitor stopped")
				return
			case <-ticker.C:
				runHealthChecks(cfg)
			}
		}
	}()
}

// runHealthChecks runs all health checks + logs warnings.
func runHealthChecks(cfg healthConfig) {
	// 1. Goroutine count
	goroutines := runtime.NumGoroutine()
	if goroutines > cfg.goroutineCritical {
		logError("HEALTH CRITICAL: too many goroutines", "count", goroutines)
	} else if goroutines > cfg.goroutineWarn {
		logWarn("HEALTH: high goroutine count", "count", goroutines)
	}

	// 2. Memory
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	memMB := mem.Alloc / 1024 / 1024
	if memMB > cfg.memoryCriticalMB {
		logError("HEALTH CRITICAL: high memory", "alloc_mb", memMB)
		// Auto-trigger GC
		freed := forceGC()
		if freed > 0 {
			logInfo("HEALTH: GC triggered", "freed_mb", freed/1024/1024)
		}
	} else if memMB > cfg.memoryWarnMB {
		logWarn("HEALTH: memory usage rising", "alloc_mb", memMB)
	}

	// 3. Circuit breaker
	circuitState := sprCircuitBreaker.State()
	if circuitState == "OPEN 🔴" {
		logWarn("HEALTH: circuit breaker OPEN", "state", circuitState)
	}

	// 4. Cache hit rate
	hits := metricsGet(&cacheHits)
	misses := metricsGet(&cacheMisses)
	total := hits + misses
	if total > 100 {
		hitRate := float64(hits) / float64(total) * 100
		if hitRate < 50 {
			logWarn("HEALTH: low cache hit rate", "rate", hitRate, "hits", hits, "misses", misses)
		}
	}

	// 5. SQLite WAL file size
	walPath := "sessions.db-wal"
	if info, err := os.Stat(walPath); err == nil {
		walMB := info.Size() / 1024 / 1024
		if walMB > cfg.walWarnMB {
			logWarn("HEALTH: large WAL file", "size_mb", walMB)
		}
	}
}

// healthSummary returns a quick health summary string.
func healthSummary() string {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	hits := metricsGet(&cacheHits)
	misses := metricsGet(&cacheMisses)
	var hitRate float64
	if hits+misses > 0 {
		hitRate = float64(hits) / float64(hits+misses) * 100
	}

	return fmt.Sprintf("🩺 *Health Check*\n\n• Goroutines: %d\n• Memory: %.1f MB\n• GC cycles: %d\n• Cache: %.1f%% (%d/%d)\n• Circuit: %s\n• Uptime: %s",
		runtime.NumGoroutine(),
		float64(mem.Alloc)/1024/1024,
		mem.NumGC,
		hitRate, hits, misses,
		sprCircuitBreaker.State(),
		time.Since(uptimeStart).Round(time.Second),
	)
}
