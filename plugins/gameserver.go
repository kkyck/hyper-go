package main

// gameserver.go — Game WebSocket Server + Multiplayer Hub + TTLive Feed
//
// Port dari src/game-ws.ts + src/game-server.ts + plugins/multiplayer.ts + plugins/ttlive.ts
// Fitur WS: RFC 6455 manual, game multiplayer, feed video TikTok-like.
//
// Server ini SHARE port dengan lacak.go (HTTP + WS bersamaan).
// Route WS: /ws (game+TTLive, JSON text frames)
//
// Commands:
//   .multiplayer / .mp     → menu pilihan game
//   .multiplayer on|off    → owner: nyalakan/matikan WS
//   .multiplayer status     → status server + room
//   .multiplayer games      → daftar game
//   .ttl / .ttlive          → feed video TikTok-like via WS
//   .ttl status             → status TTLive
//
// Owner only.

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Game definitions ────────────────────────────────────────────

type Game struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Emoji string `json:"emoji"`
	Desc  string `json:"desc"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
}

var GAMES = []Game{
	{"flappy", "Flappy Royale", "🐤", "Hindari pipa", 1, 8},
	{"snake", "Snake Arena", "🐍", "Makan apel", 1, 4},
	{"pong", "Pong Duel", "🏓", "Pantul bola", 2, 2},
	{"ttt", "Tic-Tac-Toe", "⭕", "3 baris menang", 2, 2},
	{"c4", "Connect 4", "🔴", "Susun 4 beruntun", 2, 2},
	{"react", "Reaction Duel", "⚡", "Tombol tercepat", 2, 2},
	{"quiz", "Quiz Clash", "🧠", "Jawab pertanyaan", 2, 8},
	{"typing", "Typing Race", "⌨️", "Ketik tercepat", 2, 8},
	{"memory", "Memory Match", "🃏", "Cocok kartu", 1, 4},
	{"g2048", "2048 Race", "🔢", "Gabung angka", 1, 4},
	{"wordchain", "Kata Berantai", "🔗", "Sambung kata", 2, 8},
	{"hangman", "Hangman Duel", "🎪", "Tebak huruf", 2, 4},
	{"mines", "Minesweeper", "💣", "Hindari bom", 1, 4},
	{"suit", "Batu Gunting Kertas", "✊", "Classic", 2, 2},
	{"simon", "Simon Battle", "🧠", "Urutan warna", 2, 4},
	{"hilo", "Tebak Angka", "🎯", "Hi/Lo", 2, 2},
}

// ─── WS Server state ─────────────────────────────────────────────

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	conn     net.Conn
	alive    bool
	lastSeen time.Time
	meta     map[string]interface{}
}

type gameRoom struct {
	ID      string    `json:"id"`
	Game    string    `json:"game"`
	Phase   string    `json:"phase"` // "lobby", "playing", "ended"
	Players []string  `json:"players"`
	Host    string    `json:"host"`
	Created time.Time `json:"created"`
}

var (
	gameServerMu     sync.Mutex
	gameServerUp     bool
	gameServerStartT time.Time
	gameConns        = map[net.Conn]*wsConn{}
	gameRooms        = map[string]*gameRoom{}
	ttliveVideos     []string // file paths
	ttliveMu         sync.Mutex
	gameNotifyChatID string
)

// ─── WS handshake ────────────────────────────────────────────────

func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// handleGameWS upgrades HTTP to WebSocket (RFC 6455 minimal).
func handleGameWS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "WS-only", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		conn.Close()
		return
	}

	// Send handshake
	handshake := fmt.Sprintf(
		"HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n",
		wsAcceptKey(key),
	)
	_, _ = conn.Write([]byte(handshake))

	// Register connection
	wc := &wsConn{
		conn:     conn,
		alive:    true,
		lastSeen: time.Now(),
		meta:     map[string]interface{}{},
	}
	gameServerMu.Lock()
	gameConns[conn] = wc
	gameServerMu.Unlock()
	defer func() {
		gameServerMu.Lock()
		delete(gameConns, conn)
		gameServerMu.Unlock()
	}()

	// Read loop
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		msg, err := wsDecodeFrame(buf[:n])
		if err != nil {
			continue
		}
		if msg == "" {
			continue
		}
		wc.lastSeen = time.Now()
		handleGameMessage(wc, msg)
	}
}

// wsDecodeFrame parses a single WebSocket text frame (client→server, masked).
func wsDecodeFrame(data []byte) (string, error) {
	if len(data) < 2 {
		return "", fmt.Errorf("short")
	}
	// Skip if not text frame (opcode 1)
	opcode := data[0] & 0x0F
	if opcode == 8 { // close
		return "", fmt.Errorf("close")
	}
	masked := data[1]&0x80 != 0
	payloadLen := int(data[1] & 0x7F)
	offset := 2
	if payloadLen == 126 {
		if len(data) < 4 {
			return "", fmt.Errorf("short ext")
		}
		payloadLen = int(data[2])<<8 | int(data[3])
		offset = 4
	} else if payloadLen == 127 {
		if len(data) < 10 {
			return "", fmt.Errorf("short ext64")
		}
		// 64-bit length — only use lower 32 bits (safe for our use)
		payloadLen = int(data[6])<<24 | int(data[7])<<16 | int(data[8])<<8 | int(data[9])
		offset = 10
	}
	if masked {
		if len(data) < offset+4 {
			return "", fmt.Errorf("short mask")
		}
		mask := data[offset : offset+4]
		offset += 4
		if len(data) < offset+payloadLen {
			return "", fmt.Errorf("short payload")
		}
		payload := data[offset : offset+payloadLen]
		for i := 0; i < len(payload); i++ {
			payload[i] ^= mask[i%4]
		}
		return string(payload), nil
	}
	if len(data) < offset+payloadLen {
		return "", fmt.Errorf("short payload")
	}
	return string(data[offset : offset+payloadLen]), nil
}

// wsEncodeTextFrame encodes a text frame (server→client, unmasked).
func wsEncodeTextFrame(data string) []byte {
	payload := []byte(data)
	var header []byte
	l := len(payload)
	if l < 126 {
		header = []byte{0x81, byte(l)}
	} else if l < 65536 {
		header = []byte{0x81, 126, byte(l >> 8), byte(l & 0xFF)}
	} else {
		header = make([]byte, 10)
		header[0] = 0x81
		header[1] = 127
		header[6] = byte(l >> 24)
		header[7] = byte(l >> 16)
		header[8] = byte(l >> 8)
		header[9] = byte(l & 0xFF)
	}
	return append(header, payload...)
}

// handleGameMessage processes JSON messages from clients.
func handleGameMessage(wc *wsConn, msg string) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		return
	}
	t, _ := m["type"].(string)
	switch t {
	case "hello":
		name, _ := m["name"].(string)
		wc.meta["name"] = name
		wc.meta["pid"] = fmt.Sprintf("p%d", mathrand.Intn(99999))
		wsSend(wc, map[string]interface{}{
			"type": "welcome",
			"pid":  wc.meta["pid"],
			"name": name,
		})
	case "list":
		gameServerMu.Lock()
		rooms := make([]map[string]interface{}, 0, len(gameRooms))
		for _, r := range gameRooms {
			rooms = append(rooms, map[string]interface{}{
				"id": r.ID, "game": r.Game, "phase": r.Phase,
				"players": r.Players, "host": r.Host,
			})
		}
		gameServerMu.Unlock()
		wsSend(wc, map[string]interface{}{
			"type":  "rooms",
			"rooms": rooms,
		})
	case "create":
		gameID, _ := m["game"].(string)
		roomID := genRoomID()
		pid, _ := wc.meta["pid"].(string)
		name, _ := wc.meta["name"].(string)
		if name == "" {
			name = "Player"
		}
		room := &gameRoom{
			ID: roomID, Game: gameID, Phase: "lobby",
			Players: []string{name}, Host: pid, Created: time.Now(),
		}
		gameServerMu.Lock()
		gameRooms[roomID] = room
		gameServerMu.Unlock()
		wc.meta["room"] = roomID
		wsSend(wc, map[string]interface{}{
			"type": "joined", "room": room, "isHost": true,
		})
	case "join":
		roomID, _ := m["room"].(string)
		name, _ := wc.meta["name"].(string)
		if name == "" {
			name = "Player"
		}
		gameServerMu.Lock()
		room, ok := gameRooms[roomID]
		if ok {
			room.Players = append(room.Players, name)
			wc.meta["room"] = roomID
		}
		gameServerMu.Unlock()
		if ok {
			wsSend(wc, map[string]interface{}{
				"type": "joined", "room": room, "isHost": false,
			})
		}
	case "ready", "start", "leave", "again", "input", "chat":
		// Broadcast to room members
		roomID, _ := wc.meta["room"].(string)
		if roomID == "" {
			return
		}
		broadcastToRoom(roomID, wc, m)
	}
}

// wsSend sends a JSON message to a single WebSocket connection.
func wsSend(wc *wsConn, msg map[string]interface{}) {
	if wc == nil || !wc.alive {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	frame := wsEncodeTextFrame(string(data))
	_, _ = wc.conn.Write(frame)
}

// broadcastToRoom sends a message to all connections in a room (except sender).
func broadcastToRoom(roomID string, sender *wsConn, msg map[string]interface{}) {
	gameServerMu.Lock()
	room, ok := gameRooms[roomID]
	conns := make([]*wsConn, 0, len(gameConns))
	for _, wc := range gameConns {
		if wc == sender {
			continue
		}
		if r, ok := wc.meta["room"].(string); ok && r == roomID {
			conns = append(conns, wc)
		}
	}
	gameServerMu.Unlock()
	if !ok || room == nil {
		return
	}
	msg["room"] = room
	for _, wc := range conns {
		wsSend(wc, msg)
	}
}

func genRoomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// ─── Server lifecycle ────────────────────────────────────────────

func startGameServer() error {
	gameServerMu.Lock()
	defer gameServerMu.Unlock()
	if gameServerUp {
		return nil
	}
	port := wsPort()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handleGameWS)
	mux.HandleFunc("/wss/audio", handleMediaWS)
	mux.HandleFunc("/ttl", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(buildTtliveHTML()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(buildHubHTML("")))
	})
	gameServer := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  0, // no timeout for WS
		WriteTimeout: 0,
	}
	go func() {
		if err := gameServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[gameserver] error: %v\n", err)
			gameServerMu.Lock()
			gameServerUp = false
			gameServerMu.Unlock()
		}
	}()
	gameServerUp = true
	gameServerStartT = time.Now()
	fmt.Printf("[gameserver] UP on port %s (HTTP + WS) — terpisah dari lacak\n", port)
	return nil
}

func stopGameServer() {
	gameServerMu.Lock()
	defer gameServerMu.Unlock()
	gameServerUp = false
	for conn := range gameConns {
		_ = conn.Close()
	}
	gameConns = map[net.Conn]*wsConn{}
	gameRooms = map[string]*gameRoom{}
}

func getGameStatus() map[string]interface{} {
	gameServerMu.Lock()
	defer gameServerMu.Unlock()
	uptime := 0
	if gameServerUp {
		uptime = int(time.Since(gameServerStartT).Seconds())
	}
	return map[string]interface{}{
		"up":      gameServerUp,
		"port":    wsPort(),
		"players": len(gameConns),
		"rooms":   len(gameRooms),
		"uptime":  uptime,
		"games":   len(GAMES),
	}
}

func getGameWsURL() string {
	return wsPublicURL() + "/ws"
}

// ─── Media WS (audio/video streaming via WebSocket) ───────────────

func handleMediaWS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "WS-only", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	defer bufrw.Flush()

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return
	}
	handshake := fmt.Sprintf(
		"HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n",
		wsAcceptKey(key),
	)
	_, _ = conn.Write([]byte(handshake))

	// Read URL from first message
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}
	msg, err := wsDecodeFrame(buf[:n])
	if err != nil {
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		return
	}
	urlStr, _ := m["url"].(string)
	if urlStr == "" {
		return
	}
	// Fetch media
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	// Stream to client in 64KB chunks via binary frames
	chunk := make([]byte, 64*1024)
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			frame := wsEncodeBinaryFrame(chunk[:n])
			_, werr := conn.Write(frame)
			if werr != nil {
				return
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
	}
}

// wsEncodeBinaryFrame encodes a binary frame (opcode 2).
func wsEncodeBinaryFrame(data []byte) []byte {
	var header []byte
	l := len(data)
	if l < 126 {
		header = []byte{0x82, byte(l)}
	} else if l < 65536 {
		header = []byte{0x82, 126, byte(l >> 8), byte(l & 0xFF)}
	} else {
		header = make([]byte, 10)
		header[0] = 0x82
		header[1] = 127
		header[6] = byte(l >> 24)
		header[7] = byte(l >> 16)
		header[8] = byte(l >> 8)
		header[9] = byte(l & 0xFF)
	}
	return append(header, data...)
}

// ─── TTLive ──────────────────────────────────────────────────────

func ttliveRescan() int {
	ttliveMu.Lock()
	defer ttliveMu.Unlock()
	ttliveVideos = nil
	dir := filepath.Join("data", "ttlive")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".webm") {
			ttliveVideos = append(ttliveVideos, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(ttliveVideos)
	return len(ttliveVideos)
}

func ttliveInfo() map[string]interface{} {
	ttliveMu.Lock()
	defer ttliveMu.Unlock()
	totalBytes := int64(0)
	for _, f := range ttliveVideos {
		if st, err := os.Stat(f); err == nil {
			totalBytes += st.Size()
		}
	}
	return map[string]interface{}{
		"videos":     len(ttliveVideos),
		"totalBytes": totalBytes,
		"viewers":    0, // tracked via WS connections
		"dir":        filepath.Join("data", "ttlive"),
	}
}

// ─── Rich card builder ───────────────────────────────────────────

// Meta AI signature (same as TS plugins)
const mbSig = "TklYRUwuTWVzc2FnZUJ1aWxkZXJWNC43LVZlcmlmaWNhdGlvblNpZ25hdHVyZS5NZXRhZGF0YeN55YRyad2+ZA=="
const mbCert1 = "TklYRUwuTWVzc2FnZUJ1aWxkZXJWNC43LUNlcnRpZmljYXRlQ2hhaW4uTWV0YWRhdGEOvtJr968bbpKdZreOTwkk9aPN++XPE60RfuzNLkXXc7LE8BOkJOWRpo2oNXaRJ3uCNJ43HY3A+oetnvHSfcxWqmvvTSrBOI5V1NOD6RMsZ/st1XVPUx83AGps1l5jYBOYzqMNy6un2tToJ2Bt9bXRo29mWLZTu8m7TNY/hISwVpVc5tjSet5U7btPN+dMIx2UvykB1jcbWGsdklheeuz8RXSStNXzeaGvsf1lpZ/ugLE4b2BdmlRNKrY6zLE4qFtRYQoS7axOyQX+4QUyN2m9bfm7urQmn+QRSXJwMO7X5kAJJLbkVGJFt9Pm9VXPwQVrK2aaqiXlpusj+7DfDw00OULmYMmZDTqXM0nUVLxj13z0LhMQoQhhNG8utdUn4uKOFceliTZ/xiP+A54GnX9620641bqw3ctfh9NNXPsTEK8hAUD7FDqUhVntHmoEYYEHq8X1tHHZYP49/f2iezTiE8AUaoZo42/jIWQIKohOGNUib2hEqMkW8NsX8vPihvNuqPc0zKZcl6359YFQdjiiW8kCRD/rsDOr9v1eYLFZKYloFyzFqEgj+jcG/V47elOjShJ5CCPwatXwP6HIloVwtgygFsnOFmCg6Ojoivfoz8Nw1qxFwg5OU2cq/1WbWNELKnaFg4eUWCAIJ/3ZIJsEPkgemZxGhE+hdiNn9dkQYBJs1kx2BxdIkJmQ9vJSKkrMz6lTxwM3IJ9mhmKS6zYdU1ppeAao0/ayte997DQParb/AHLN79g0iW1ad0z8ir5jAl0q3a+UZPTSa4YiSqC2PZ/gfxG5wvL2mKmeKowG0RXjmEp5iNxrni+T/HRLZOoH7y0DQ24nMCPg"
const mbCert2 = "TklYRUwuTWVzc2FnZUJ1aWxkZXJWNC43LUNlcnRpZmljYXRlQ2hhaW4uTWV0YWRhdGHsL0Ccm0ELINFZ2IaBhKaeWnVuh0o6nZLCioCn9xpSADzwIS5VCWO+1eVXT2atJOyf7FYlpB0/JA3Us+aQtekuIkHu/zBXijORZ4ClF4+sF3cSTNg6gY/+6iwLK/zs3bMg+GeJrcI65vVfs95Shxlb2Rd5GRT2/2yBmR6Zkf5QwMJuptUHWtM26WY7/xlkEKGFYDZVqOSylusiOzSALa815sC6dCiHoJNLBEKMlaZZQOk57/+OYoU5zzTaEgLhyvNFHSyAlyQ3SGFtVHAaJZHSmmSPyJowCOB+92Gkk6SWVMsk6FbU8QJWFtlhzV/W/gZ7WzUlS/AKgN0th9/cq20ToFkW7X9c+rtYavufmuieqFhXgaMD8AGsoN9QC/HzNC9D1nydPfFYEUr9BHVy2nF5gM58Y59r2rT8p5LPARIkUp8g+5DLhyW0tdZFZ1305o4AHCayZnp5rjcU2Xi/c1Qf/djBGakmijlMs4aMzKJYD0c4Q8jdI7sNyd876K2wRD+L6KeD2QB3PtCS4P7BWAl5gh5CJ6ZBrwcaKXZqcSjEwm52MqVCgYZdapAaNYUy/QndttzLOGw0xxwuX1hIhMjPnIKZR1kwnqD5EqlHpilrnojRZvjVGN9sZEKmilS8rNstt4HHs/D949W+Q6LRVWiWMc0cT2IugrX+Skxd4En7Gq52UEmuVBrStpN+UpIu20NsVb9lsvuYh3XO441606tOEY2eKcZdTtqrOTNqbbTk0zVn1yhbOCvmfctBNDhTwaC5QMi0f9P0w9wX5g=="

// buildRichCard builds a rich card with GenAIaeacdsnwHtmlPrimitive payload.
func buildRichCard(html, commentText string) *waE2E.Message {
	uuid1 := generateUUID()

	// Build unified response JSON
	unified := map[string]interface{}{
		"response_id": uuid1,
		"sections": []map[string]interface{}{{
			"view_model": map[string]interface{}{
				"primitive": map[string]interface{}{
					"__typename":      "GenAIaeacdsnwHtmlPrimitive",
					"payload":         html,
					"trusted_sources": []string{richCardTrustedSource},
				},
				"__typename": "GenAISingleLayoutViewModel",
			},
		}},
	}
	jsonBytes, _ := json.Marshal(unified)
	// Escape non-ASCII (match TS stringifyEscaped)
	escaped := escapeNonASCII(string(jsonBytes))

	// Use the rich card builder to assemble the full botForwardedMessage
	builder := NewRichCard(botClient)
	builder.AddText(commentText)
	builder.SetUnifiedResponseData([]byte(escaped))
	return builder.Build()
}

// escapeNonASCII replaces non-ASCII chars with \uXXXX.
func escapeNonASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 0x7E {
			b.WriteString(fmt.Sprintf("\\u%04x", r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ─── HTML builders (minimal — focus on WS functionality) ─────────

func buildHubHTML(focus string) string {
	wsURL := getGameWsURL()
	gamesJSON := "["
	for i, g := range GAMES {
		if i > 0 {
			gamesJSON += ","
		}
		gamesJSON += fmt.Sprintf(`{"id":%q,"name":%q,"emoji":%q,"desc":%q,"min":%d,"max":%d}`,
			g.ID, g.Name, g.Emoji, g.Desc, g.Min, g.Max)
	}
	gamesJSON += "]"
	focusJS := "null"
	if focus != "" {
		focusJS = fmt.Sprintf("%q", focus)
	}
	return fmt.Sprintf(`<style>*{box-sizing:border-box;-webkit-tap-highlight-color:transparent;user-select:none}body{margin:0;background:#0a0a0f;font-family:Arial,sans-serif;color:#eee}.wrap{max-width:620px;margin:auto;padding:12px}.card{background:rgba(255,255,255,.06);border:1px solid rgba(255,255,255,.15);border-radius:16px;overflow:hidden}.top{padding:12px 16px;border-bottom:1px solid rgba(255,255,255,.12);display:flex;justify-content:space-between;align-items:center}.dot{width:8px;height:8px;border-radius:50%%;background:#636e72;display:inline-block;margin-right:5px}.dot.on{background:#00e676}.pad{padding:14px}.btn{padding:10px 16px;border:none;border-radius:10px;background:linear-gradient(135deg,#6c5ce7,#a29bfe);color:#fff;font-weight:bold;font-size:13px;cursor:pointer;margin:4px}.btn.gr{background:linear-gradient(135deg,#00b894,#55efc4);color:#08331}.inp{width:100%%;padding:12px;border-radius:10px;border:1px solid rgba(255,255,255,.2);background:rgba(0,0,0,.3);color:#fff;font-size:15px}.gcard{background:rgba(255,255,255,.05);border:1px solid rgba(255,255,255,.1);border-radius:12px;padding:12px;margin:6px 0;display:flex;align-items:center;gap:12px}.gicon{font-size:30px}.h1{font-size:20px;font-weight:bold;color:#fff;margin:0 0 10px}</style>
<div class="wrap"><div class="card">
<div class="top"><span><span class="dot" id="dot"></span>🎮 Game Hub</span><span class="tiny" id="status">connecting…</span></div>
<div class="pad">
<div class="h1" id="title">Multiplayer Games</div>
<input class="inp" id="name" placeholder="Nama kamu" maxlength="20">
<div id="lobby"></div>
<div id="gameArea" style="display:none"></div>
</div></div></div>
<script>
var WS=%s,GAMES=%s,FOCUS=%s;
var ws=null,pid=null,myName="",room=null,isHost=false;
function $(id){return document.getElementById(id)}
function connect(){
 ws=new WebSocket(WS);
 ws.onopen=function(){$('dot').className='dot on';$('status').textContent='connected'};
 ws.onclose=function(){$('dot').className='dot off';$('status').textContent='disconnected';setTimeout(connect,2000)};
 ws.onmessage=function(e){var m=JSON.parse(e.data);handle(m)};
}
function send(m){if(ws&&ws.readyState===1)ws.send(JSON.stringify(m))}
function handle(m){
 if(m.type==='welcome'){pid=m.pid;$('status').textContent='✓ '+m.name}
 else if(m.type==='rooms'){renderLobby(m.rooms)}
 else if(m.type==='joined'){room=m.room;isHost=m.isHost;renderRoom()}
 else if(m.type==='room'){room=m.room;renderRoom()}
 else if(m.type==='start'||m.type==='state'||m.type==='go'||m.type==='ended'||m.type==='err'||m.type==='kick'){$('gameArea').innerHTML='<pre>'+JSON.stringify(m,null,2)+'</pre>'}
}
function renderLobby(rooms){
 var h='';
 if(FOCUS){
  var g=GAMES.find(function(x){return x.id===FOCUS});
  if(g)h+='<div class="gcard"><div class="gicon">'+g.emoji+'</div><div><b>'+g.name+'</b><br><small>'+g.desc+' ('+g.min+'-'+g.max+' pemain)</small></div></div>';
 }else{
  GAMES.forEach(function(g){h+='<div class="gcard"><div class="gicon">'+g.emoji+'</div><div><b>'+g.name+'</b><br><small>'+g.desc+'</small></div></div>'});
 }
 h+='<button class="btn gr" onclick="createRoom()">BUAT ROOM</button>';
 if(rooms&&rooms.length){
  h+='<div style="margin-top:10px"><b>Room aktif:</b></div>';
  rooms.forEach(function(r){h+='<div style="background:rgba(255,255,255,.05);padding:8px;margin:4px 0;border-radius:8px"><b>'+r.id+'</b> · '+r.game+' · '+r.players.join(', ')+' <button class="btn" onclick="joinRoom(\''+r.id+'\')">JOIN</button></div>'});
 }
 $('lobby').innerHTML=h;
}
function renderRoom(){
 if(!room)return;
 var h='<div style="background:rgba(0,184,148,.15);padding:12px;border-radius:10px;margin:6px 0">';
 h+='<b>Room: '+room.id+'</b> · '+room.game+'<br>';
 h+='<b>Players:</b> '+room.players.join(', ')+'<br>';
 if(isHost)h+='<button class="btn gr" onclick="startGame()">MULAI GAME</button>';
 h+='<button class="btn" onclick="leaveRoom()">LEAVE</button>';
 h+='</div>';
 $('lobby').innerHTML=h;
}
function createRoom(){
 myName=$('name').value||'Player';
 send({type:'hello',name:myName});
 var g=FOCUS||GAMES[0].id;
 send({type:'create',game:g});
}
function joinRoom(id){
 myName=$('name').value||'Player';
 send({type:'hello',name:myName});
 send({type:'join',room:id});
}
function startGame(){send({type:'start'})}
function leaveGame(){send({type:'leave'})}
connect();
</script>`, wsURL, gamesJSON, focusJS)
}

func buildTtliveHTML() string {
	wsURL := getGameWsURL()
	return fmt.Sprintf(`<style>*{box-sizing:border-box;margin:0;padding:0;-webkit-tap-highlight-color:transparent;user-select:none}body{background:#000;font-family:Arial,sans-serif;color:#fff;overflow:hidden;height:100vh}.feed{height:100vh;scroll-snap-type:y mandatory;overflow-y:scroll}.video-card{height:100vh;scroll-snap-align:start;position:relative;display:flex;align-items:center;justify-content:center;background:#111}.video-card video{width:100%%;height:100%%;object-fit:cover}.overlay{position:absolute;bottom:80px;left:16px;right:80px;z-index:2}.overlay h3{font-size:18px;margin-bottom:8px;text-shadow:0 2px 4px rgba(0,0,0,.8)}.overlay p{font-size:14px;opacity:.8;text-shadow:0 2px 4px rgba(0,0,0,.8)}.actions{position:absolute;right:16px;bottom:100px;display:flex;flex-direction:column;gap:20px;z-index:2}.action{width:48px;height:48px;border-radius:50%%;background:rgba(0,0,0,.5);display:flex;align-items:center;justify-content:center;font-size:24px;cursor:pointer}.action:active{transform:scale(.9)}.nav{position:absolute;top:50%%;right:16px;transform:translateY(-50%%);display:flex;flex-direction:column;gap:12px;z-index:3}.nav button{width:44px;height:44px;border-radius:50%%;background:rgba(0,0,0,.6);border:none;color:#fff;font-size:20px;cursor:pointer}.loading{position:absolute;top:50%%;left:50%%;transform:translate(-50%%,-50%%);font-size:14px;opacity:.6}</style>
<div class="feed" id="feed"><div class="video-card"><div class="loading">⏳ Memuat video…</div></div></div>
<div class="nav"><button onclick="next()">▼</button><button onclick="prev()">▲</button></div>
<script>
var WS=%s,ws=null,idx=0,videos=[];
function connect(){
 ws=new WebSocket(WS);
 ws.onopen=function(){ws.send(JSON.stringify({type:'ttlive_list'}))};
 ws.onmessage=function(e){var m=JSON.parse(e.data);if(m.type==='ttlive_videos'){videos=m.videos||[];render()}};
 ws.onclose=function(){setTimeout(connect,2000)};
}
function render(){
 var f=document.getElementById('feed');
 if(!videos.length){f.innerHTML='<div class="video-card"><div class="loading">Belum ada video. Taruh MP4 di data/ttlive/ lalu .ttl refresh</div></div>';return}
 f.innerHTML='';
 videos.forEach(function(v){
  var c=document.createElement('div');c.className='video-card';
  c.innerHTML='<video src="'+v.url+'" loop muted playsinline></video><div class="overlay"><h3>'+v.title+'</h3><p>'+v.author+'</p></div>';
  f.appendChild(c);
 });
 // autoplay first
 var v=f.querySelector('video');if(v)v.play();
}
function next(){var f=document.getElementById('feed');f.scrollTop+=window.innerHeight;var v=f.querySelectorAll('video')[Math.round(f.scrollTop/window.innerHeight)];if(v)v.play()}
function prev(){var f=document.getElementById('feed');f.scrollTop-=window.innerHeight;var v=f.querySelectorAll('video')[Math.round(f.scrollTop/window.innerHeight)];if(v)v.play()}
connect();
</script>`, wsURL)
}

// ─── Command handlers ────────────────────────────────────────────

func handleMultiplayerCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	input := strings.TrimSpace(strings.ToLower(args))
	sub := strings.TrimPrefix(input, "-")
	sub = strings.TrimPrefix(sub, ".")

	switch sub {
	case "on":
		if err := startGameServer(); err != nil {
			return fmt.Sprintf("❌ Gagal menyalakan WS: %v", err)
		}
		return fmt.Sprintf("🎮 *Multiplayer ON!*\n\n• WebSocket: %s\n• %d game siap · port %s\n• HTTP + WS BERSAMAAN", getGameWsURL(), len(GAMES), wsPort())
	case "off":
		stopGameServer()
		return "🔴 *Multiplayer OFF*\n\nServer game dimatikan"
	case "status":
		st := getGameStatus()
		return fmt.Sprintf("🎮 *MULTIPLAYER GAME HUB*\n\n• Server: %v\n• Port: %v\n• WebSocket: %s\n• Pemain online: %v\n• Room aktif: %v\n• Uptime: %v detik\n• Game: %d game\n\nKetik .multiplayer untuk menu", st["up"], st["port"], getGameWsURL(), st["players"], st["rooms"], st["uptime"], len(GAMES))
	case "games", "list":
		var b strings.Builder
		b.WriteString(fmt.Sprintf("🎮 *%d GAME MULTIPLAYER*\n\n", len(GAMES)))
		for i, g := range GAMES {
			b.WriteString(fmt.Sprintf("%d. %s *%s* — %s (%d-%d pemain)\n", i+1, g.Emoji, g.Name, g.Desc, g.Min, g.Max))
		}
		b.WriteString("\nKetik .multiplayer <nomor> atau .multiplayer <nama> → kirim 1 kartu game itu.")
		return b.String()
	case "help", "h":
		return `🎮 *Multiplayer — Game Online (WebSocket)*

• .multiplayer — menu pilihan game
• .multiplayer <nomor> — kirim 1 kartu game itu
• .multiplayer <nama> — idem (mis. .multiplayer ttt)
• .multiplayer on|off — nyalakan/matikan server (owner)
• .multiplayer status — status server + room
• .multiplayer games — daftar game + alias

*Cara main:*
1. Owner: .multiplayer on
2. Pilih game: .multiplayer 3 → 1 kartu game terkirim
3. Buka kartu game → isi nama → BUAT room / JOIN room code
4. Host mulai saat semua SIAP

WS-only: semua request dari kartu game ke server WAJIB WebSocket.`
	}

	// Try to find game by number or name
	if sub != "" {
		// Number?
		var n int
		if _, err := fmt.Sscanf(sub, "%d", &n); err == nil && n >= 1 && n <= len(GAMES) {
			g := GAMES[n-1]
			return sendGameCard(client, msg, g.ID, g.Emoji+" "+g.Name)
		}
		// Find by name/alias
		for _, g := range GAMES {
			if strings.EqualFold(g.ID, sub) || strings.EqualFold(g.Name, sub) {
				return sendGameCard(client, msg, g.ID, g.Emoji+" "+g.Name)
			}
		}
	}

	// Default: menu
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🎮 *MULTIPLAYER — %d GAME*\n\n", len(GAMES)))
	st := getGameStatus()
	if st["up"].(bool) {
		b.WriteString(fmt.Sprintf("🟢 Server UP · port %s · %v pemain online\n\n", st["port"], st["players"]))
	} else {
		b.WriteString("🔴 Server MATI — owner: .multiplayer on\n\n")
	}
	for i, g := range GAMES {
		b.WriteString(fmt.Sprintf("%d. %s *%s* — %s\n", i+1, g.Emoji, g.Name, g.Desc))
	}
	b.WriteString("\n*Pilih game:* ketik .multiplayer <nomor> atau .multiplayer <nama>")
	return b.String()
}

func sendGameCard(client *whatsmeow.Client, msg *events.Message, gameID, label string) string {
	st := getGameStatus()
	if !st["up"].(bool) {
		return "🔴 Server belum nyala — .multiplayer on dulu (owner)"
	}
	html := buildHubHTML(gameID)
	card := buildRichCard(html, label)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := client.SendMessage(ctx, msg.Info.Chat, card)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim kartu: %v", err)
	}
	return ""
}

func handleTTLiveCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	input := strings.TrimSpace(strings.ToLower(args))
	sub := strings.TrimPrefix(input, "-")

	switch sub {
	case "status":
		st := getGameStatus()
		info := ttliveInfo()
		return fmt.Sprintf("🎬 *TTLIVE — feed video ala TikTok*\n\n• Server WS: %v\n• WebSocket: %s\n• Video: %v (%v bytes)\n• Folder: %v\n\nKetik .ttl untuk membuka feed.", st["up"], getGameWsURL(), info["videos"], info["totalBytes"], info["dir"])
	case "refresh", "scan":
		n := ttliveRescan()
		if n > 0 {
			return fmt.Sprintf("🎬 TTLive di-refresh: *%d video* siap ditonton", n)
		}
		return "🎬 TTLive di-refresh: belum ada video — taruh MP4 di data/ttlive/ lalu ulangi .ttl refresh"
	case "help", "h":
		return `🎬 *TTLive — Video ala TikTok*

• .ttl — kartu feed video ala TikTok (ketuk ▼ untuk ganti video)
• .ttl cari <kata> — buka kartu dengan hasil cari
• .ttl status — status server + video
• .ttl refresh — rescan video baru (owner)

*Fitur kartu:*
• Tombol ▲▼ untuk ganti video
• 🔊 tombol suara
• Double-tap = like
• Feed TANPA BATAS (loop)

WS-only 100%: semua video dikirim via WebSocket.`
	}

	// Default: send TTLive card
	st := getGameStatus()
	if !st["up"].(bool) {
		if err := startGameServer(); err != nil {
			return fmt.Sprintf("❌ Gagal menyalakan server WS: %v", err)
		}
	}
	ttliveRescan() // ensure videos are loaded
	html := buildTtliveHTML()
	card := buildRichCard(html, "🎬 TTLive — nonton video ala TikTok · via WebSocket 100%")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := client.SendMessage(ctx, msg.Info.Chat, card)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim kartu TTLive: %v", err)
	}
	return ""
}

// ─── Random helpers ──────────────────────────────────────────────

func randInt(min, max int) int {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(max-min)))
	return int(n.Int64()) + min
}

// richCardTrustedSource — sumber konten yang dianggap tepercaya pada kartu
// rich HTML. Bisa diganti lewat env RICH_CARD_TRUSTED_SOURCE.
var richCardTrustedSource = getenvDefault("RICH_CARD_TRUSTED_SOURCE", "localhost")
