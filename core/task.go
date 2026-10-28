// task.go — Bounded worker pool ala tokio (Rust).
//
// Masalah: 64x "go func()" liar di codebase = goroutine unbounded.
// Saat load tinggi (spam grup, broadcast), goroutine bisa meledak -> OOM.
// Tokio (Rust) tidak membiarkan spawn tanpa backpressure.
//
// Solusi: Pool dengan worker tetap + antrian terbatas.
//   - Submit() memblokir (backpressure) atau return false bila penuh.
//   - Panic per-task di-recover, tidak menjatuhkan worker.
//   - Metrics: queued/running/completed untuk observability.
//
// Cara pakai:
//
//	var MsgPool = NewPool("msg", 32, 1000)  // 32 worker, antrian 1000
//	func init() { MsgPool.Start() }
//	MsgPool.Submit(func() { ... })  // ganti "go func() { ... }()"
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Task adalah unit kerja.
type Task func()

// Pool adalah worker pool terbatas.
type Pool struct {
	name      string
	workers   int
	queue     chan Task
	wg        sync.WaitGroup
	stopped   int32
	queued    atomic.Int64
	running   atomic.Int64
	completed atomic.Int64
	dropped   atomic.Int64
	panics    atomic.Int64
}

// NewPool membuat pool baru (belum jalan, panggil Start).
func NewPool(name string, workers, queueSize int) *Pool {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if queueSize <= 0 {
		queueSize = 1000
	}
	return &Pool{
		name:    name,
		workers: workers,
		queue:   make(chan Task, queueSize),
	}
}

// Start menjalankan worker goroutines.
func (p *Pool) Start() {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
	fmt.Printf("[POOL] %s started (%d workers, queue %d)\n", p.name, p.workers, cap(p.queue))
}

// Stop menunggu semua task selesai (graceful shutdown).
func (p *Pool) Stop() {
	if !atomic.CompareAndSwapInt32(&p.stopped, 0, 1) {
		return
	}
	close(p.queue)
	p.wg.Wait()
	fmt.Printf("[POOL] %s stopped (completed=%d dropped=%d panics=%d)\n",
		p.name, p.completed.Load(), p.dropped.Load(), p.panics.Load())
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	for task := range p.queue {
		p.queued.Add(-1)
		p.running.Add(1)
		func() {
			defer func() {
				if r := recover(); r != nil {
					p.panics.Add(1)
					fmt.Printf("[POOL] %s worker %d panic: %v\n", p.name, id, r)
				}
			}()
			task()
		}()
		p.running.Add(-1)
		p.completed.Add(1)
	}
}

// Submit mengirim task ke antrian. Return false bila pool penuh/stopped
// (caller harus handle: drop, retry, atau fallback).
// Tidak pernah memblokir — backpressure via return value, bukan deadlock.
func (p *Pool) Submit(task Task) bool {
	if atomic.LoadInt32(&p.stopped) != 0 {
		return false
	}
	select {
	case p.queue <- task:
		p.queued.Add(1)
		return true
	default:
		p.dropped.Add(1)
		return false
	}
}

// SubmitBlocking seperti Submit tapi menunggu sampai ada slot (max wait).
// Untuk task yang tidak boleh di-drop.
func (p *Pool) SubmitBlocking(task Task, maxWait time.Duration) bool {
	if atomic.LoadInt32(&p.stopped) != 0 {
		return false
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case p.queue <- task:
		p.queued.Add(1)
		return true
	case <-timer.C:
		p.dropped.Add(1)
		return false
	}
}

// Stats mengembalikan metrik pool.
func (p *Pool) Stats() (queued, running, completed, dropped, panics int64) {
	return p.queued.Load(), p.running.Load(), p.completed.Load(),
		p.dropped.Load(), p.panics.Load()
}

// ─── Pool global ───
// Dipakai untuk menggantikan "go func()" liar di hot path.

var (
	// MsgPool untuk handle pesan (I/O-bound, worker = 2x CPU).
	MsgPool = NewPool("msg", 32, 5000)
	// BgPool untuk tugas background (DB, file, network).
	BgPool = NewPool("bg", 16, 2000)
	// NetPool untuk HTTP/API calls (batasi koneksi konkuren).
	NetPool = NewPool("net", 24, 1000)
)

func init() {
	MsgPool.Start()
	BgPool.Start()
	NetPool.Start()
}
