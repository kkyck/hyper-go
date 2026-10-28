package main

// context_helper.go — Named timeout presets + generic context helpers.

import (
	"context"
	"time"
)

var timeoutPresets = map[string]time.Duration{
	"quick": 5 * time.Second, "normal": 15 * time.Second, "long": 30 * time.Second,
	"very_long": 60 * time.Second, "extreme": 5 * time.Minute,
	"spr_send": 60 * time.Second, "spr_hide": 30 * time.Second,
	"ai_call": 30 * time.Second, "geo_lookup": 10 * time.Second,
	"gsmarena": 15 * time.Second, "http": 10 * time.Second, "shell": 30 * time.Second,
}

func withTimeout(name string) (context.Context, context.CancelFunc) {
	dur, ok := timeoutPresets[name]
	if !ok {
		dur = 30 * time.Second
	}
	return context.WithTimeout(getRootCtx(), dur)
}

func withTimeoutDo(name string, fn func(ctx context.Context) error) error {
	ctx, cancel := withTimeout(name)
	defer cancel()
	return fn(ctx)
}

func withTimeoutResult[T any](name string, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx, cancel := withTimeout(name)
	defer cancel()
	return fn(ctx)
}

func isContextExpired(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func remainingTime(ctx context.Context) time.Duration {
	if ctx == nil {
		return 0
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	r := time.Until(deadline)
	if r < 0 {
		return 0
	}
	return r
}
