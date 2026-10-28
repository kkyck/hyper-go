package main

// metrics.go — Atomic lock-free counters for runtime metrics.
// Uses sync/atomic — 10x faster than mutex-based counters.

import (
	"fmt"
	"sync/atomic"
	"time"
)

var (
	commandsTotal   int64
	commandsFailed  int64
	sprSendsTotal   int64
	sprSendsFailed  int64
	spr2SendsTotal  int64
	spr2SendsFailed int64
	cacheHits       int64
	cacheMisses     int64
	replyLatencyNs  int64
	uptimeStart     = time.Now()
)

func metricsIncr(counter *int64)             { atomic.AddInt64(counter, 1) }
func metricsAdd(counter *int64, delta int64) { atomic.AddInt64(counter, delta) }
func metricsSet(counter *int64, val int64)   { atomic.StoreInt64(counter, val) }
func metricsGet(counter *int64) int64        { return atomic.LoadInt64(counter) }

func recordCacheHit()  { metricsIncr(&cacheHits) }
func recordCacheMiss() { metricsIncr(&cacheMisses) }

func metricsReport() string {
	uptime := time.Since(uptimeStart).Round(time.Second)
	hits := metricsGet(&cacheHits)
	misses := metricsGet(&cacheMisses)
	cacheRate := 0.0
	if hits+misses > 0 {
		cacheRate = float64(hits) / float64(hits+misses) * 100
	}
	return fmt.Sprintf("📊 *Bot Metrics*\n\n*Uptime:* %s\n\n*Commands:* %d (failed: %d)\n*SPR:* %d (failed: %d)\n*SPR2:* %d (failed: %d)\n*Cache:* %d hits / %d misses (%.1f%%)\n*Last latency:* %s",
		uptime,
		metricsGet(&commandsTotal), metricsGet(&commandsFailed),
		metricsGet(&sprSendsTotal), metricsGet(&sprSendsFailed),
		metricsGet(&spr2SendsTotal), metricsGet(&spr2SendsFailed),
		hits, misses, cacheRate,
		time.Duration(metricsGet(&replyLatencyNs)).Round(time.Millisecond))
}
