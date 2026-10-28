// New content for core/socks5_go.go — replaces the old file entirely.
// Changes vs old:
//   - socks5ProxyAddr() default = "" (no proxy when env not set; old default was dead 31.56.78.41:1080)
//   - Added socks5HTTPClientFixed(addr, timeout) for plugins that must always use a specific proxy
//     (e.g. .fetch) regardless of env SOCKS5_PROXY.
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/proxy"
)

// socks5ProxyAddr returns SOCKS5 proxy address from env.
// Empty env var (or unset) = direct connection (no proxy).
// To force a specific proxy only for ONE plugin, use socks5HTTPClientFixed()
// inside that plugin instead of relying on env.
func socks5ProxyAddr() string {
	if v, ok := os.LookupEnv("SOCKS5_PROXY"); ok {
		return v // bisa kosong = disable
	}
	return ""
}

// socks5DialContext membuat DialContext yang mencoba SOCKS5 proxy dulu,
// lalu FALLBACK ke direct bila proxy gagal (proxy mati / SOCKS error).
// Proxy rusak tidak boleh bikin request gagal total.
// proxyAttemptTimeout membatasi SATU percobaan koneksi via proxy.
const proxyAttemptTimeout = 6 * time.Second

func socks5DialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	direct := &net.Dialer{Timeout: 30 * time.Second}
	proxyAddr := socks5ProxyAddr()
	if proxyAddr == "" {
		return direct.DialContext
	}
	forward := &net.Dialer{Timeout: proxyAttemptTimeout}
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, forward)
	if err != nil {
		return direct.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		type dialResult struct {
			conn net.Conn
			err  error
		}
		ch := make(chan dialResult, 1)
		go func() {
			conn, err := dialer.Dial(network, addr)
			ch <- dialResult{conn, err}
		}()
		select {
		case r := <-ch:
			if r.err == nil {
				return r.conn, nil
			}
			// Proxy gagal (mati / general SOCKS failure) → direct.
		case <-time.After(proxyAttemptTimeout):
			// Proxy menggantung → jangan tunggu, langsung direct.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return direct.DialContext(ctx, network, addr)
	}
}

// socks5HTTPClient returns HTTP client yang lewat SOCKS5 proxy (jika env
// SOCKS5_PROXY diset), dengan fallback direct otomatis bila proxy bermasalah.
func socks5HTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext: socks5DialContext(),
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// socks5HTTPClientFixed returns HTTP client yang lewat SOCKS5 proxy
// dengan address eksplisit (tidak baca env SOCKS5_PROXY). Untuk plugin
// yang harus selalu pakai proxy tertentu (mis. .fetch) terlepas dari
// konfigurasi env global. Fallback ke direct otomatis bila proxy gagal.
func socks5HTTPClientFixed(addr string, timeout time.Duration) *http.Client {
	if addr == "" {
		return &http.Client{Timeout: timeout}
	}
	direct := &net.Dialer{Timeout: 30 * time.Second}
	forward := &net.Dialer{Timeout: proxyAttemptTimeout}
	dialer, err := proxy.SOCKS5("tcp", addr, nil, forward)
	if err != nil {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, dst string) (net.Conn, error) {
				type dialResult struct {
					conn net.Conn
					err  error
				}
				ch := make(chan dialResult, 1)
				go func() {
					conn, err := dialer.Dial(network, dst)
					ch <- dialResult{conn, err}
				}()
				select {
				case r := <-ch:
					if r.err == nil {
						return r.conn, nil
					}
					// Proxy gagal → direct fallback.
				case <-time.After(proxyAttemptTimeout):
					// Proxy hang → direct fallback.
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return direct.DialContext(ctx, network, dst)
			},
		},
		Timeout: timeout,
	}
}
