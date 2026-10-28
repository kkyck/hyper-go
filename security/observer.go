package main

// observer.go — Runtime profiling + health check endpoint.
// GET /health → JSON health check
// GET /stats  → metrics + cache stats
// GET /gc     → trigger GC

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"
)

type healthStatus struct {
	Status       string  `json:"status"`
	Uptime       string  `json:"uptime"`
	Goroutines   int     `json:"goroutines"`
	MemoryAlloc  uint64  `json:"memory_alloc"`
	NumGC        uint32  `json:"num_gc"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	CircuitState string  `json:"circuit_state"`
}

func startObserver(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getHealthStatus())
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, metricsReport())
		fmt.Fprintf(w, "\n\nCircuit: %s\nGoroutines: %d\n", sprCircuitBreaker.State(), runtime.NumGoroutine())
	})
	mux.HandleFunc("/gc", func(w http.ResponseWriter, r *http.Request) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		runtime.GC()
		runtime.ReadMemStats(&after)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "GC done. Freed: %s\n", formatBytesHuman(before.Alloc-after.Alloc))
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	go func() {
		fmt.Printf("🔍 Observer: http://localhost%s\n", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("Observer error", "err", err)
		}
	}()
	go func() {
		<-getRootCtx().Done()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
}

func getHealthStatus() healthStatus {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	hits := metricsGet(&cacheHits)
	misses := metricsGet(&cacheMisses)
	var hr float64
	if hits+misses > 0 {
		hr = float64(hits) / float64(hits+misses) * 100
	}
	status := "ok"
	if sprCircuitBreaker.State() == "OPEN 🔴" || runtime.NumGoroutine() > 1000 {
		status = "degraded"
	}
	return healthStatus{
		Status: status, Uptime: time.Since(uptimeStart).Round(time.Second).String(),
		Goroutines: runtime.NumGoroutine(), MemoryAlloc: m.Alloc, NumGC: m.NumGC,
		CacheHitRate: hr, CircuitState: sprCircuitBreaker.State(),
	}
}

func formatBytesHuman(b uint64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1048576:
		return fmt.Sprintf("%.2f KB", float64(b)/1024)
	case b < 1073741824:
		return fmt.Sprintf("%.2f MB", float64(b)/1048576)
	default:
		return fmt.Sprintf("%.2f GB", float64(b)/1073741824)
	}
}
