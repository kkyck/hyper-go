package main

// stealth_primary.go — .stealth primary — mode kirim ke perangkat utama saja.
//
// Cara pakai (owner only, grup only):
//   .stealth primary @all on          — aktif untuk semua member grup
//   .stealth primary 628xxx 628yyy on — aktif hanya untuk nomor yg disebut
//   .stealth primary off              — matikan
//   .stealth primary status           — lihat status
//
// Saat mode aktif, setiap pesan balasan bot di grup ini dikirim secara
// senyap HANYA ke perangkat utama (device 0 / HP) setiap target.
// Perangkat tertaut (WhatsApp Web, dsb) tidak menerima apa-apa.
//
// 2026-10-27: migrated from data/stealth_primary.json to PG group_settings.
// In-memory cache retained for fast reads (called per-message via
// humanmsg.go). Writes go to PG + cache (write-through).

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Plugin name in group_settings.
const stealthPrimaryPluginName = "stealth_primary"

type stealthPrimaryState struct {
	Active     bool     `json:"active"`
	AllMembers bool     `json:"all_members"`
	Targets    []string `json:"targets"`
}

var (
	stealthPrimaryMu     sync.RWMutex
	stealthPrimaryGroups = map[string]*stealthPrimaryState{}
	stealthPrimaryLoaded bool
)

// loadStealthPrimaryState loads all stealth_primary rows from PG into
// the in-memory cache. Called once at startup (lazy on first access).
// Idempotent — safe to call multiple times.
func loadStealthPrimaryState() {
	stealthPrimaryMu.Lock()
	defer stealthPrimaryMu.Unlock()
	if stealthPrimaryLoaded {
		return
	}
	stealthPrimaryLoaded = true
	// Pull all rows for this plugin from PG
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pgQuery(ctx, db,
		`SELECT group_jid, enabled, COALESCE(config::text,'{}') FROM group_settings WHERE plugin = ?`,
		stealthPrimaryPluginName)
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
		var cfg map[string]any
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			continue
		}
		st := &stealthPrimaryState{Active: enabled}
		if v, ok := cfg["all_members"].(bool); ok {
			st.AllMembers = v
		}
		if arr, ok := cfg["targets"].([]any); ok {
			for _, a := range arr {
				if s, ok := a.(string); ok {
					st.Targets = append(st.Targets, s)
				}
			}
		}
		stealthPrimaryGroups[jid] = st
	}
}

// stealthPrimaryPersist writes one group's state to PG (write-through).
// Called with stealthPrimaryMu held (caller's responsibility).
func stealthPrimaryPersist(groupJID string, st *stealthPrimaryState) {
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := map[string]any{
		"all_members": st.AllMembers,
		"targets":     st.Targets,
	}
	jsonBytes, _ := json.Marshal(cfg)
	jsonStr := string(jsonBytes)
	if !json.Valid([]byte(jsonStr)) {
		jsonStr = "{}"
	}
	_, _ = pgExec(ctx, db,
		`INSERT INTO group_settings (group_jid, plugin, enabled, config)
                 VALUES (?, ?, ?, ?::jsonb)
                 ON CONFLICT (group_jid, plugin) DO UPDATE
                   SET enabled = EXCLUDED.enabled, config = EXCLUDED.config, updated_at = NOW()`,
		groupJID, stealthPrimaryPluginName, st.Active, jsonStr)
}

// saveStealthPrimaryState — write-through stub (per-group persist happens
// inline on each mutation). Kept for backward-compat with old call sites.
func saveStealthPrimaryState() {
	// No-op: stealthPrimaryPersist() called inline by mutations.
}

func isStealthPrimaryActive(groupJID string) bool {
	loadStealthPrimaryState()
	stealthPrimaryMu.RLock()
	defer stealthPrimaryMu.RUnlock()
	st, ok := stealthPrimaryGroups[groupJID]
	return ok && st.Active
}

func stealthPrimaryTargets(groupJID string) (allMembers bool, targets []string) {
	loadStealthPrimaryState()
	stealthPrimaryMu.RLock()
	defer stealthPrimaryMu.RUnlock()
	st, ok := stealthPrimaryGroups[groupJID]
	if !ok {
		return false, nil
	}
	return st.AllMembers, st.Targets
}

// handleStealthPrimaryCommand — .stealth primary ...
func handleStealthPrimaryCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ .stealth primary hanya di grup"
	}
	loadStealthPrimaryState()

	groupJID := msg.Info.Chat.String()
	low := strings.ToLower(args)

	switch {
	case strings.Contains(low, "off"):
		stealthPrimaryMu.Lock()
		if _, ok := stealthPrimaryGroups[groupJID]; !ok {
			stealthPrimaryGroups[groupJID] = &stealthPrimaryState{}
		}
		stealthPrimaryGroups[groupJID].Active = false
		st := stealthPrimaryGroups[groupJID]
		stealthPrimaryPersist(groupJID, st)
		stealthPrimaryMu.Unlock()
		return "🔴 *Stealth primary OFF*\n\nPesan kembali normal ke semua perangkat."

	case strings.Contains(low, "status"):
		stealthPrimaryMu.RLock()
		st, ok := stealthPrimaryGroups[groupJID]
		stealthPrimaryMu.RUnlock()
		state := "OFF"
		scope := "-"
		if ok && st.Active {
			state = "ON"
			if st.AllMembers {
				scope = "semua member grup"
			} else {
				scope = fmt.Sprintf("%d nomor", len(st.Targets))
			}
		}
		return fmt.Sprintf("🥷 *Stealth Primary Status*\n\n• Status: %s\n• Cakupan: %s\n• Hanya perangkat utama (HP) yang menerima", state, scope)

	case strings.Contains(low, "on"):
		st := &stealthPrimaryState{Active: true}
		if strings.Contains(low, "@all") {
			st.AllMembers = true
		} else {
			// Kumpulkan nomor/JID dari args
			fields := strings.Fields(args)
			for _, f := range fields {
				f = strings.Trim(f, "@+,")
				if f == "" {
					continue
				}
				fl := strings.ToLower(f)
				if fl == "on" || fl == "primary" || fl == "all" {
					continue
				}
				// Hanya digit (nomor telepon)
				digits := ""
				for _, r := range f {
					if r >= '0' && r <= '9' {
						digits += string(r)
					}
				}
				if len(digits) >= 8 {
					st.Targets = append(st.Targets, digits)
				}
			}
			// Kalau tidak ada nomor disebut, pakai whitelist .stealth
			if len(st.Targets) == 0 {
				wl := selfSprWhitelistList(groupJID)
				if len(wl) == 0 {
					return "❓ Whitelist kosong. Tambah dulu via .wl @tag, atau pakai:\n.stealth primary @all on"
				}
				st.Targets = wl
			}
		}
		stealthPrimaryMu.Lock()
		stealthPrimaryGroups[groupJID] = st
		stealthPrimaryPersist(groupJID, st)
		stealthPrimaryMu.Unlock()

		scope := "semua member grup"
		if !st.AllMembers {
			// Cek apakah dari whitelist atau nomor langsung
			fields := strings.Fields(strings.ToLower(args))
			hasDigits := false
			for _, f := range fields {
				d := ""
				for _, r := range strings.Trim(f, "@+,") {
					if r >= '0' && r <= '9' {
						d += string(r)
					}
				}
				if len(d) >= 8 {
					hasDigits = true
					break
				}
			}
			if hasDigits {
				scope = fmt.Sprintf("%d nomor", len(st.Targets))
			} else {
				scope = fmt.Sprintf("whitelist (%d nomor)", len(st.Targets))
			}
		}
		return fmt.Sprintf("🥷 *Stealth primary ON*\n\n• Cakupan: %s\n• Hanya perangkat utama (HP) yang menerima pesan\n• Perangkat tertaut tidak menerima apa-apa", scope)
	}

	return "❓ *Cara pakai:*\n.stealth primary @all on\n.stealth primary 628xxx on\n.stealth primary off\n.stealth primary status"
}

// sendStealthPrimaryBatch — kirim teks ke target via batch relay,
// HANYA ke perangkat utama (device 0). Mengembalikan (terkirim, gagal).
func sendStealthPrimaryBatch(client *whatsmeow.Client, groupJID types.JID, targets []types.JID, text string) (int, int) {
	if len(targets) == 0 || client == nil {
		return 0, 0
	}
	baseID := "STH" + fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	var sent, failed int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	for _, target := range targets {
		wg.Add(1)
		go func(t types.JID) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Typing indicator biar kelihatan natural
			go func() {
				tctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				_ = client.SendChatPresence(tctx, groupJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
			}()
			// Delay manusiawi 300-900ms
			time.Sleep(time.Duration(300+rand.Intn(600)) * time.Millisecond)

			waMsg := &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(text),
				},
			}
			participants, err := buildSPRParticipants(client, groupJID, []types.JID{t})
			if err != nil {
				atomic.AddInt32(&failed, 1)
				return
			}
			// primaryOnly=true — hanya device 0
			if _, err := sendSPRDeviceFilteredWithID(client, groupJID, participants, waMsg, true, baseID); err != nil {
				atomic.AddInt32(&failed, 1)
			} else {
				atomic.AddInt32(&sent, 1)
			}
		}(target)
	}
	wg.Wait()
	return int(sent), int(failed)
}

// sendStealthPrimaryText — kirim teks polos via stealth primary ke chat grup.
// Dipakai oleh jalur kirim yang tidak punya konteks msg (humanSendText, dll).
// Mengembalikan true jika berhasil ditangani.
func sendStealthPrimaryText(client *whatsmeow.Client, chat types.JID, text string) bool {
	if client == nil || chat.Server != types.GroupServer {
		return false
	}
	groupJIDStr := chat.String()
	if !isStealthPrimaryActive(groupJIDStr) {
		return false
	}

	allMembers, targetPhones := stealthPrimaryTargets(groupJIDStr)

	var targets []types.JID
	if allMembers {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, chat)
		if err != nil || meta == nil {
			return false
		}
		for _, p := range meta.Participants {
			if !p.JID.IsEmpty() {
				targets = append(targets, p.JID.ToNonAD())
			}
		}
	} else {
		for _, ph := range targetPhones {
			// targetPhones bisa berupa nomor atau JID string
			if strings.Contains(ph, "@") {
				if j, err := types.ParseJID(ph); err == nil && !j.IsEmpty() {
					targets = append(targets, j.ToNonAD())
					continue
				}
			}
			j := types.NewJID(ph, types.DefaultUserServer)
			targets = append(targets, j)
		}
	}

	if len(targets) == 0 {
		return false
	}

	sent, _ := sendStealthPrimaryBatch(client, chat, targets, text)
	return sent > 0
}

// sendReplyViaStealthPrimary — hook: kirim balasan bot via stealth primary.
// Mengembalikan true jika berhasil ditangani (caller harus return silent).
func sendReplyViaStealthPrimary(client *whatsmeow.Client, msg *events.Message, text string) bool {
	if msg == nil || client == nil {
		return false
	}
	groupJIDStr := msg.Info.Chat.String()
	if !isStealthPrimaryActive(groupJIDStr) || !msg.Info.IsGroup {
		return false
	}

	allMembers, targetPhones := stealthPrimaryTargets(groupJIDStr)

	var targets []types.JID
	if allMembers {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
		if err != nil || meta == nil {
			return false
		}
		for _, p := range meta.Participants {
			if !p.JID.IsEmpty() {
				targets = append(targets, p.JID.ToNonAD())
			}
		}
	} else {
		for _, ph := range targetPhones {
			j := types.NewJID(ph, types.DefaultUserServer)
			targets = append(targets, j)
		}
	}

	if len(targets) == 0 {
		return false
	}

	sent, _ := sendStealthPrimaryBatch(client, msg.Info.Chat, targets, text)
	return sent > 0
}
