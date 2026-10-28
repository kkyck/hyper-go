package main

// errgroup.go — Error group for parallel goroutines with error propagation.
// Runs goroutines in parallel, captures first error, cancels remaining.

import (
	"context"
	"sync"
	"sync/atomic"
)

type errGroup struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	err    atomic.Pointer[error]
}

func newErrGroup(ctx context.Context) *errGroup {
	innerCtx, cancel := context.WithCancel(ctx)
	return &errGroup{ctx: innerCtx, cancel: cancel}
}

func (g *errGroup) goRun(fn func(ctx context.Context) error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := fn(g.ctx); err != nil {
			g.err.CompareAndSwap(nil, &err)
			g.cancel()
		}
	}()
}

func (g *errGroup) wait() error {
	g.wg.Wait()
	g.cancel()
	errPtr := g.err.Load()
	if errPtr == nil {
		return nil
	}
	return *errPtr
}

func (g *errGroup) context() context.Context { return g.ctx }
