package main

// antifdmsg.go — Anti FakeMsg/DMsg detection (port dari Baileys _antifdmsg.js)
//
// Detects edit messages (protocolMessage type=14 / secretEncryptedMessage type=2)
// where the editor is NOT the original sender → kicks the faker from group.
//
// Logic:
//   1. Skip if antifdmsg not enabled for this chat
//   2. Get sender LID (decode PN→LID if needed)
//   3. Look up message in CRM cache by (id, chat, sender)
//   4. Check if message is protocolMessage type=14 (MESSAGE_EDIT) or
//      secretEncryptedMessage type=2 (MESSAGE_EDIT)
//   5. Get target message ID (Key.ID / targetMessageKey.id)
//   6. Look up target message in cache by (targetID, chat, sender != faker)
//   7. If target found and target.sender != current sender → FAKE DETECTED
//   8. Send warning + kick faker from group
//
// Enable: .antifdmsg on   (owner/admin only)
// Disable: .antifdmsg off  (owner/admin only)
// Status: .antifdmsg status
//
// Storage: data/antifdmsg.json — map[chatJID]bool

import (
	"context"
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
)

// ─── State ────────────────────────────────────────────────────────

var (
	antifdmsgMu     sync.RWMutex
	antifdmsgConfig = map[string]bool{} // chat JID → enabled
	antifdmsgLoaded bool
)

func antifdmsgPath() string {
	return filepath.Join("data", "antifdmsg.json")
}

func loadAntifdmsg() {
	// Fast path: RLock dulu agar tidak kontensi di hot path per-pesan
	antifdmsgMu.RLock()
	if antifdmsgLoaded {
		antifdmsgMu.RUnlock()
		return
	}
	antifdmsgMu.RUnlock()

	antifdmsgMu.Lock()
	defer antifdmsgMu.Unlock()
	if antifdmsgLoaded {
		return
	}
	antifdmsgLoaded = true
	data, err := os.ReadFile(antifdmsgPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &antifdmsgConfig)
}

func saveAntifdmsg() {
	antifdmsgMu.RLock()
	defer antifdmsgMu.RUnlock()
	data, _ := json.MarshalIndent(antifdmsgConfig, "", "  ")
	_ = os.MkdirAll("data", 0o755)
	_ = os.WriteFile(antifdmsgPath(), data, 0o644)
}

func antifdmsgEnabled(chat string) bool {
	antifdmsgMu.RLock()
	defer antifdmsgMu.RUnlock()
	return antifdmsgConfig[chat]
}

func antifdmsgSet(chat string, enabled bool) {
	antifdmsgMu.Lock()
	defer antifdmsgMu.Unlock()
	if enabled {
		antifdmsgConfig[chat] = true
	} else {
		delete(antifdmsgConfig, chat)
	}
	go saveAntifdmsg()
}

// ─── Command handler ─────────────────────────────────────────────

func handleAntifdmsgCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return "❌ Pesan tidak valid"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner/admin grup"
	}

	loadAntifdmsg()
	sub := strings.ToLower(strings.TrimSpace(args))
	chat := msg.Info.Chat.String()

	switch sub {
	case "on", "enable", "aktif":
		antifdmsgSet(chat, true)
		return fmt.Sprintf("✅ *Anti-FakeMsg/DMsg AKTIF* di grup ini.\n\nDeteksi: edit pesan orang lain → auto-kick.\n⚠️ Bot harus jadi admin grup!")

	case "off", "disable", "mati":
		antifdmsgSet(chat, false)
		return "🔴 Anti-FakeMsg/DMsg DIMATIKAN di grup ini."

	case "status", "stat":
		enabled := antifdmsgEnabled(chat)
		status := "🔴 OFF"
		if enabled {
			status = "🟢 ON"
		}
		return fmt.Sprintf("⚙️ *Anti-FakeMsg/DMsg: %s*\n\n• Grup: %s\n• Bot admin: %v", status, chat, isBotGroupAdmin(client, msg.Info.Chat))

	default:
		return `🛡️ *Anti-FakeMsg/DMsg*

Deteksi edit pesan orang lain (protocolMessage type=14 / secretEncryptedMessage type=2) → auto-kick faker.

*Commands:*
• .antifdmsg on — aktifkan
• .antifdmsg off — matikan
• .antifdmsg status — cek status

⚠️ Bot harus jadi admin grup untuk bisa kick!`
	}
}

// ─── Detection logic ─────────────────────────────────────────────

// checkAntifdmsg runs on every incoming message to detect fake edits.
// Called from handleMessage before command processing.
//
// Returns true if message was handled (faker kicked).
func checkAntifdmsg(client *whatsmeow.Client, msg *events.Message) bool {
	if client == nil || msg == nil || !msg.Info.IsGroup {
		return false
	}

	loadAntifdmsg()
	if !antifdmsgEnabled(msg.Info.Chat.String()) {
		return false
	}

	// Skip bot's own messages
	if msg.Info.IsFromMe {
		return false
	}

	// Get the raw message
	raw := msg.RawMessage
	if raw == nil {
		raw = msg.Message
	}
	if raw == nil {
		return false
	}

	// Check for protocolMessage (type=14 = MESSAGE_EDIT)
	var targetID string
	var source string

	if proto := raw.GetProtocolMessage(); proto != nil {
		t := proto.GetType()
		if t != waE2E.ProtocolMessage_MESSAGE_EDIT {
			return false
		}
		source = "protocolMessage"
		if proto.Key != nil {
			targetID = proto.Key.GetID()
		}
	}

	// Check for secretEncryptedMessage (type=2 = MESSAGE_EDIT)
	if targetID == "" {
		if secret := raw.GetSecretEncryptedMessage(); secret != nil {
			// secretEncType 2 = MESSAGE_EDIT
			t := secret.GetSecretEncType()
			if t != waE2E.SecretEncryptedMessage_MESSAGE_EDIT {
				return false
			}
			source = "secretEncryptedMessage"
			if secret.TargetMessageKey != nil {
				targetID = secret.TargetMessageKey.GetID()
			}
		}
	}

	if targetID == "" {
		return false
	}

	// Look up the target message in CRM cache
	// Find by ID + chat, where sender != current sender (proves it's someone else's message)
	target := findCRMTargetMessage(msg.Info.Chat.String(), targetID, msg.Info.Sender.String())
	if target == "" {
		// Target not in cache — can't confirm fake
		return false
	}

	// Target found and target.sender != current sender → FAKE DETECTED
	currentSender := msg.Info.Sender.String()
	if target == currentSender {
		// Same sender editing own message — not fake
		return false
	}

	// FAKE DETECTED — send warning + kick
	go antifdmsgPunish(client, msg, currentSender, target, source, targetID)
	return true
}

// findCRMTargetMessage searches CRM cache for a message with given ID + chat,
// where sender != excludeSender. Returns the target sender JID if found.
func findCRMTargetMessage(chat, msgID, excludeSender string) string {
	crmMu.RLock()
	defer crmMu.RUnlock()

	for _, e := range crmCache {
		if e.Chat != chat {
			continue
		}
		if e.ID != msgID {
			continue
		}
		// Check if this message's sender != excludeSender
		if e.Sender != "" && e.Sender != excludeSender {
			return e.Sender
		}
	}
	return ""
}

// antifdmsgPunish sends warning message and kicks the faker.
func antifdmsgPunish(client *whatsmeow.Client, msg *events.Message, faker, target, source, targetID string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[antifdmsg] panic: %v\n", r)
		}
	}()

	fakerPhone := extractPhone(faker)
	targetPhone := extractPhone(target)

	warning := fmt.Sprintf("⚠️ *FakeMsg/DMsg Terdeteksi!*\n\n"+
		"• Sender: @%s\n"+
		"• Target: @%s\n"+
		"• Type: %s\n"+
		"• TargetID: %s\n\n"+
		"Sender telah dikeluarkan dari grup.",
		fakerPhone, targetPhone, source, targetID)

	// Send warning with mentions
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fakerJID, _ := parseAnyJID(faker)
	targetJID, _ := parseAnyJID(target)
	mentions := []string{}
	if !fakerJID.IsEmpty() {
		mentions = append(mentions, fakerJID.String())
	}
	if !targetJID.IsEmpty() {
		mentions = append(mentions, targetJID.String())
	}
	waMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: &warning,
			ContextInfo: &waE2E.ContextInfo{
				MentionedJID: mentions,
			},
		},
	}
	_, _ = client.SendMessage(ctx, msg.Info.Chat, waMsg)

	// Kick faker from group
	kickCtx, kickCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer kickCancel()
	fakerForms := dualForms(client, fakerJID)
	if len(fakerForms) == 0 {
		fakerForms = []types.JID{fakerJID}
	}
	for _, f := range fakerForms {
		_, err := client.UpdateGroupParticipants(kickCtx, msg.Info.Chat, []types.JID{f}, whatsmeow.ParticipantChangeRemove)
		if err == nil {
			fmt.Printf("[antifdmsg] ✅ Kicked %s for fake %s (target %s)\n", fakerPhone, source, targetPhone)
			return
		}
	}
	fmt.Printf("[antifdmsg] ⚠️ Failed to kick %s (bot not admin?)\n", fakerPhone)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "antifdmsg",
		Aliases:     []string{"antifakemsg", "antidmsg"},
		Description: "antifdmsg",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAntifdmsgCommand(client, msg, args)
		},
	})
}
