package main

// responsemode.go — Response Mode (whitelist via SPR / default normal)
//
// .responsemode whitelist → hanya orang yang di-whitelist yang bisa lihat
//                            balasan bot. Pakai SPR (decrypt-fail:hide).
// .responsemode default   → mode normal, semua orang lihat balasan.
//
// .whitelist add @tag     → tambah user ke whitelist
// .whitelist del @tag     → hapus user dari whitelist
// .whitelist list          → lihat daftar whitelist
//
// Owner wajib whitelist secara default.
//
// Storage: data/responsemode.json
//   { "mode": "default"|"whitelist", "whitelist": ["jid1", "jid2"] }

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── State ────────────────────────────────────────────────────────

type ResponseModeConfig struct {
	Mode      string   `json:"mode"`      // "default" or "whitelist"
	Whitelist []string `json:"whitelist"` // list of JIDs
}

var (
	responseModeMu     sync.RWMutex
	responseModeCache  = map[string]*ResponseModeConfig{} // chat JID → config
	responseModeLoaded bool
)

func responseModePath() string {
	return filepath.Join("data", "responsemode.json")
}

func loadResponseMode() {
	responseModeMu.Lock()
	defer responseModeMu.Unlock()
	if responseModeLoaded {
		return
	}
	responseModeLoaded = true
	data, err := os.ReadFile(responseModePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &responseModeCache)
	// Ensure owner is always in whitelist
	for _, cfg := range responseModeCache {
		responseModeEnsureOwner(cfg)
	}
}

func saveResponseMode() {
	responseModeMu.RLock()
	defer responseModeMu.RUnlock()
	// Ensure owner in all whitelists before save
	for _, cfg := range responseModeCache {
		responseModeEnsureOwner(cfg)
	}
	data, _ := json.MarshalIndent(responseModeCache, "", "  ")
	_ = os.MkdirAll("data", 0o755)
	_ = os.WriteFile(responseModePath(), data, 0o644)
}

// responseModeEnsureOwner adds owner JID to whitelist if not present.
func responseModeEnsureOwner(cfg *ResponseModeConfig) {
	ownerJID := os.Getenv("OWNER_JID")
	if ownerJID == "" {
		// No owner configured — nothing to whitelist.
		return
	}
	for _, jid := range cfg.Whitelist {
		if jid == ownerJID {
			return
		}
	}
	cfg.Whitelist = append(cfg.Whitelist, ownerJID)
}

func getResponseMode(chat string) *ResponseModeConfig {
	responseModeMu.RLock()
	defer responseModeMu.RUnlock()
	cfg, ok := responseModeCache[chat]
	if !ok {
		return &ResponseModeConfig{Mode: "default", Whitelist: []string{}}
	}
	return cfg
}

func setResponseMode(chat, mode string) {
	responseModeMu.Lock()
	defer responseModeMu.Unlock()
	cfg, ok := responseModeCache[chat]
	if !ok {
		cfg = &ResponseModeConfig{Mode: "default", Whitelist: []string{}}
		responseModeCache[chat] = cfg
	}
	cfg.Mode = mode
	responseModeEnsureOwner(cfg)
	go saveResponseMode()
}

func whitelistAdd(chat, jid string) bool {
	responseModeMu.Lock()
	defer responseModeMu.Unlock()
	cfg, ok := responseModeCache[chat]
	if !ok {
		cfg = &ResponseModeConfig{Mode: "default", Whitelist: []string{}}
		responseModeCache[chat] = cfg
	}
	// Check if already in whitelist
	for _, j := range cfg.Whitelist {
		if j == jid {
			return false // already exists
		}
	}
	cfg.Whitelist = append(cfg.Whitelist, jid)
	responseModeEnsureOwner(cfg)
	go saveResponseMode()
	return true
}

func whitelistDel(chat, jid string) bool {
	responseModeMu.Lock()
	defer responseModeMu.Unlock()
	cfg, ok := responseModeCache[chat]
	if !ok {
		return false
	}
	// Don't allow removing owner
	ownerJID := os.Getenv("OWNER_JID")
	if ownerJID != "" && jid == ownerJID {
		return false
	}
	for i, j := range cfg.Whitelist {
		if j == jid {
			cfg.Whitelist = append(cfg.Whitelist[:i], cfg.Whitelist[i+1:]...)
			go saveResponseMode()
			return true
		}
	}
	return false
}

func isWhitelisted(chat, jid string) bool {
	cfg := getResponseMode(chat)
	if cfg.Mode != "whitelist" {
		return true // default mode → everyone allowed
	}
	// Owner always allowed
	ownerJID := os.Getenv("OWNER_JID")
	if ownerJID != "" && jid == ownerJID {
		return true
	}
	for _, j := range cfg.Whitelist {
		if j == jid {
			return true
		}
	}
	return false
}

// isResponseModeWhitelist checks if a chat is in whitelist mode.
// Always reloads config from file to ensure fresh state.
func isResponseModeWhitelist(chat string) bool {
	// Force reload config to get fresh state
	responseModeMu.Lock()
	responseModeLoaded = false
	responseModeMu.Unlock()
	loadResponseMode()
	cfg := getResponseMode(chat)
	return cfg.Mode == "whitelist"
}

// ─── Command handlers ─────────────────────────────────────────────

func handleResponseModeCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner/admin grup"
	}

	loadResponseMode()
	sub := strings.ToLower(strings.TrimSpace(args))
	chat := msg.Info.Chat.String()
	cfg := getResponseMode(chat)

	switch sub {
	case "whitelist":
		setResponseMode(chat, "whitelist")
		return fmt.Sprintf("✅ *Response Mode: WHITELIST*\n\nHanya user yang di-whitelist yang bisa lihat balasan bot.\n\nWhitelist saat ini: %d user\n\nKelola:\n• .whitelist add @tag\n• .whitelist del @tag\n• .whitelist list", len(cfg.Whitelist))

	case "default":
		setResponseMode(chat, "default")
		return "✅ *Response Mode: DEFAULT*\n\nMode normal — semua orang lihat balasan bot."

	case "status":
		mode := cfg.Mode
		if mode == "" {
			mode = "default"
		}
		return fmt.Sprintf("⚙️ *Response Mode*\n\nMode: *%s*\nWhitelist: %d user\n\n• .responsemode whitelist\n• .responsemode default\n• .whitelist add/del/list", mode, len(cfg.Whitelist))

	default:
		return `⚙️ *Response Mode*

Mode:
• .responsemode whitelist → hanya whitelist yang lihat (via SPR)
• .responsemode default → normal, semua lihat

Whitelist:
• .whitelist add @tag → tambah user
• .whitelist del @tag → hapus user
• .whitelist list → lihat daftar

Owner wajib whitelist secara default.`
	}
}

func handleWhitelistCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner/admin grup"
	}

	loadResponseMode()
	sub := strings.Fields(strings.ToLower(args))
	chat := msg.Info.Chat.String()

	if len(sub) == 0 {
		return "❓ *Cara pakai:*\n• .whitelist add @tag\n• .whitelist del @tag\n• .whitelist list"
	}

	switch sub[0] {
	case "add":
		// Get mentioned JIDs
		mentions := extractMentionedJIDs(msg)
		if len(mentions) == 0 {
			return "❓ Tag user yang ingin di-whitelist\nContoh: *.whitelist add @user*"
		}
		var added []string
		var failed []string
		for _, m := range mentions {
			j, err := parseAnyJID(m)
			if err != nil || j.IsEmpty() {
				failed = append(failed, m)
				continue
			}
			jid := j.ToNonAD().String()
			if whitelistAdd(chat, jid) {
				added = append(added, jid)
			} else {
				failed = append(failed, jid)
			}
		}
		result := fmt.Sprintf("✅ *Whitelist Add*\n\n• Ditambahkan: %d user\n", len(added))
		if len(added) > 0 {
			result += "• " + strings.Join(added, "\n• ") + "\n"
		}
		if len(failed) > 0 {
			result += fmt.Sprintf("\n• Sudah ada / gagal: %d\n", len(failed))
		}
		return result

	case "del", "delete", "remove":
		mentions := extractMentionedJIDs(msg)
		if len(mentions) == 0 {
			return "❓ Tag user yang ingin dihapus dari whitelist\nContoh: *.whitelist del @user*"
		}
		var removed []string
		var failed []string
		for _, m := range mentions {
			j, err := parseAnyJID(m)
			if err != nil || j.IsEmpty() {
				failed = append(failed, m)
				continue
			}
			jid := j.ToNonAD().String()
			if whitelistDel(chat, jid) {
				removed = append(removed, jid)
			} else {
				failed = append(failed, jid)
			}
		}
		result := fmt.Sprintf("✅ *Whitelist Delete*\n\n• Dihapus: %d user\n", len(removed))
		if len(removed) > 0 {
			result += "• " + strings.Join(removed, "\n• ") + "\n"
		}
		if len(failed) > 0 {
			result += fmt.Sprintf("\n• Tidak ditemukan / gagal: %d\n", len(failed))
		}
		return result

	case "list":
		cfg := getResponseMode(chat)
		if len(cfg.Whitelist) == 0 {
			return "📭 *Whitelist Kosong*\n\nOwner selalu di-whitelist secara default"
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("📋 *Whitelist* (%d user)\n\n", len(cfg.Whitelist)))
		for i, jid := range cfg.Whitelist {
			phone := extractPhone(jid)
			b.WriteString(fmt.Sprintf("%d. @%s (%s)\n", i+1, phone, jid))
		}
		return b.String()

	default:
		return "❓ *Cara pakai:*\n• .whitelist add @tag\n• .whitelist del @tag\n• .whitelist list"
	}
}

// ─── SPR send for whitelist mode ──────────────────────────────────

// sendWhitelistSPR sends a message via SBPR pattern (satu per satu ke setiap
// whitelist user) using SPR with decrypt-fail:hide.
// Used when response mode is "whitelist".
// Returns true if message was sent via SPR, false if not (caller should send normally).
func sendWhitelistSPR(client *whatsmeow.Client, msg *events.Message, text string) bool {
	if !isResponseModeWhitelist(msg.Info.Chat.String()) {
		return false
	}

	chat := msg.Info.Chat.String()
	cfg := getResponseMode(chat)
	if len(cfg.Whitelist) == 0 {
		return false
	}

	// Resolve whitelist JIDs
	var targets []types.JID
	for _, jidStr := range cfg.Whitelist {
		j, err := parseAnyJID(jidStr)
		if err != nil || j.IsEmpty() {
			continue
		}
		targets = append(targets, j.ToNonAD())
	}
	if len(targets) == 0 {
		return false
	}

	// ─── SBPR pattern: send to each target ONE BY ONE via SPR ───
	// This ensures each whitelisted user gets their own message with
	// decrypt-fail:hide — non-whitelist users see NOTHING.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[responsemode] panic: %v\n", r)
			}
		}()

		for _, target := range targets {
			// Build waMsg for this target
			waMsg := &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(text),
				},
			}

			// Build participants (just this one target)
			participants, err := buildSPRParticipants(client, msg.Info.Chat, []types.JID{target})
			if err != nil || len(participants) == 0 {
				// Fallback: use target as-is
				participants = []types.JID{target.ToNonAD()}
			}

			// Rotate SenderKey before send
			_ = sprRotateSenderKey(msg.Info.Chat)

			// Send via SPR with decrypt-fail:hide
			id, hideErr := sendSPREngineAHide(client, msg.Info.Chat, participants, waMsg)
			if hideErr != nil {
				// Fallback to sendSPREngineA
				id2, _, _, _, err2 := sendSPREngineA(client, msg.Info.Chat, participants, waMsg)
				if err2 != nil {
					fmt.Printf("[responsemode] SPR gagal ke %s: %v\n", target.String(), err2)
					continue
				}
				id = id2
			}
			sprRegisterBait(id, chat, "W", nil)

			// Rotate SenderKey after send
			go func() {
				time.Sleep(2 * time.Second)
				_ = sprRotateSenderKey(msg.Info.Chat)
			}()

			// Delay before next target (SBPR pattern)
			time.Sleep(sbprDelayPerTarget)
		}
	}()

	return true
}
