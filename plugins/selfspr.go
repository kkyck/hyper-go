package main

// selfspr.go — Self SPR Mode (PER-GRUP): route ALL bot replies via SPR Hide
//
// ─── PER-GROUP STATE (like .self on / .self off) ──────────────────────
//
// Setiap grup punya state selfSpr sendiri:
//   .self spr on   → aktifkan SPR mode di grup INI saja
//   .self spr off  → matikan SPR mode di grup INI
//   .self spr status → lihat status grup INI
//
// Whitelist juga per-grup:
//   .wl @tag       → tambah user ke whitelist grup INI
//   .wl clear      → kosongkan whitelist grup INI
//   .wl list       → lihat whitelist grup INI
//
// ─── FLOW ────────────────────────────────────────────────────────────
//
// Kalau selfSpr ON di grup X:
//   1. Setiap reply bot → SPR Hide ke owner (command sender) + whitelist grup X
//   2. Non-targets (non-owner, non-whitelist) lihat NOTHING
//   3. Grup Y tetap normal (selfSpr OFF di grup Y)
//
// ─── PERSISTENCE ─────────────────────────────────────────────────────
//
// 2026-10-27: migrated from data/selfspr_groups.json to PG group_settings.
// In-memory map retained for fast reads (called per-message). Writes go
// to PG + cache (write-through).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── Data structures ────────────────────────────────────────────────

// selfSprGroupState holds per-group SPR state
type selfSprGroupState struct {
	Active    bool     `json:"active"`
	Whitelist []string `json:"whitelist"`
}

const selfSprPluginName = "selfspr"

var (
	selfSprMu     sync.RWMutex
	selfSprGroups = map[string]*selfSprGroupState{}
	selfSprLoaded bool
)

// ─── State management ───────────────────────────────────────────────

// loadSelfSprState loads per-group state from PG (once)
func loadSelfSprState() {
	selfSprMu.Lock()
	defer selfSprMu.Unlock()
	if selfSprLoaded {
		return
	}
	selfSprLoaded = true
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pgQuery(ctx, db,
		`SELECT group_jid, enabled, COALESCE(config::text,'{}') FROM group_settings WHERE plugin = ?`,
		selfSprPluginName)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var jid, configJSON string
		var enabled bool
		if err := rows.Scan(&jid, &enabled, &configJSON); err != nil {
			continue
		}
		var wrapper struct {
			State selfSprGroupState `json:"state"`
		}
		if err := json.Unmarshal([]byte(configJSON), &wrapper); err == nil {
			st := wrapper.State
			st.Active = enabled
			selfSprGroups[jid] = &st
		}
	}
}

// saveSelfSprState persists all per-group state to PG (write-through, async)
func saveSelfSprState() {
	selfSprMu.RLock()
	groups := make(map[string]*selfSprGroupState, len(selfSprGroups))
	for k, v := range selfSprGroups {
		groups[k] = v
	}
	selfSprMu.RUnlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for jid, st := range groups {
			wrapper := map[string]any{
				"state":     st,
				"whitelist": st.Whitelist,
			}
			jsonBytes, err := json.Marshal(wrapper)
			if err != nil {
				continue
			}
			jsonStr := string(jsonBytes)
			if !json.Valid([]byte(jsonStr)) {
				jsonStr = "{}"
			}
			db, err := pgGet()
			if err != nil {
				continue
			}
			_, _ = pgExec(ctx, db,
				`INSERT INTO group_settings (group_jid, plugin, enabled, config)
                                 VALUES (?, ?, ?, ?::jsonb)
                                 ON CONFLICT (group_jid, plugin) DO UPDATE
                                   SET enabled = EXCLUDED.enabled, config = EXCLUDED.config, updated_at = NOW()`,
				jid, selfSprPluginName, st.Active, jsonStr)
		}
	}()
}

// ─── Per-group accessors ────────────────────────────────────────────

// isSelfSprActive checks if SPR mode is ON for a specific group
func isSelfSprActive(groupJID string) bool {
	loadSelfSprState()
	selfSprMu.RLock()
	defer selfSprMu.RUnlock()
	if g, ok := selfSprGroups[groupJID]; ok {
		return g.Active
	}
	return false
}

// selfSprWhitelistAdd adds a user to a group's whitelist
func selfSprWhitelistAdd(groupJID, userID string) bool {
	loadSelfSprState()
	selfSprMu.Lock()
	defer selfSprMu.Unlock()
	if _, ok := selfSprGroups[groupJID]; !ok {
		selfSprGroups[groupJID] = &selfSprGroupState{}
	}
	for _, u := range selfSprGroups[groupJID].Whitelist {
		if u == userID {
			return false
		}
	}
	selfSprGroups[groupJID].Whitelist = append(selfSprGroups[groupJID].Whitelist, userID)
	return true
}

// selfSprWhitelistClear clears a group's whitelist, returns count removed
func selfSprWhitelistClear(groupJID string) int {
	loadSelfSprState()
	selfSprMu.Lock()
	defer selfSprMu.Unlock()
	if g, ok := selfSprGroups[groupJID]; ok {
		n := len(g.Whitelist)
		g.Whitelist = nil
		return n
	}
	return 0
}

// selfSprWhitelistList returns a group's whitelist (copy)
func selfSprWhitelistList(groupJID string) []string {
	loadSelfSprState()
	selfSprMu.RLock()
	defer selfSprMu.RUnlock()
	if g, ok := selfSprGroups[groupJID]; ok {
		out := make([]string, len(g.Whitelist))
		copy(out, g.Whitelist)
		return out
	}
	return nil
}

// ─── SPR reply router ──────────────────────────────────────────────

// sendReplyViaSPR2 sends a reply via SPR Hide to owner + whitelist (per-group).
// Returns true if SPR was used (caller should skip normal send).
func sendReplyViaSPR2(client *whatsmeow.Client, msg *events.Message, text string) bool {
	if msg == nil {
		return false
	}
	groupJID := msg.Info.Chat.String()
	// ⚡ OPTIMIZED: removed debug logging (fmt.Printf) — saves ~10-100µs I/O per reply
	if !isSelfSprActive(groupJID) || !msg.Info.IsGroup {
		return false
	}

	// Build recipients: owner (command sender) + whitelist
	var recipients []types.JID
	owner := msg.Info.Sender.ToNonAD()
	if !owner.IsEmpty() {
		recipients = append(recipients, owner)
	}

	// Resolve owner to phone-form for dedup (catches PN/LID form mismatch)
	ownerPhone := phoneOf(client, owner)

	// Add whitelist members (per-group)
	wl := selfSprWhitelistList(groupJID)
	for _, uid := range wl {
		j, err := parseAnyJID(uid)
		if err != nil || j.IsEmpty() {
			continue
		}
		// Dedup against owner — check BOTH User ID AND phone-form.
		// Without phone-form check, owner in LID form (e.g., 65138346446989@lid)
		// + whitelist in PN form (e.g., 6281234567890@s.whatsapp.net) would
		// NOT match → owner gets 2 SPR → double message.
		jPhone := phoneOf(client, j)
		isDup := false
		if !owner.IsEmpty() && j.User == owner.User {
			isDup = true
		}
		if !isDup && ownerPhone != "" && jPhone != "" && jPhone == ownerPhone {
			isDup = true
		}
		if isDup {
			continue
		}
		recipients = append(recipients, j)
	}

	fmt.Printf("[SELFSpr] recipients=%d (owner=%s, wl=%d)\n", len(recipients), owner.String(), len(wl))

	if len(recipients) == 0 {
		return false
	}

	// Build message ONCE (reuse for all recipients)
	// Include ContextInfo that quote-replies to the command message
	waMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:    proto.String(msg.Info.ID),
				Participant: proto.String(msg.Info.Sender.String()),
				QuotedMessage: &waE2E.Message{
					Conversation: proto.String(extractText(msg)),
				},
			},
		},
	}

	// Send SPR Hide to each recipient sequentially
	sentCount := 0
	for _, t := range recipients {
		// buildSPRParticipants — REQUIRED for Hide variant to work
		participants, err := buildSPRParticipants(client, msg.Info.Chat, []types.JID{t})
		if err != nil {
			fmt.Printf("[SELFSpr] buildSPRParticipants gagal ke %s: %v\n", t.String(), err)
			continue
		}

		// Rotate sender key BEFORE send (critical for Hide variant).
		// Dibungkus sprBaseMu agar tidak interleave dengan SPR lain yang
		// sedang dalam rangkaian rotation→send.
		sprBaseMu.Lock()
		if rerr := sprRotateSenderKey(msg.Info.Chat); rerr != nil {
			fmt.Printf("[SELFSpr] ⚠️ pre-send rotation gagal: %v\n", rerr)
		}
		sprBaseMu.Unlock()

		// Send SPR Hide
		id, hideErr := sendSPREngineAHide(client, msg.Info.Chat, participants, waMsg)
		if hideErr != nil {
			fmt.Printf("[SELFSpr] Hide gagal ke %s: %v, coba plain...\n", t.String(), hideErr)
			id2, _, _, _, err2 := sendSPREngineA(client, msg.Info.Chat, participants, waMsg)
			if err2 != nil {
				fmt.Printf("[SELFSpr] SPR gagal ke %s: hide=%v plain=%v\n", t.String(), hideErr, err2)
				continue
			}
			id = id2
		}
		sprRegisterBait(id, msg.Info.Chat.String(), "S", nil)
		sentCount++
		fmt.Printf("[SELFSpr] SPR sent to %s (id=%s)\n", t.String(), id)
		time.Sleep(50 * time.Millisecond) // avoid SQLite lock
	}

	fmt.Printf("[SELFSpr] done: %d/%d recipients got SPR\n", sentCount, len(recipients))
	return sentCount > 0
}

// ─── Command handlers ──────────────────────────────────────────────

// handleSelfSprCommand — .self spr on|off|status (per-group)
func handleSelfSprCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ .self spr hanya di grup"
	}
	loadSelfSprState()

	groupJID := msg.Info.Chat.String()
	parts := strings.Fields(strings.ToLower(args))
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}

	switch sub {
	case "on":
		selfSprMu.Lock()
		if _, ok := selfSprGroups[groupJID]; !ok {
			selfSprGroups[groupJID] = &selfSprGroupState{}
		}
		selfSprGroups[groupJID].Active = true
		selfSprMu.Unlock()
		saveSelfSprState()
		wl := len(selfSprWhitelistList(groupJID))
		return fmt.Sprintf("✅ *Self SPR ON*\n\n• Whitelist: %d user", wl)
	case "off":
		selfSprMu.Lock()
		if _, ok := selfSprGroups[groupJID]; !ok {
			selfSprGroups[groupJID] = &selfSprGroupState{}
		}
		selfSprGroups[groupJID].Active = false
		selfSprMu.Unlock()
		saveSelfSprState()
		return "🔴 *Self SPR OFF*"
	case "status", "":
		active := isSelfSprActive(groupJID)
		wl := len(selfSprWhitelistList(groupJID))
		state := "OFF"
		if active {
			state = "ON"
		}
		return fmt.Sprintf("🔒 *Self SPR Status*\n\n• Status: %s\n• Whitelist: %d user", state, wl)
	}
	return "❓ *Cara pakai:* .stealth on|off|status"
}

// handleWLCommand — .wl @tag / .wl clear / .wl list (per-group)
func handleWLCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	loadSelfSprState()

	groupJID := msg.Info.Chat.String()
	parts := strings.Fields(strings.ToLower(args))
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}

	switch sub {
	case "clear":
		n := selfSprWhitelistClear(groupJID)
		saveSelfSprState()
		return fmt.Sprintf("✅ Whitelist dibersihkan — %d user dihapus", n)
	case "list", "ls":
		wl := selfSprWhitelistList(groupJID)
		if len(wl) == 0 {
			return "📭 Whitelist kosong"
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("📋 *Whitelist* (%d user)\n\n", len(wl)))
		for i, uid := range wl {
			b.WriteString(fmt.Sprintf("%d. @%s\n", i+1, uid))
		}
		return b.String()
	}

	// Default: add @tag (parse mentions from message)
	mentions := extractMentionedJIDsDual(msg)
	if len(mentions) == 0 {
		return "❓ *Cara pakai:* .wl @tag\n• .wl clear — kosongkan whitelist\n• .wl list — lihat daftar"
	}

	added := 0
	for _, m := range mentions {
		j, err := parseAnyJID(m)
		if err != nil || j.IsEmpty() {
			continue
		}
		// Use phone-form user ID (preferred); fallback to User
		uid := phoneOf(client, j)
		if uid == "" {
			uid = j.User
		}
		if selfSprWhitelistAdd(groupJID, uid) {
			added++
		}
	}
	saveSelfSprState()

	if added == 0 {
		return "ℹ️ Sudah ada di whitelist"
	}
	return fmt.Sprintf("✅ %d user ditambahkan ke whitelist", added)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "wl",
		PreGate:     true,
		Description: "wl",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			// .wl @tag / .wl clear / .wl list — manage per-group SPR whitelist
			return handleWLCommand(client, msg, args)
		},
	})
}
