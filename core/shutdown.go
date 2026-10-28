package main

// shutdown.go — Graceful shutdown with context propagation.
// Replaces blocking channel with context-based shutdown.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	rootCtx    context.Context
	rootCancel context.CancelFunc
	shutdownWG sync.WaitGroup
)

func initShutdownContext() {
	rootCtx, rootCancel = context.WithCancel(context.Background())
}

func waitForShutdown() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\n👋 Menerima sinyal shutdown, memutus koneksi...")
	rootCancel()
	done := make(chan struct{})
	go func() { shutdownWG.Wait(); close(done) }()
	select {
	case <-done:
		fmt.Println("✅ Semua task selesai, shutdown graceful.")
	case <-time.After(10 * time.Second):
		fmt.Println("⚠️ Shutdown timeout (10s) — force exit.")
	}
}

func trackWork() func() {
	shutdownWG.Add(1)
	return func() { shutdownWG.Done() }
}

func isShuttingDown() bool {
	if rootCtx == nil {
		return false
	}
	select {
	case <-rootCtx.Done():
		return true
	default:
		return false
	}
}

func getRootCtx() context.Context {
	if rootCtx == nil {
		return context.Background()
	}
	return rootCtx
}
