package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waBinary "github.com/polymorfa/hypermeow/binary"
	waProto "github.com/polymorfa/hypermeow/binary/proto"
	"github.com/polymorfa/hypermeow/types"
	qrcode "github.com/skip2/go-qrcode"
	"google.golang.org/protobuf/proto"
)

/**
 * 🌐 Web UI Plugin — QR Login di Browser
 * ════════════════════════════════════════════════════════════
 * Mirip pola Baileys yang menampilkan QR di dashboard web.
 *
 * Env:
 *   WEB_PORT  → default 3000
 *
 * Endpoint:
 *   GET /            → Halaman scan QR (auto-refresh 2 detik)
 *   GET /api/status  → JSON { state, message, has_qr }
 *   GET /api/qr.png  → PNG QR code
 *
 * State: starting | waiting_qr | connected | logged_out
 * Terminal QR tetap ditampilkan sebagai cadangan.
 */

type webStatus struct {
	State     string `json:"state"` // waiting_qr | connected | logged_out | starting
	Message   string `json:"message"`
	HasQR     bool   `json:"has_qr"`
	UpdatedAt int64  `json:"updated_at"`
}

var (
	webMu      sync.RWMutex
	webQRCode  string // raw QR string dari events.QR
	webState   = "starting"
	webMessage = "Bot sedang mulai..."
	webClient  *whatsmeow.Client
)

func setWebClient(c *whatsmeow.Client) {
	webMu.Lock()
	defer webMu.Unlock()
	webClient = c
}

func setWebQR(code string) {
	webMu.Lock()
	defer webMu.Unlock()
	webQRCode = code
	webState = "waiting_qr"
	webMessage = "Scan QR dengan WhatsApp → Perangkat Tertaut → Tautkan Perangkat"
}

func setWebConnected(jid string) {
	webMu.Lock()
	defer webMu.Unlock()
	webQRCode = ""
	webState = "connected"
	if jid != "" {
		webMessage = "Terhubung sebagai " + jid
	} else {
		webMessage = "Bot terhubung ke WhatsApp"
	}
}

func setWebLoggedOut() {
	webMu.Lock()
	defer webMu.Unlock()
	webQRCode = ""
	webState = "logged_out"
	webMessage = "Logged out. Restart bot untuk scan QR ulang."
}

func getWebSnapshot() (state, message, qr string) {
	webMu.RLock()
	defer webMu.RUnlock()
	return webState, webMessage, webQRCode
}

func startWebUI() {
	port := os.Getenv("WEB_PORT")
	if port == "" {
		port = "3000"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleWebIndex)
	mux.HandleFunc("/api/status", handleWebStatus)
	mux.HandleFunc("/api/qr.png", handleWebQRPNG)
	mux.HandleFunc("/api/groups", handleWebGroups)
	mux.HandleFunc("/api/groupinfo", handleWebGroupInfo)
	mux.HandleFunc("/api/send", handleWebSend)
	mux.HandleFunc("/api/revoke", handleWebRevoke)
	mux.HandleFunc("/api/paircode", handleWebPairCode)

	addr := ":" + port
	fmt.Printf("🌐 Web UI QR: http://localhost%s\n", addr)
	fmt.Printf("   (bisa diakses dari perangkat lain jika firewall mengizinkan)\n")

	go func() {
		server := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("⚠️  Web UI error: %v\n", err)
		}
	}()
}

func handleWebStatus(w http.ResponseWriter, r *http.Request) {
	state, msg, qr := getWebSnapshot()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"state":%q,"message":%q,"has_qr":%v,"updated_at":%d}`,
		state, msg, qr != "", time.Now().Unix())
}

func handleWebGroups(w http.ResponseWriter, r *http.Request) {
	webMu.RLock()
	c := webClient
	webMu.RUnlock()
	if c == nil || !c.IsConnected() {
		http.Error(w, `{"error":"bot tidak terhubung"}`, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	groups, err := c.GetJoinedGroups(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	type ginfo struct {
		JID     string `json:"jid"`
		Name    string `json:"name"`
		Members int    `json:"members"`
	}
	out := make([]ginfo, 0, len(groups))
	for _, g := range groups {
		out = append(out, ginfo{g.JID.String(), g.Name, len(g.Participants)})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// handleWebGroupInfo: diagnostik grup — mode addressing, komposisi
// partisipan (LID vs PN), dan entri partisipan milik bot sendiri.
// GET /api/groupinfo?jid=<group jid>
func handleWebGroupInfo(w http.ResponseWriter, r *http.Request) {
	webMu.RLock()
	c := webClient
	webMu.RUnlock()
	if c == nil || !c.IsConnected() {
		http.Error(w, `{"error":"bot tidak terhubung"}`, http.StatusServiceUnavailable)
		return
	}
	jidStr := r.URL.Query().Get("jid")
	if jidStr == "" {
		http.Error(w, `{"error":"param jid wajib"}`, http.StatusBadRequest)
		return
	}
	gjid, err := types.ParseJID(jidStr)
	if err != nil {
		http.Error(w, `{"error":"jid tidak valid"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	info, err := c.GetGroupInfo(ctx, gjid)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	ownPN := ""
	ownLID := ""
	if c.Store != nil {
		if c.Store.ID != nil {
			ownPN = c.Store.ID.ToNonAD().String()
		}
		if !c.Store.LID.IsEmpty() {
			ownLID = c.Store.LID.ToNonAD().String()
		}
	}
	lidCount, pnOnlyCount := 0, 0
	var ownEntry map[string]interface{}
	for _, p := range info.Participants {
		if !p.LID.IsEmpty() {
			lidCount++
		} else {
			pnOnlyCount++
		}
		isOwn := (ownPN != "" && p.PhoneNumber.ToNonAD().String() == ownPN) ||
			(ownLID != "" && !p.LID.IsEmpty() && p.LID.ToNonAD().String() == ownLID) ||
			(ownPN != "" && p.JID.ToNonAD().String() == ownPN) ||
			(ownLID != "" && p.JID.ToNonAD().String() == ownLID)
		if isOwn {
			ownEntry = map[string]interface{}{
				"jid":          p.JID.String(),
				"phone_number": p.PhoneNumber.String(),
				"lid":          p.LID.String(),
				"is_admin":     p.IsAdmin,
			}
		}
	}
	out := map[string]interface{}{
		"jid":             info.JID.String(),
		"name":            info.Name,
		"addressing_mode": string(info.AddressingMode),
		"announce":        info.IsAnnounce,
		"incognito":       info.IsIncognito,
		"is_parent":       info.GroupParent.IsParent,
		"linked_parent":   info.GroupLinkedParent.LinkedParentJID.String(),
		"participants":    len(info.Participants),
		"with_lid":        lidCount,
		"pn_only":         pnOnlyCount,
		"own_pn":          ownPN,
		"own_lid":         ownLID,
		"own_entry":       ownEntry,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

func handleWebSend(w http.ResponseWriter, r *http.Request) {
	// POST /api/send {to, text, biz}
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		To   string `json:"to"`
		Text string `json:"text"`
		Biz  bool   `json:"biz"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if req.To == "" || req.Text == "" {
		http.Error(w, `{"error":"to dan text wajib"}`, http.StatusBadRequest)
		return
	}
	webMu.RLock()
	c := webClient
	webMu.RUnlock()
	if c == nil || !c.IsConnected() {
		http.Error(w, `{"error":"bot tidak terhubung"}`, http.StatusServiceUnavailable)
		return
	}
	to, err := parseAnyJID(req.To)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "JID salah"), http.StatusBadRequest)
		return
	}
	msg := &waProto.Message{Conversation: proto.String(req.Text)}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var resp whatsmeow.SendResponse
	if req.Biz {
		bizNode := a2uiBizNode()
		resp, err = c.SendMessage(ctx, to, msg, whatsmeow.SendRequestExtra{
			AdditionalNodes: &[]waBinary.Node{bizNode},
		})
	} else {
		resp, err = c.SendMessage(ctx, to, msg)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"id":%q}`, resp.ID)
}

// handleWebRevoke — POST /api/revoke {chat, id}
// Revoke (hapus untuk semua) sebuah pesan milik bot. Untuk keperluan
// operasional (mis. bersihkan notif ⏳ yang gantung). Hanya via localhost.
func handleWebRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Chat string `json:"chat"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if req.Chat == "" || req.ID == "" {
		http.Error(w, `{"error":"chat dan id wajib"}`, http.StatusBadRequest)
		return
	}
	webMu.RLock()
	c := webClient
	webMu.RUnlock()
	if c == nil || !c.IsConnected() {
		http.Error(w, `{"error":"bot tidak terhubung"}`, http.StatusServiceUnavailable)
		return
	}
	chat, err := parseAnyJID(req.Chat)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "JID salah"), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := c.RevokeMessage(ctx, chat, types.MessageID(req.ID))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"id":%q}`, resp.ID)
}

func handleWebQRPNG(w http.ResponseWriter, r *http.Request) {
	_, _, qr := getWebSnapshot()
	if qr == "" {
		http.Error(w, "QR belum tersedia", http.StatusNotFound)
		return
	}
	png, err := qrcode.Encode(qr, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "gagal generate QR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

// handleWebPairCode — GET /api/paircode?phone=628xxx
// Generate kode pairing 8 digit untuk ditautkan via nomor HP
// (WhatsApp → Perangkat Tertaut → Tautkan dengan nomor telepon).
// Hanya saat belum login. Kode kedaluwarsa ~160 detik.
func handleWebPairCode(w http.ResponseWriter, r *http.Request) {
	c := webClient
	if c == nil {
		http.Error(w, "client belum siap", http.StatusServiceUnavailable)
		return
	}
	if c.IsLoggedIn() {
		http.Error(w, "sudah login", http.StatusBadRequest)
		return
	}
	phone := ""
	for _, ch := range r.URL.Query().Get("phone") {
		if ch >= '0' && ch <= '9' {
			phone += string(ch)
		}
	}
	if len(phone) < 7 {
		http.Error(w, "param phone wajib (format internasional, mis. 62812xxxx)", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 2026-10-27: Default to Android (mobile) pairing — 100% detected as mobile.
	// Optional ?client=web override: c.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	// Optional ?name= override: custom device display name
	clientType := whatsmeow.PairClientAndroid
	displayName := "Chrome (Android)" // Browser (OS) format required by WA
	if r.URL.Query().Get("client") == "web" {
		clientType = whatsmeow.PairClientChrome
		displayName = "Chrome (Linux)"
	}
	if name := r.URL.Query().Get("name"); name != "" {
		displayName = name
	}
	code, err := c.PairPhone(ctx, phone, true, clientType, displayName)
	if err != nil {
		http.Error(w, "gagal generate kode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"ok":true,"code":%q,"phone":%q}`, code, phone)
}

func handleWebIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(webIndexHTML))
}

const webIndexHTML = `<!DOCTYPE html>
<html lang="id">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>WhatsApp Bot — Scan QR</title>
  <style>
    :root {
      --bg: #0b141a;
      --card: #111b21;
      --accent: #00a884;
      --text: #e9edef;
      --muted: #8696a0;
      --border: #222d34;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
      background: radial-gradient(ellipse at top, #1a2a32 0%, var(--bg) 55%);
      color: var(--text);
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 24px;
    }
    .card {
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 16px;
      padding: 32px 28px;
      max-width: 420px;
      width: 100%;
      text-align: center;
      box-shadow: 0 12px 40px rgba(0,0,0,.35);
    }
    .logo {
      width: 48px; height: 48px;
      border-radius: 12px;
      background: var(--accent);
      display: inline-flex; align-items: center; justify-content: center;
      font-size: 24px; margin-bottom: 12px;
    }
    h1 { font-size: 1.25rem; margin: 0 0 6px; font-weight: 600; }
    .sub { color: var(--muted); font-size: .9rem; margin-bottom: 20px; line-height: 1.45; }
    .qr-wrap {
      background: #fff;
      border-radius: 12px;
      padding: 16px;
      display: inline-block;
      min-height: 256px;
      min-width: 256px;
      position: relative;
    }
    .qr-wrap img { display: block; width: 256px; height: 256px; }
    .qr-wrap.hidden { display: none; }
    .status {
      margin-top: 18px;
      padding: 10px 14px;
      border-radius: 8px;
      background: #1a2a32;
      font-size: .9rem;
      color: var(--muted);
    }
    .status.ok { color: var(--accent); }
    .status.warn { color: #f0b429; }
    .status.err { color: #ea4335; }
    .steps {
      text-align: left;
      margin-top: 20px;
      padding: 0;
      list-style: none;
      font-size: .85rem;
      color: var(--muted);
    }
    .steps li { margin: 8px 0; padding-left: 28px; position: relative; }
    .steps li::before {
      content: attr(data-n);
      position: absolute; left: 0;
      width: 20px; height: 20px;
      background: var(--border);
      border-radius: 50%;
      font-size: .7rem;
      display: flex; align-items: center; justify-content: center;
      color: var(--text);
    }
    .spinner {
      width: 40px; height: 40px;
      border: 3px solid var(--border);
      border-top-color: var(--accent);
      border-radius: 50%;
      animation: spin 0.8s linear infinite;
      margin: 100px auto;
    }
    @keyframes spin { to { transform: rotate(360deg); } }
    footer { margin-top: 16px; font-size: .75rem; color: #5a6a72; }
  </style>
</head>
<body>
  <div class="card">
    <div class="logo">💬</div>
    <h1>WhatsApp Bot</h1>
    <p class="sub" id="subtitle">Menyiapkan koneksi...</p>

    <div class="qr-wrap hidden" id="qrBox">
      <img id="qrImg" alt="QR Code" width="256" height="256" />
    </div>
    <div id="loading"><div class="spinner"></div></div>

    <div class="status" id="status">Memuat status...</div>

    <ol class="steps">
      <li data-n="1">Buka WhatsApp di HP</li>
      <li data-n="2">Pengaturan → <b>Perangkat tertaut</b></li>
      <li data-n="3">Ketuk <b>Tautkan perangkat</b></li>
      <li data-n="4">Scan QR di atas</li>
    </ol>
    <footer>QR otomatis diperbarui jika kedaluwarsa</footer>
  </div>

  <script>
    const qrBox = document.getElementById('qrBox');
    const qrImg = document.getElementById('qrImg');
    const loading = document.getElementById('loading');
    const statusEl = document.getElementById('status');
    const subtitle = document.getElementById('subtitle');
    let lastHasQr = null;

    async function tick() {
      try {
        const res = await fetch('/api/status?_=' + Date.now());
        const data = await res.json();
        statusEl.textContent = data.message || data.state;
        statusEl.className = 'status';
        if (data.state === 'connected') {
          statusEl.classList.add('ok');
          subtitle.textContent = 'Bot sudah terhubung';
          qrBox.classList.add('hidden');
          loading.classList.add('hidden');
          loading.style.display = 'none';
        } else if (data.state === 'logged_out') {
          statusEl.classList.add('err');
          subtitle.textContent = 'Sesi berakhir';
          qrBox.classList.add('hidden');
        } else if (data.has_qr) {
          statusEl.classList.add('warn');
          subtitle.textContent = 'Scan QR untuk login';
          loading.style.display = 'none';
          qrBox.classList.remove('hidden');
          // bust cache setiap poll agar QR baru muncul
          qrImg.src = '/api/qr.png?_=' + Date.now();
          lastHasQr = true;
        } else {
          subtitle.textContent = 'Menunggu QR dari server WhatsApp...';
          loading.style.display = 'block';
          qrBox.classList.add('hidden');
        }
      } catch (e) {
        statusEl.textContent = 'Tidak bisa menghubungi bot (refresh halaman)';
        statusEl.className = 'status err';
      }
    }
    tick();
    setInterval(tick, 2000);
  </script>
</body>
</html>
`
