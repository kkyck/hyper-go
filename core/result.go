// result.go — Result[T] ala Rust.
//
// Rust memaksa error handling eksplisit via Result<T, E>.
// Go sudah punya (value, err), tapi chaining operasi yang panjang
// jadi nested dan rawan lupa cek err.
//
// Result[T] membungkus (value, err) dengan method chaining:
//
//	result.Ok(fetchUser(id)).
//	    AndThen(func(u User) Result[Profile] { return Ok(fetchProfile(u)) }).
//	    Map(func(p Profile) string { return p.Name }).
//	    UnwrapOr("unknown")
//
// Zero-cost: struct 2 field, tidak ada alokasi tambahan.
package main

// Result membungkus value atau error. Mirip Rust Result<T, E>.
type Result[T any] struct {
	val T
	err error
}

// Ok membuat Result sukses.
func Ok[T any](v T) Result[T] { return Result[T]{val: v} }

// Err membuat Result gagal.
func Err[T any](e error) Result[T] { return Result[T]{err: e} }

// IsOk true bila tidak ada error.
func (r Result[T]) IsOk() bool { return r.err == nil }

// IsErr true bila ada error.
func (r Result[T]) IsErr() bool { return r.err != nil }

// Unwrap mengembalikan value atau panic bila Err.
// Pakai hanya bila yakin Ok (mirip Rust unwrap).
func (r Result[T]) Unwrap() T {
	if r.err != nil {
		panic("Result.Unwrap on Err: " + r.err.Error())
	}
	return r.val
}

// UnwrapOr mengembalikan value atau fallback bila Err.
func (r Result[T]) UnwrapOr(fallback T) T {
	if r.err != nil {
		return fallback
	}
	return r.val
}

// UnwrapOrElse mengembalikan value atau hasil fn(err) bila Err.
func (r Result[T]) UnwrapOrElse(fn func(error) T) T {
	if r.err != nil {
		return fn(r.err)
	}
	return r.val
}

// ErrValue mengembalikan error (nil bila Ok).
func (r Result[T]) ErrValue() error { return r.err }

// Map mentransformasi value bila Ok (mirip Rust map).
func Map[T, U any](r Result[T], fn func(T) U) Result[U] {
	if r.err != nil {
		return Err[U](r.err)
	}
	return Ok(fn(r.val))
}

// AndThen chaining operasi yang return Result (mirip Rust and_then).
// Berhenti di error pertama.
func AndThen[T, U any](r Result[T], fn func(T) Result[U]) Result[U] {
	if r.err != nil {
		return Err[U](r.err)
	}
	return fn(r.val)
}

// Option adalah nullable eksplisit ala Rust Option<T>.
// Membedakan "kosong" vs "zero value" tanpa pointer.
type Option[T any] struct {
	val T
	ok  bool
}

// Some membuat Option berisi value.
func Some[T any](v T) Option[T] { return Option[T]{val: v, ok: true} }

// None membuat Option kosong.
func None[T any]() Option[T] { return Option[T]{} }

// IsSome true bila ada value.
func (o Option[T]) IsSome() bool { return o.ok }

// IsNone true bila kosong.
func (o Option[T]) IsNone() bool { return !o.ok }

// UnwrapOr mengembalikan value atau fallback.
func (o Option[T]) UnwrapOr(fallback T) T {
	if !o.ok {
		return fallback
	}
	return o.val
}
