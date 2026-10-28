// own.go — Disiplin ownership ala Rust untuk Go.
//
// Borrow-checker Rust tidak bisa ditempel ke compiler Go. Tapi DISIPLIN-nya
// bisa: single ownership, move semantics, dan cleanup deterministik.
//
// Prinsip yang ditegakkan di codebase ini:
//  1. SATU OWNER: mutable state hanya dimiliki satu goroutine dalam satu
//     waktu. Transfer kepemilikan lewat channel, bukan shared memory + mutex.
//     ("Share memory by communicating" = ownership transfer ala Go.)
//  2. MOVE, BUKAN COPY: handle yang sudah di-move tidak boleh dipakai lagi.
//     Box.Move() membuat handle lama panik saat dipakai (use-after-move).
//  3. RAII: resource (file, temp dir, proses) dibersihkan deterministik
//     via defer/Scope — tidak mengandalkan GC.
//  4. BUILD KETAT: go vet gagal = build gagal (lihat build.sh).
//
// Aturan untuk kode baru:
//   - Butuh shared mutable state? Pakai Registry (actor) di bawah, JANGAN
//     map global + sync.Mutex (rawan lupa lock / deadlock).
//   - Butuh transfer ownership antar goroutine? Pakai Box[T].Move().
//   - Butuh banyak cleanup berurutan? Pakai Scope.
package main

import (
	"fmt"
	"sync"
)

// ─── Box[T]: single-ownership cell dengan move semantics ──────────────
// Setelah Move(), handle lama tidak valid lagi (Get/Take panik).
// Ini menangkap bug use-after-move yang di Rust ditolak compiler.
type Box[T any] struct {
	mu     sync.Mutex
	val    T
	moved  bool
	closed bool
}

func NewBox[T any](v T) *Box[T] { return &Box[T]{val: v} }

// Get membaca tanpa move. Panik bila sudah di-move/diambil.
func (b *Box[T]) Get() T {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.moved {
		panic("own: use-after-move")
	}
	return b.val
}

// Move memindahkan kepemilikan ke handle baru; handle lama mati.
func (b *Box[T]) Move() *Box[T] {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.moved {
		panic("own: double-move")
	}
	b.moved = true
	return &Box[T]{val: b.val}
}

// Take = Move + ambil nilainya sekaligus.
func (b *Box[T]) Take() T {
	nb := b.Move()
	return nb.val
}

// ─── Scope: grup cleanup deterministik (RAII) ─────────────────────────
// Daftarkan cleanup dengan OnClose; Close() menjalankannya LIFO —
// seperti destructor Rust yang berjalan saat scope berakhir.
type Scope struct {
	mu       sync.Mutex
	cleanups []func()
	closed   bool
}

func NewScope() *Scope { return &Scope{} }

// OnClose mendaftarkan satu fungsi cleanup.
func (s *Scope) OnClose(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// Sudah ditutup: jalankan langsung agar tidak bocor.
		go fn()
		return
	}
	s.cleanups = append(s.cleanups, fn)
}

// Close menjalankan semua cleanup LIFO, tepat sekali.
func (s *Scope) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	fns := s.cleanups
	s.cleanups = nil
	s.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		func() {
			defer func() { _ = recover() }()
			fns[i]()
		}()
	}
}

// ─── Registry: shared state dengan SATU owner (actor pattern) ────────
// Pengganti "map global + sync.Mutex". Satu goroutine memiliki map;
// semua operasi lewat channel. Tidak ada lock yang bisa lupa/deadlock.
type regOp[K comparable, V any] struct {
	op    int // 0=set, 1=del, 2=list
	key   K
	val   V
	reply chan map[K]V
}

type Registry[K comparable, V any] struct {
	ops chan regOp[K, V]
}

// NewRegistry membuat registry; goroutine owner jalan di background.
func NewRegistry[K comparable, V any]() *Registry[K, V] {
	r := &Registry[K, V]{ops: make(chan regOp[K, V], 64)}
	go r.owner()
	return r
}

func (r *Registry[K, V]) owner() {
	m := make(map[K]V)
	for op := range r.ops {
		switch op.op {
		case 0:
			m[op.key] = op.val
		case 1:
			delete(m, op.key)
		case 2:
			cp := make(map[K]V, len(m))
			for k, v := range m {
				cp[k] = v
			}
			op.reply <- cp
		}
	}
}

// Set mendaftarkan key->val (ownership pindah ke registry).
func (r *Registry[K, V]) Set(key K, val V) {
	r.ops <- regOp[K, V]{op: 0, key: key, val: val}
}

// Delete menghapus key.
func (r *Registry[K, V]) Delete(key K) {
	r.ops <- regOp[K, V]{op: 1, key: key}
}

// Snapshot mengembalikan COPY map (bukan referensi ke state owner).
func (r *Registry[K, V]) Snapshot() map[K]V {
	reply := make(chan map[K]V, 1)
	r.ops <- regOp[K, V]{op: 2, reply: reply}
	return <-reply
}

// Len aman untuk dibaca kapan saja.
func (r *Registry[K, V]) Len() int { return len(r.Snapshot()) }

var _ = fmt.Sprint // cegah import unused bila fmt tak terpakai langsung
