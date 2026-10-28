package main

// shared_http.go — HTTP client bersama untuk seluruh plugin.
// ══════════════════════════════════════════════════════════════════
//
// Satu instance http.Client dipakai bersama supaya koneksi keep-alive
// dipakai ulang antar-request (hemat socket & handshake TLS).
//
// Timeout default 30 detik. Timeout khusus per-request tetap bisa dipakai
// dengan context.WithTimeout di pemanggil.

import (
	"net/http"
	"time"
)

// httpClient adalah HTTP client bersama dengan timeout default 30 detik.
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}
