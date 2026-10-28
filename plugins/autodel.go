package main

// autodel.go — Auto-Delete Mode (PER-GRUP): semua pesan bot di grup ini
// dikirim dengan payload disappearing persis kode Baileys (afterReadDuration: 5).
//
// ─── PER-GROUP STATE (kayak .self spr) ──────────────────────────────
//
// Setiap grup punya state autodel sendiri:
//   .self autodel on     → aktifkan di grup INI saja
//   .self autodel off    → matikan di grup INI
//   .self autodel status → lihat status grup INI
//   .autodel ...         → alias dari .self autodel
//
// Durasi TETAP 5 detik (afterReadDuration: 5), tidak bisa disetting.
//
// Default: OFF di semua grup.
//
// ─── CARA KERJA ─────────────────────────────────────────────────────
//
// Kalau autodel ON di grup X, setiap pesan yang dikirim bot ke grup X
// otomatis memakai payload persis dari kode Baileys:
//
//   extendedTextMessage: {
//     text, previewType: 0,
//     contextInfo: {
//       expiration: 0,
//       ephemeralSettingTimestamp: Date.now(),  // milidetik
//       disappearingMode: { initiator: 0, trigger: 1 },
//       afterReadDuration: 5
//     },
//     inviteLinkGroupTypeV2: 0
//   }
//
// ─── PERSISTENCE ────────────────────────────────────────────────────
//
// 2026-10-27: migrated from data/autodel_groups.json to PG group_settings.
// The in-memory map is kept for fast reads (per-message hook), but
// load pulls from PG and save upserts each entry to PG (write-through).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// autodelGroupState holds per-group autodel state
type autodelGroupState struct {
	Active   bool  `json:"active"`
	Duration int64 `json:"duration"` // detik; 0 = pakai default
}

var (
	autodelMu     sync.RWMutex
	autodelGroups = map[string]*autodelGroupState{}
	autodelLoaded bool
)

// autodelDefaultDuration — durasi tetap 5 detik (tidak bisa disetting)
const autodelDefaultDuration = int64(5)

// ─── PERSISTENCE ────────────────────────────────────────────────────
//
// 2026-10-27: migrated from data/autodel_groups.json to PG group_settings.
// The in-memory map is kept for fast reads (per-message hook), but
// load pulls from PG and save upserts each entry to PG (write-through).

const autodelPluginName = "autodel"

// loadAutodelState loads per-group state from PG into the in-memory map.
// Idempotent (only runs once).
func loadAutodelState() {
	autodelMu.Lock()
	defer autodelMu.Unlock()
	if autodelLoaded {
		return
	}
	autodelLoaded = true
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pgQuery(ctx, db,
		`SELECT group_jid, enabled, COALESCE(config::text,'{}') FROM group_settings WHERE plugin = ?`,
		autodelPluginName)
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
		// The state is stored under "state" key in config JSONB.
		var wrapper struct {
			State autodelGroupState `json:"state"`
		}
		if err := json.Unmarshal([]byte(configJSON), &wrapper); err == nil {
			st := wrapper.State
			st.Active = enabled // authoritative source of truth
			autodelGroups[jid] = &st
		}
	}
}

// saveAutodelState writes all entries to PG (write-through, async per group).
// Caller must NOT hold autodelMu (this function takes its own lock).
func saveAutodelState() {
	autodelMu.RLock()
	groups := make(map[string]*autodelGroupState, len(autodelGroups))
	for k, v := range autodelGroups {
		groups[k] = v
	}
	autodelMu.RUnlock()
	// Async write-through to PG (one goroutine per group is fine; small N)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for jid, st := range groups {
			wrapper := map[string]any{
				"state":    st,
				"duration": st.Duration,
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
				jid, autodelPluginName, st.Active, jsonStr)
		}
	}()
}

// ─── Per-group accessors ────────────────────────────────────────────

// isAutodelActive checks if autodel is ON for a group (default OFF)
func isAutodelActive(groupJID string) bool {
	loadAutodelState()
	autodelMu.RLock()
	defer autodelMu.RUnlock()
	if g, ok := autodelGroups[groupJID]; ok {
		return g.Active
	}
	return false
}

// autodelDuration returns the disappearing duration (seconds) for a group
func autodelDuration(groupJID string) int64 {
	loadAutodelState()
	autodelMu.RLock()
	defer autodelMu.RUnlock()
	if g, ok := autodelGroups[groupJID]; ok && g.Duration > 0 {
		return g.Duration
	}
	return autodelDefaultDuration
}

// ─── Payload ────────────────────────────────────────────────────────
// Payload PERSIS kayak kode Baileys (relayMessage):
//
//   extendedTextMessage: {
//     text, previewType: 0,
//     contextInfo: {
//       expiration: 0,
//       ephemeralSettingTimestamp: Date.now(),  // MILIDETIK
//       disappearingMode: { initiator: 0, trigger: 1 },
//       afterReadDuration: 5
//     },
//     inviteLinkGroupTypeV2: 0
//   }
//
// Diterapkan ke semua pesan bot di grup yang autodel-nya ON.

// fillAutodelPayload menerapkan payload disappearing persis Baileys ke pesan,
// TANPA cek mode grup. Dipakai untuk one-shot per-pesan (.afterread).
func fillAutodelPayload(waMsg *waE2E.Message) {
	if waMsg == nil {
		return
	}
	// Nilai persis dari kode: initiator 0 (CHANGED_IN_CHAT), trigger 1 (CHAT_SETTING)
	initiator := waE2E.DisappearingMode_CHANGED_IN_CHAT
	trigger := waE2E.DisappearingMode_CHAT_SETTING
	previewNone := waE2E.ExtendedTextMessage_NONE
	linkV2Default := waE2E.ExtendedTextMessage_DEFAULT

	fill := func(ci *waE2E.ContextInfo) *waE2E.ContextInfo {
		if ci == nil {
			ci = &waE2E.ContextInfo{}
		}
		ci.Expiration = proto.Uint32(0)
		ci.EphemeralSettingTimestamp = proto.Int64(time.Now().UnixMilli())
		ci.DisappearingMode = &waE2E.DisappearingMode{
			Initiator: &initiator,
			Trigger:   &trigger,
		}
		ci.AfterReadDuration = proto.Uint32(5)
		return ci
	}

	if m := waMsg.GetExtendedTextMessage(); m != nil {
		m.PreviewType = &previewNone
		m.ContextInfo = fill(m.GetContextInfo())
		m.InviteLinkGroupTypeV2 = &linkV2Default
	}
	if m := waMsg.GetImageMessage(); m != nil {
		m.ContextInfo = fill(m.GetContextInfo())
	}
	if m := waMsg.GetVideoMessage(); m != nil {
		m.ContextInfo = fill(m.GetContextInfo())
	}
	if m := waMsg.GetAudioMessage(); m != nil {
		m.ContextInfo = fill(m.GetContextInfo())
	}
	if m := waMsg.GetDocumentMessage(); m != nil {
		m.ContextInfo = fill(m.GetContextInfo())
	}
	if m := waMsg.GetStickerMessage(); m != nil {
		m.ContextInfo = fill(m.GetContextInfo())
	}
}

// applyAutodelPayload menambahkan payload disappearing persis Baileys ke pesan.
// Dipanggil dari jalur pengiriman bila autodel ON di grup.
func applyAutodelPayload(waMsg *waE2E.Message, chatJID string) {
	if waMsg == nil || !isAutodelActive(chatJID) {
		return
	}
	fillAutodelPayload(waMsg)
}

// ─── Command ────────────────────────────────────────────────────────

// handleAutodelCommand — .self autodel on|off|status (per-group)
// Alias: .autodel on|off|status
// Durasi TETAP 5 detik — tidak bisa disetting.
func handleAutodelCommand(msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	loadAutodelState()

	groupJID := msg.Info.Chat.String()
	parts := strings.Fields(args)
	sub := ""
	if len(parts) > 0 {
		sub = strings.ToLower(parts[0])
	}

	switch sub {
	case "on":
		autodelMu.Lock()
		if _, ok := autodelGroups[groupJID]; !ok {
			autodelGroups[groupJID] = &autodelGroupState{}
		}
		autodelGroups[groupJID].Active = true
		autodelGroups[groupJID].Duration = autodelDefaultDuration
		autodelMu.Unlock()
		saveAutodelState()
		return "🟢 *Autodel ON*\n\n• Durasi: 5 detik (pesan bot hilang otomatis)"
	case "off":
		autodelMu.Lock()
		if _, ok := autodelGroups[groupJID]; !ok {
			autodelGroups[groupJID] = &autodelGroupState{}
		}
		autodelGroups[groupJID].Active = false
		autodelMu.Unlock()
		saveAutodelState()
		return "🔴 *Autodel OFF*"
	case "status", "":
		active := isAutodelActive(groupJID)
		state := "OFF"
		if active {
			state = "ON"
		}
		return fmt.Sprintf("🕐 *Autodel*\n\n• Status: %s\n• Durasi: 5 detik", state)
	}
	return "❓ *Cara pakai:* .autodel on, .autodel off, atau .autodel status"
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "autodel",
		PreGate:     true,
		Description: "autodel",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			// alias .self autodel
			return handleAutodelCommand(msg, args)
		},
	})
}
