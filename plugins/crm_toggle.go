package main

// crm_toggle.go — CRM message archive OFF by default.
// Aktif manual per grup via .crmon / .crmoff (owner only).
//
// 2026-10-27: migrated from data/crm_groups.json (in-memory map[string]bool)
// to PostgreSQL group_settings table. Public API unchanged.

import (
	"context"
	"fmt"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// Plugin name in group_settings table.
const crmPluginName = "crm"

// crmGroupEnabled cek apakah arsip CRM aktif untuk chat ini (default: OFF).
func crmGroupEnabled(chat string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return pgGroupGetBool(ctx, crmPluginName, chat, false)
}

// crmGroupSet enable/disable CRM archive for a chat.
func crmGroupSet(chat string, enabled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pgGroupSetBool(ctx, crmPluginName, chat, enabled)
}

// crmGroupList returns the list of groups where CRM is enabled.
func crmGroupList() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := pgGroupListEnabled(ctx, crmPluginName)
	return out
}

// ─── Commands ───

func handleCRMOn(_ *whatsmeow.Client, msg *events.Message, _ string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	chat := msg.Info.Chat.String()
	if err := crmGroupSet(chat, true); err != nil {
		return fmt.Sprintf("❌ PG: %v", err)
	}
	return "✅ Arsip CRM *AKTIF* untuk grup ini"
}

func handleCRMOff(_ *whatsmeow.Client, msg *events.Message, _ string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	chat := msg.Info.Chat.String()
	if err := crmGroupSet(chat, false); err != nil {
		return fmt.Sprintf("❌ PG: %v", err)
	}
	return "⚫ Arsip CRM *MATI* untuk grup ini"
}

func handleCRMGroups(_ *whatsmeow.Client, msg *events.Message, _ string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	groups := crmGroupList()
	if len(groups) == 0 {
		return "⚫ *Arsip CRM*\n\n• Status: mati di semua grup (default OFF)\n• Aktifkan: *.crmon* di grup yang diinginkan"
	}
	out := "🟢 *Grup Arsip CRM Aktif*\n\n"
	for _, g := range groups {
		out += "• `" + g + "`\n"
	}
	return strings.TrimSuffix(out, "\n")
}

var _ = fmt.Sprintf

func init() {
	RegisterPlugin(Plugin{
		Command:     "crmon",
		Description: "aktifkan arsip CRM di grup ini",
		Category:    "Plugin",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCRMOn(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "crmoff",
		Description: "matikan arsip CRM di grup ini",
		Category:    "Plugin",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCRMOff(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "crmgroups",
		Description: "list grup arsip CRM aktif",
		Category:    "Plugin",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCRMGroups(client, msg, args)
		},
	})
}
