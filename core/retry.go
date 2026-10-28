package main

// retry.go — Exponential backoff with jitter for resilient API calls.

import (
	"context"
	"math/rand"
	"time"
)

type retryConfig struct {
	maxRetries   int
	baseDelay    time.Duration
	maxDelay     time.Duration
	jitterFactor float64
}

func defaultRetryConfig() retryConfig {
	return retryConfig{maxRetries: 3, baseDelay: 500 * time.Millisecond, maxDelay: 10 * time.Second, jitterFactor: 0.3}
}

func retryWithBackoff(ctx context.Context, maxRetries int, fn func(context.Context) error) error {
	cfg := defaultRetryConfig()
	cfg.maxRetries = maxRetries
	var lastErr error
	for attempt := 0; attempt <= cfg.maxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := fn(ctx); err != nil {
			lastErr = err
			logWarn("Retry failed", "attempt", attempt+1, "err", err)
			if attempt < cfg.maxRetries {
				delay := calculateBackoff(attempt, cfg)
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		} else {
			return nil
		}
	}
	return lastErr
}

func calculateBackoff(attempt int, cfg retryConfig) time.Duration {
	delay := cfg.baseDelay * time.Duration(1<<uint(attempt))
	if delay > cfg.maxDelay {
		delay = cfg.maxDelay
	}
	if cfg.jitterFactor > 0 {
		jitterRange := float64(delay) * cfg.jitterFactor
		delay += time.Duration(rand.Float64()*jitterRange*2 - jitterRange)
		if delay < 0 {
			delay = 0
		}
	}
	return delay
}
