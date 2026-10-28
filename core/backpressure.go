package main

// backpressure.go — Token bucket rate limiter + circuit breaker.
// Prevents WhatsApp ban by limiting request rate.

import (
	"sync"
	"sync/atomic"
	"time"
)

type tokenBucket struct {
	tokens   atomic.Int64
	capacity int64
	refillAt atomic.Int64
	mu       sync.Mutex
}

func newTokenBucket(capacity int64, refill time.Duration) *tokenBucket {
	tb := &tokenBucket{capacity: capacity}
	tb.tokens.Store(capacity)
	tb.refillAt.Store(time.Now().Add(refill).UnixNano())
	return tb
}

func (tb *tokenBucket) Allow() bool {
	tb.refillIfNeeded()
	return tb.tokens.Add(-1) >= 0
}

func (tb *tokenBucket) refillIfNeeded() {
	now := time.Now().UnixNano()
	if now < tb.refillAt.Load() {
		return
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if time.Now().UnixNano() < tb.refillAt.Load() {
		return
	}
	tb.tokens.Store(tb.capacity)
	tb.refillAt.Store(time.Now().Add(1 * time.Second).UnixNano())
}

type circuitState int32

const (
	circuitClosed   circuitState = 0
	circuitOpen     circuitState = 1
	circuitHalfOpen circuitState = 2
)

type circuitBreaker struct {
	state        atomic.Int32
	failureCount atomic.Int64
	threshold    int64
	cooldown     time.Duration
	trippedAt    atomic.Int64
}

func newCircuitBreaker(threshold int64, cooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{threshold: threshold, cooldown: cooldown}
}

func (cb *circuitBreaker) Allow() bool {
	switch circuitState(cb.state.Load()) {
	case circuitClosed:
		return true
	case circuitOpen:
		if time.Now().UnixNano()-cb.trippedAt.Load() > cb.cooldown.Nanoseconds() {
			if cb.state.CompareAndSwap(int32(circuitOpen), int32(circuitHalfOpen)) {
				return true
			}
		}
		return false
	case circuitHalfOpen:
		return true
	default:
		return true
	}
}

func (cb *circuitBreaker) RecordSuccess() {
	if circuitState(cb.state.Load()) == circuitHalfOpen {
		cb.state.Store(int32(circuitClosed))
		cb.failureCount.Store(0)
	}
}

func (cb *circuitBreaker) RecordFailure() {
	if circuitState(cb.state.Load()) == circuitHalfOpen {
		cb.state.Store(int32(circuitOpen))
		cb.trippedAt.Store(time.Now().UnixNano())
		return
	}
	if cb.failureCount.Add(1) >= cb.threshold {
		cb.state.Store(int32(circuitOpen))
		cb.trippedAt.Store(time.Now().UnixNano())
	}
}

func (cb *circuitBreaker) State() string {
	switch circuitState(cb.state.Load()) {
	case circuitClosed:
		return "CLOSED ✅"
	case circuitOpen:
		return "OPEN 🔴"
	case circuitHalfOpen:
		return "HALF_OPEN 🟡"
	default:
		return "UNKNOWN"
	}
}

var sprRateLimiter = newTokenBucket(20, 1*time.Second)
var sprCircuitBreaker = newCircuitBreaker(5, 30*time.Second)
