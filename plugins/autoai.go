package main

// autoai.go — .autoai on|off|status
//
// Mengontrol AI auto-response (trigger "ruby", "ayang", dan reply AI)
// per grup. Owner only.
//
// 2026-10-27: migrated from data/autoai.json (in-memory map[string]bool)
// to PostgreSQL group_settings table. Public API unchanged.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// Plugin name used as the primary key in group_settings table.
const autoAIPlugin = "autoai"

// autoAIDefault — true (AI auto-response is ON by default).
// Group not in PG = default ON.
const autoAIDefault = true

// isAutoAIEnabled — true jika AI auto-response aktif di chat ini.
// Default ON kecuali dimatikan via .autoai off.
// Reads from PG group_settings; falls back to default on error/missing.
func isAutoAIEnabled(chatJID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return pgGroupGetBool(ctx, autoAIPlugin, chatJID, autoAIDefault)
}

func handleAutoAICommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	chatJID := msg.Info.Chat.String()
	low := strings.ToLower(strings.TrimSpace(args))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch low {
	case "on":
		if err := pgGroupSetBool(ctx, autoAIPlugin, chatJID, true); err != nil {
			return fmt.Sprintf("❌ PG: %v", err)
		}
		return "✅ *Auto AI ON*\n\nBot akan auto-balas AI untuk trigger \"ruby\" / \"ayang\"."
	case "off":
		if err := pgGroupSetBool(ctx, autoAIPlugin, chatJID, false); err != nil {
			return fmt.Sprintf("❌ PG: %v", err)
		}
		return "🔴 *Auto AI OFF*\n\nBot tidak akan auto-balas AI di chat ini.\nCommand .ai manual tetap bisa dipakai."
	case "status", "":
		status := "ON"
		if !isAutoAIEnabled(chatJID) {
			status = "OFF"
		}
		return fmt.Sprintf("🤖 *Auto AI Status*\n\n• Status: %s", status)
	}
	return "❓ *Cara pakai:* .autoai on|off|status"
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "autoai",
		Description: "Auto AI on/off per chat",
		Category:    "AI",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAutoAICommand(client, msg, args)
		},
	})
}
