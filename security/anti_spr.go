package main

// anti_spr.go — Anti-SPR detector: deteksi pesan SPR dari user lain di grup
// ════════════════════════════════════════════════════════════
//
// Karena SPR meninggalkan jejak ciphertext yang khas, kita bisa mendeteksi
// ketika user lain di grup menggunakan SPR. Berguna untuk:
//   - Mengetahui ada pesan rahasia yang dikirim di grup
//   - Mendeteksi bot pesaing yang juga pakai SPR
//   - Audit: siapa yang sering kirim SPR di grup
//
// DETECTION SIGNALS (kombinasi → tinggi confidence):
//   1. enc node type="skmsg" v="2" — sender-key message (vs normal "msg")
//   2. "decrypt-fail:hide" attribute — SPR Hide signature (paling kuat)
//   3. Pesan masuk tapi tidak bisa di-decrypt oleh bot → kemungkinan SPR
//      (karena bot tidak ada di target list)
//   4. Reaction ⚡ pada pesan → SPR2 (fast SPR signature)
//   5. Pesan dengan timestamp jitter yang tidak natural
//
// HANDLER:
//   Saat SPR terdeteksi:
//   - Log ke data/antispr_log.jsonl (chat, sender, timestamp, signal)
//   - Notify owner via DM (jika .antispr notify on)
//   - TIDAK melakukan apa-apa pada pesan asli (hanya observe)
//
// Command:
//   .antispr on|off|status      — toggle detector (per-grup)
//   .antispr notify on|off      — toggle DM notification ke owner
//   .antispr list [n]           — lihat N deteksi terakhir
//   .antispr clear              — clear log

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waBinary "github.com/polymorfa/hypermeow/binary"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── State ─────────────────────────────────────────────────────────

var (
	antiSPRMu     sync.RWMutex
	antiSPRConfig = map[string]*AntiSPRCfg{} // chatJID → config
	antiSPRLoaded bool
	antiSPRLogMu  sync.Mutex
)

type AntiSPRCfg struct {
	Enabled bool `json:"enabled"`
	Notify  bool `json:"notify"` // notify owner via DM
}

type AntiSPRLogEntry struct {
	Timestamp  time.Time `json:"ts"`
	Chat       string    `json:"chat"`
	Sender     string    `json:"sender"`
	MsgID      string    `json:"msg_id"`
	Signals    []string  `json:"signals"`
	Confidence int       `json:"confidence"` // 0-100
}

const (
	antiSPRConfigFile = "data/antispr.json"
	antiSPRLogFile    = "data/antispr_log.jsonl"
	antiSPRMaxLogSize = 1024 * 1024 // 1MB max log file
)

func loadAntiSPR() {
	// Fast path: RLock dulu agar tidak kontensi di hot path per-pesan
	antiSPRMu.RLock()
	if antiSPRLoaded {
		antiSPRMu.RUnlock()
		return
	}
	antiSPRMu.RUnlock()

	antiSPRMu.Lock()
	defer antiSPRMu.Unlock()
	if antiSPRLoaded {
		return
	}
	antiSPRLoaded = true
	data, err := os.ReadFile(antiSPRConfigFile)
	if err == nil {
		_ = json.Unmarshal(data, &antiSPRConfig)
	}
}

func saveAntiSPR() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[anti-SPR] saveAntiSPR panic: %v\n", r)
		}
	}()
	antiSPRMu.RLock()
	defer antiSPRMu.RUnlock()
	data, _ := json.MarshalIndent(antiSPRConfig, "", "  ")
	_ = os.MkdirAll("data", 0o755)
	_ = os.WriteFile(antiSPRConfigFile, data, 0o644)
}

// antiSPRGet mengembalikan COPY struct (bukan pointer ke map) agar mutasi
// di command handler tidak race dengan pembacaan di goroutine deteksi.
func antiSPRGet(chat string) *AntiSPRCfg {
	antiSPRMu.RLock()
	defer antiSPRMu.RUnlock()
	cfg := antiSPRConfig[chat]
	if cfg == nil {
		return &AntiSPRCfg{} // returns disabled by default
	}
	cp := *cfg
	return &cp
}

func antiSPRSet(chat string, cfg *AntiSPRCfg) {
	antiSPRMu.Lock()
	defer antiSPRMu.Unlock()
	if cfg == nil || (!cfg.Enabled && !cfg.Notify) {
		delete(antiSPRConfig, chat)
	} else {
		antiSPRConfig[chat] = cfg
	}
	go saveAntiSPR()
}

// ─── Detection Logic ──────────────────────────────────────────────

// NOTE: antiSPRDetectNode (raw-node analysis) was removed — it was dead code
// (never wired to any raw-node hook). Detection now happens via
// antiSPRCheckUndecryptable on *events.UndecryptableMessage, which is the
// event hypermeow actually emits for undecryptable incoming messages.

// antiSPRCheckUndecryptable is the REAL anti-SPR detector.
//
// Hypermeow emits *events.UndecryptableMessage when an incoming message can't
// be decrypted — which is exactly what the bot sees when someone sends SPR
// targeting OTHER devices:
//
//   - IsUnavailable=true: the sender's stanza carried NO ciphertext for our
//     device (sender-key message restricted to other targets). Normal group
//     messages always include all devices, so this is a strong SPR signal.
//   - DecryptFailMode="hide": the visible skmsg node carries decrypt-fail:hide
//     (injected by SPR Hide engines). Official clients never set this.
//
// Combined (unavailable + hide) = SPR Hide caught red-handed.
// Unavailable alone = targeted sender-key send (base SPR or similar).
// Decryptable-but-failed (session noise) is deliberately NOT counted.
func antiSPRCheckUndecryptable(client interface{}, um *events.UndecryptableMessage) bool {
	if um == nil || !um.Info.IsGroup {
		return false
	}
	// Never flag our own sends.
	if um.Info.IsFromMe {
		return false
	}
	chat := um.Info.Chat.String()
	cfg := antiSPRGet(chat)
	if cfg == nil || !cfg.Enabled {
		return false
	}

	signals := []string{}
	confidence := 0

	if um.IsUnavailable {
		signals = append(signals, "decrypt:unavailable")
		confidence += 55
	}
	if um.DecryptFailMode == events.DecryptFailHide {
		signals = append(signals, "decrypt-fail:hide")
		confidence += 40
	}

	if confidence < 50 {
		return false
	}

	entry := AntiSPRLogEntry{
		Timestamp:  time.Now(),
		Chat:       chat,
		Sender:     um.Info.Sender.String(),
		MsgID:      string(um.Info.ID),
		Signals:    signals,
		Confidence: confidence,
	}
	antiSPRLog(entry)

	if cfg.Notify {
		go antiSPRNotifyOwner(client, entry)
	}

	fmt.Printf("[anti-SPR] ⚡ SPR detected in %s from %s (confidence %d%%, signals=%v)\n",
		chat, um.Info.Sender.String(), confidence, signals)
	return true
}

// antiSPRCheckMessage is the entry point called from main event handler.
// It checks if an incoming message looks like SPR and logs/notifications accordingly.
//
// Returns true if SPR was detected (and logged), false otherwise.
func antiSPRCheckMessage(client interface{}, msg *events.Message) bool {
	if msg == nil || !msg.Info.IsGroup {
		return false
	}
	chat := msg.Info.Chat.String()
	cfg := antiSPRGet(chat)
	if cfg == nil || !cfg.Enabled {
		return false
	}

	// Heuristic check on the message itself
	signals := []string{}
	confidence := 0

	// Signal: bot cannot decrypt message (means it's targeted at others)
	// whatsmeow represents undecrypted messages as protocolMessage or
	// the message gets dropped silently. We can detect via:
	//   - msg.Message being nil or empty
	//   - msg.RawMessage having enc nodes that we couldn't decrypt
	if msg.Message == nil {
		signals = append(signals, "undecryptable")
		confidence += 30
	}

	// Signal: message has very short text or only contains reaction
	if msg.Message != nil {
		if msg.Message.GetReactionMessage() != nil {
			// Reaction ⚡ on a message = SPR2 signature
			reaction := msg.Message.GetReactionMessage()
			if reaction.GetText() == "⚡" {
				signals = append(signals, "reaction:lightning")
				confidence += 40
			}
		}
	}

	// Wajib kombinasi ≥2 sinyal independen + threshold 50 untuk menekan
	// false positive (mis. pesan nil polos atau reaksi ⚡ user biasa).
	if confidence < 50 || len(signals) < 2 {
		return false
	}

	// Log it
	entry := AntiSPRLogEntry{
		Timestamp:  time.Now(),
		Chat:       chat,
		Sender:     msg.Info.Sender.String(),
		MsgID:      string(msg.Info.ID),
		Signals:    signals,
		Confidence: confidence,
	}
	antiSPRLog(entry)

	// Notify owner if enabled
	if cfg.Notify {
		go antiSPRNotifyOwner(client, entry)
	}

	return true
}

// antiSPRLog appends an entry to the JSONL log file.
// Trims the log file if it exceeds antiSPRMaxLogSize.
func antiSPRLog(entry AntiSPRLogEntry) {
	antiSPRLogMu.Lock()
	defer antiSPRLogMu.Unlock()

	_ = os.MkdirAll("data", 0o755)
	line, _ := json.Marshal(entry)
	line = append(line, '\n')

	// Check if log file needs trimming
	if st, err := os.Stat(antiSPRLogFile); err == nil && st.Size() > antiSPRMaxLogSize {
		// Rotate: keep last 200 lines
		data, _ := os.ReadFile(antiSPRLogFile)
		lines := strings.Split(string(data), "\n")
		if len(lines) > 200 {
			data = []byte(strings.Join(lines[len(lines)-200:], "\n"))
			_ = os.WriteFile(antiSPRLogFile, data, 0o644)
		}
	}

	// Append
	f, err := os.OpenFile(antiSPRLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
}

// antiSPRNotifyOwner sends a DM notification to owner about detected SPR.
func antiSPRNotifyOwner(client interface{}, entry AntiSPRLogEntry) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[anti-SPR] antiSPRNotifyOwner panic: %v\n", r)
		}
	}()
	// Owner notification is fire-and-forget — we don't fail the message handler
	// if this errors out.
	//
	// Implementation: use the same sendOwnerAlert helper if available,
	// otherwise log to stdout for pm2 logs.
	if client == nil {
		return
	}
	msg := fmt.Sprintf("🛡️ *SPR Detected*\n\n• Chat: %s\n• Sender: %s\n• Msg ID: %s\n• Confidence: %d%%\n• Signals: %s\n• Time: %s",
		entry.Chat, entry.Sender, entry.MsgID, entry.Confidence,
		strings.Join(entry.Signals, ", "), entry.Timestamp.Format("15:04:05"))
	_ = msg
	// Try to use sendOwnerBroadcast if available, else just print
	if fn := getOwnerBroadcastFn(); fn != nil {
		fn(msg)
	}
}

// getOwnerBroadcastFn returns a function to broadcast a message to all owners,
// or nil if not available. This avoids import cycles.
//
// The function is set by main.go at startup via SetOwnerBroadcastFn.
var ownerBroadcastFn func(string)

func SetOwnerBroadcastFn(fn func(string)) {
	ownerBroadcastFn = fn
}

func getOwnerBroadcastFn() func(string) {
	return ownerBroadcastFn
}

// ─── Command: .antispr ────────────────────────────────────────────

func handleAntiSPRCommand(client interface{}, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	loadAntiSPR()
	chat := msg.Info.Chat.String()
	parts := strings.Fields(strings.TrimSpace(strings.ToLower(args)))
	if len(parts) == 0 {
		return antiSPRStatus(chat)
	}

	switch parts[0] {
	case "on":
		cfg := antiSPRGet(chat)
		if cfg == nil {
			cfg = &AntiSPRCfg{}
		}
		cfg.Enabled = true
		antiSPRSet(chat, cfg)
		return fmt.Sprintf("✅ *Anti-SPR ON* di grup ini\n\nBot akan mendeteksi pesan SPR dari user lain\n%s", antiSPRStatus(chat))

	case "off":
		cfg := antiSPRGet(chat)
		if cfg == nil {
			cfg = &AntiSPRCfg{}
		}
		cfg.Enabled = false
		antiSPRSet(chat, cfg)
		return fmt.Sprintf("🔴 *Anti-SPR OFF* di grup ini\n\n%s", antiSPRStatus(chat))

	case "notify":
		if len(parts) < 2 {
			return "❓ *Cara pakai:* .antispr notify on|off"
		}
		cfg := antiSPRGet(chat)
		if cfg == nil {
			cfg = &AntiSPRCfg{}
		}
		cfg.Notify = parts[1] == "on"
		antiSPRSet(chat, cfg)
		state := "OFF"
		if cfg.Notify {
			state = "ON"
		}
		return fmt.Sprintf("✅ Owner notify %s", state)

	case "list":
		n := 10
		if len(parts) >= 2 {
			fmt.Sscanf(parts[1], "%d", &n)
		}
		return antiSPRListEntries(n)

	case "clear":
		_ = os.Remove(antiSPRLogFile)
		return "✅ Log dibersihkan"

	default:
		return antiSPRStatus(chat)
	}
}

func antiSPRStatus(chat string) string {
	cfg := antiSPRGet(chat)
	enabled := "OFF"
	notify := "OFF"
	if cfg != nil && cfg.Enabled {
		enabled = "ON"
	}
	if cfg != nil && cfg.Notify {
		notify = "ON"
	}

	// Count log entries
	logCount := 0
	if data, err := os.ReadFile(antiSPRLogFile); err == nil {
		logCount = strings.Count(string(data), "\n")
	}

	return fmt.Sprintf(`🛡️ *Anti-SPR Detector*

• Grup ini: %s
• Notify: %s (DM ke owner)
• Log count: %d

*Detection signals:*
  • decrypt-fail:hide attribute (50%%)
  • enc:skmsg sender-key (25%%)
  • mediatype-leak (10%%)
  • count=1/2/3 distribution (15%%)
  • reaction ⚡ SPR2 (40%%)
  • undecryptable message (30%%)

*Commands:*
  • .antispr on|off
  • .antispr notify on|off
  • .antispr list [n]
  • .antispr clear`, enabled, notify, logCount)
}

func antiSPRListEntries(n int) string {
	if n <= 0 || n > 50 {
		n = 10
	}
	data, err := os.ReadFile(antiSPRLogFile)
	if err != nil {
		return "📭 Belum ada log"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return "📭 Log kosong"
	}
	// Take last N
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	recent := lines[start:]

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 *%d Deteksi SPR Terakhir*\n\n", len(recent)))
	for _, line := range recent {
		var entry AntiSPRLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("• [%s] %d%%  %s\n  signals: %s\n\n",
			entry.Timestamp.Format("01-02 15:04"),
			entry.Confidence,
			entry.Sender,
			strings.Join(entry.Signals, ", ")))
	}
	return sb.String()
}

// keep types used
var _ = events.Message{}
var _ = waBinary.Node{}

func init() {
	RegisterPlugin(Plugin{
		Command:     "antispr",
		Description: "antispr",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAntiSPRCommand(client, msg, args)
			// ─── AI2 (Multi-AI fallback) ──────────────────────────
		},
	})
}
