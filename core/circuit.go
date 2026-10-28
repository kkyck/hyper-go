// circuit.go — Circuit breaker ala ekosistem Rust.
//
// Masalah: API eksternal (KangRouter, Gemini, Pollinations) kadang mati.
// Tanpa circuit breaker, setiap request tetap dicoba -> timeout bertumpuk,
// goroutine numpuk, latency meledak (cascade failure).
//
// Pola: CLOSED (normal) -> OPEN (gagal N kali, langsung tolak) ->
// HALF-OPEN (coba 1 request setelah cooldown) -> CLOSED/OPEN.
//
// Mirip crate `breaker` / `failsafe` di Rust.
package main

import (
	"errors"
	"sync"
	"time"
)

// CircuitState adalah status circuit.
type CircuitState int

const (
	StateClosed CircuitState = iota
	StateOpen
	StateHalfOpen
)

// Circuit adalah circuit breaker untuk satu downstream.
type Circuit struct {
	name         string
	maxFailures  int
	cooldown     time.Duration
	mu           sync.Mutex
	state        CircuitState
	failures     int
	lastFailTime time.Time
	successes    int // di half-open
}

// NewCircuit membuat circuit breaker baru.
func NewCircuit(name string, maxFailures int, cooldown time.Duration) *Circuit {
	if maxFailures <= 0 {
		maxFailures = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Circuit{name: name, maxFailures: maxFailures, cooldown: cooldown}
}

var ErrCircuitOpen = errors.New("circuit open: downstream sedang mati, request ditolak langsung")

// Call mengeksekusi fn dengan proteksi circuit.
// Return ErrCircuitOpen bila circuit terbuka (tanpa coba request).
func (c *Circuit) Call(fn func() error) error {
	c.mu.Lock()
	switch c.state {
	case StateOpen:
		if time.Since(c.lastFailTime) < c.cooldown {
			c.mu.Unlock()
			return ErrCircuitOpen
		}
		// Cooldown habis -> half-open, izinkan 1 probe
		c.state = StateHalfOpen
		c.successes = 0
	case StateHalfOpen:
		// Hanya 1 probe dalam satu waktu; yang lain ditolak
		// (disederhanakan: izinkan, tapi track)
	}
	c.mu.Unlock()

	err := fn()

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.failures++
		c.lastFailTime = time.Now()
		if c.state == StateHalfOpen || c.failures >= c.maxFailures {
			c.state = StateOpen
		}
		return err
	}
	// Sukses
	if c.state == StateHalfOpen {
		c.successes++
		if c.successes >= 2 {
			c.state = StateClosed
			c.failures = 0
		}
	} else {
		c.failures = 0
	}
	return nil
}

// State mengembalikan status saat ini (untuk monitoring).
func (c *Circuit) State() CircuitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Auto-transition OPEN -> HALF-OPEN bila cooldown habis
	if c.state == StateOpen && time.Since(c.lastFailTime) >= c.cooldown {
		c.state = StateHalfOpen
		c.successes = 0
	}
	return c.state
}

// ─── Circuit global untuk API eksternal ───

var (
	CircuitKangRouter   = NewCircuit("kangrouter", 3, 30*time.Second)
	CircuitGemini       = NewCircuit("gemini", 3, 60*time.Second)
	CircuitPollinations = NewCircuit("pollinations", 5, 20*time.Second)
	CircuitPrimaryAI    = NewCircuit("primary-ai", 3, 60*time.Second)
)
