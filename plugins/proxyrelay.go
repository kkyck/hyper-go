package main

// proxyrelay.go — localhost forward proxy tanpa auth yang meneruskan ke
// egress proxy VM (menyuntikkan Proxy-Authorization).
//
// Latar: Chrome headless (dipakai .tt search via chromedp) tidak bisa memakai
// kredensial proxy dari env secara andal, dan tidak mengenal CA bundle
// runtime (SSL_CERT_FILE) sehingga gagal TLS. Solusi: bot membuka listener
// 127.0.0.1 (port acak) sekali per proses; Chrome diarahkan ke sana lewat
// chromedp.ProxyServer. Semua kredensial dibaca dari env saat runtime,
// tidak disimpan di file.

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

var (
	proxyRelayOnce sync.Once
	proxyRelayAddr string
)

// ensureProxyRelay memastikan relay berjalan; mengembalikan "127.0.0.1:port"
// atau "" bila egress proxy tidak terdeteksi.
func ensureProxyRelay() string {
	proxyRelayOnce.Do(func() {
		proxyRelayAddr = startProxyRelay()
	})
	return proxyRelayAddr
}

func egressProxyURL() *url.URL {
	for _, k := range []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY"} {
		if v := os.Getenv(k); v != "" {
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				return u
			}
		}
	}
	return nil
}

func startProxyRelay() string {
	up := egressProxyURL()
	if up == nil || up.User == nil {
		return ""
	}
	user := up.User.Username()
	pass, _ := up.User.Password()
	if user == "" {
		return ""
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	upstream := up.Host

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ""
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleRelayConn(c, upstream, auth)
		}
	}()
	return ln.Addr().String()
}

func handleRelayConn(client net.Conn, upstream, auth string) {
	defer client.Close()
	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method == http.MethodConnect {
		handleRelayConnect(client, br, req, upstream, auth)
		return
	}
	// HTTP biasa: teruskan satu request-response dengan Proxy-Authorization.
	upConn, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer upConn.Close()
	req.RequestURI = ""
	req.Header.Set("Proxy-Authorization", auth)
	req.Close = true
	if err := req.Write(upConn); err != nil {
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(upConn), req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	resp.Close = true
	resp.Write(client)
}

func handleRelayConnect(client net.Conn, br *bufio.Reader, req *http.Request, upstream, auth string) {
	upConn, err := net.Dial("tcp", upstream)
	if err != nil {
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upConn.Close()

	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	fmt.Fprintf(upConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		host, host, auth)

	upBr := bufio.NewReader(upConn)
	// Baca status line + header secara manual. JANGAN pakai http.ReadResponse:
	// Go menganggap body respons 200 CONNECT sebagai unbounded (baca sampai
	// close) sehingga io.Copy(resp.Body) akan memblokir dan proxy menutup
	// tunnel karena tidak ada TLS yang datang.
	statusLine, err := upBr.ReadString('\n')
	if err != nil {
		return
	}
	for {
		line, err := upBr.ReadString('\n')
		if err != nil || line == "\r\n" || line == "\n" {
			break
		}
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") && !strings.HasPrefix(statusLine, "HTTP/1.0 200") {
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}

	fmt.Fprintf(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	// splice; upBr dipakai agar byte yang sudah ter-buffer tidak hilang
	go io.Copy(upConn, br)
	io.Copy(client, upBr)
}
