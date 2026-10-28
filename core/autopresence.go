package main

/**
 * 🤖 autopresence.go — Auto-read, auto-typing, presence settings
 * ════════════════════════════════════════════════════════════
 * Commands (prefix .):
 *   .autoread on|off|status → toggle auto-mark-read
 *   .autotyping on|off|status → toggle typing indicator
 *   .autorecord on|off|status → toggle recording indicator
 *   .setpresence <available|unavailable> → set bot presence
 *
 * Auto-read: marks incoming chat messages as read.
 * Auto-typing: sends "typing…" presence when bot processes a command.
 * Auto-record: sends "recording…" presence when bot processes audio/media.
 */

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

type AutoPresenceState struct {
	AutoRead   bool `json:"auto_read"`
	AutoTyping bool `json:"auto_typing"`
	AutoRecord bool `json:"auto_record"`
}

var (
	apState     = AutoPresenceState{AutoRead: true, AutoTyping: false, AutoRecord: false}
	apStateMu   sync.RWMutex
	apStateFile = dataDir + "/autopresence.json"
)

func loadAutoPresence() {
	apStateMu.Lock()
	defer apStateMu.Unlock()
	_ = loadJSON(apStateFile, &apState)
}

func saveAutoPresence() {
	apStateMu.RLock()
	defer apStateMu.RUnlock()
	_ = saveJSON(apStateFile, apState)
}

// maybeAutoRead marks a chat as read if AutoRead is on.
func maybeAutoRead(client *whatsmeow.Client, msg *events.Message) {
	apStateMu.RLock()
	on := apState.AutoRead
	apStateMu.RUnlock()
	if !on || client == nil || msg == nil {
		return
	}
	// mark message as read (with delay to avoid rate limit)
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = markChatRead(client, msg.Info.Chat, msg.Info.Sender, msg.Info.ID)
	}()
}

// markChatRead sends a read receipt.
func markChatRead(client *whatsmeow.Client, chat, sender types.JID, msgID string) error {
	if client == nil {
		return fmt.Errorf("no client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return client.MarkRead(ctx, []types.MessageID{msgID}, time.Now(), chat, sender)
}

// maybeAutoTyping sends "composing…" presence if AutoTyping is on.
func maybeAutoTyping(client *whatsmeow.Client, chat types.JID) {
	apStateMu.RLock()
	on := apState.AutoTyping
	apStateMu.RUnlock()
	if !on || client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.SendChatPresence(ctx, chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	}()
}

// maybeAutoRecord sends "recording audio…" presence if AutoRecord is on.
// Note: WhatsApp uses ChatPresenceComposing with ChatPresenceMediaAudio to indicate recording.
func maybeAutoRecord(client *whatsmeow.Client, chat types.JID) {
	apStateMu.RLock()
	on := apState.AutoRecord
	apStateMu.RUnlock()
	if !on || client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.SendChatPresence(ctx, chat, types.ChatPresenceComposing, types.ChatPresenceMediaAudio)
	}()
}

// handleAutoPresence processes .autoread / .autotyping / .autorecord on|off|status
func handleAutoPresence(kind, args string) string {
	args = strings.ToLower(strings.TrimSpace(args))

	apStateMu.Lock()
	defer apStateMu.Unlock()

	var p *bool
	var label string
	switch kind {
	case "autoread":
		p = &apState.AutoRead
		label = "Auto-Read"
	case "autotyping":
		p = &apState.AutoTyping
		label = "Auto-Typing"
	case "autorecord":
		p = &apState.AutoRecord
		label = "Auto-Recording"
	default:
		return "❌ Unknown presence type"
	}

	switch args {
	case "on", "1", "true":
		*p = true
		go saveAutoPresence()
		return fmt.Sprintf("✅ %s diaktifkan.", label)
	case "off", "0", "false":
		*p = false
		go saveAutoPresence()
		return fmt.Sprintf("✅ %s dimatikan.", label)
	case "status", "":
		state := "ON ✅"
		if !*p {
			state = "OFF"
		}
		return fmt.Sprintf("📊 %s: %s", label, state)
	}
	return fmt.Sprintf("❓ Cara pakai: .%s on|off|status", kind)
}

// handleSetPresence sets bot presence globally.
func handleSetPresence(client *whatsmeow.Client, args string) string {
	if !isSelfMode() {
		// anyone can use this, but in self mode only owner can
	}
	presence := strings.ToLower(strings.TrimSpace(args))
	switch presence {
	case "available", "online":
		_ = setBotPresence(client, types.PresenceAvailable)
		return "✅ Presence set to *Available*"
	case "unavailable", "offline":
		_ = setBotPresence(client, types.PresenceUnavailable)
		return "✅ Presence set to *Unavailable*"
	}
	return "❓ Cara pakai: *.setpresence available|unavailable*"
}

func setBotPresence(client *whatsmeow.Client, p types.Presence) error {
	if client == nil {
		return fmt.Errorf("no client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return client.SendPresence(ctx, p)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "autoread",
		Description: "autoread",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAutoPresence("autoread", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "autotyping",
		Description: "autotyping",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAutoPresence("autotyping", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "autorecord",
		Description: "autorecord",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAutoPresence("autorecord", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "setpresence",
		Description: "setpresence",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSetPresence(client, args)
			// ─── SW HD (status HD via ffmpeg) ──────────────────────
		},
	})
}
