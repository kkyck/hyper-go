package main

// profiling.go — Go pprof + runtime profiling integration.
//
// ─── WHY PROFILING ─────────────────────────────────────────────────
//
// In production, we need to identify:
//   - Memory leaks (which goroutines hold references?)
//   - CPU hotspots (which functions take most time?)
//   - Goroutine leaks (goroutines that never exit)
//   - Block profiling (which mutexes cause contention?)
//
// Go's net/http/pprof provides all of this via HTTP endpoints:
//   /debug/pprof/heap     — heap allocation profile
//   /debug/pprof/goroutine — goroutine stack traces
//   /debug/pprof/profile   — CPU profile (30s sample)
//   /debug/pprof/block     — blocking profile
//   /debug/pprof/mutex     — mutex contention profile
//
// ─── USAGE ─────────────────────────────────────────────────────────
//
// Start profiling server (alongside observer):
//   go startProfiling(":24726")
//
// Then access from browser or CLI:
//   go tool pprof http://localhost:24726/debug/pprof/heap
//   go tool pprof http://localhost:24726/debug/pprof/profile?seconds=30

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
)

// startProfiling starts a pprof HTTP server for runtime profiling.
// Runs in background goroutine, non-blocking.
//
// Endpoints:
//
//	/debug/pprof/           — index page
//	/debug/pprof/heap       — heap profile
//	/debug/pprof/goroutine  — goroutine profile
//	/debug/pprof/threadcreate — thread creation profile
//	/debug/pprof/block      — blocking profile
//	/debug/pprof/mutex      — mutex profile
//	/debug/pprof/profile     — CPU profile (default 30s)
//	/debug/pprof/trace       — execution trace
func startProfiling(addr string) {
	mux := http.NewServeMux()

	// Register pprof handlers
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// Manual profile handlers
	mux.HandleFunc("/debug/pprof/heap", pprof.Handler("heap").ServeHTTP)
	mux.HandleFunc("/debug/pprof/goroutine", pprof.Handler("goroutine").ServeHTTP)
	mux.HandleFunc("/debug/pprof/threadcreate", pprof.Handler("threadcreate").ServeHTTP)
	mux.HandleFunc("/debug/pprof/block", pprof.Handler("block").ServeHTTP)
	mux.HandleFunc("/debug/pprof/mutex", pprof.Handler("mutex").ServeHTTP)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		fmt.Printf("🔬 Profiling: http://localhost%s/debug/pprof/\n", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("Profiling server error", "err", err)
		}
	}()

	// Enable mutex + block profiling
	runtime.SetMutexProfileFraction(1)   // sample 1/1 mutex events
	runtime.SetBlockProfileRate(1000000) // sample 1ms blocking events

	// Graceful shutdown
	go func() {
		<-getRootCtx().Done()
		_ = server.Close()
		fmt.Println("🔬 Profiling stopped")
	}()
}

// goroutineCount returns current goroutine count.
// Useful for detecting goroutine leaks.
func goroutineCount() int {
	return runtime.NumGoroutine()
}

// forceGC triggers garbage collection and returns freed memory.
func forceGC() uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	runtime.GC()
	runtime.ReadMemStats(&after)
	return before.Alloc - after.Alloc
}
