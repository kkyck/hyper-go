package main

/**
 * 📋 menu.go — Menu terpisah: Owner / Admin Grup / User
 * ════════════════════════════════════════════════════════════
 * Visual structure:
 *   ┌─ Header (bot name + version + prefix)
 *   ├─ Section block (emoji + bold title)
 *   ├─ bullet list (• .cmd — desc)
 *   └─ Footer (status line + hint)
 *
 * Each menu is role-gated:
 *   - isOwnerMsg    → menuOwner  (full access)
 *   - isGroupAdmin  → menuAdmin  (moderasi + guard + SPR)
 *   - default       → menuUser   (basic commands)
 */

import (
	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

const (
	botMenuVersion = "v2.6"
	botMenuPrefix  = "."
)

// menuHeader returns the standard top-of-menu banner.
func menuHeader(title, subtitle string) string {
	return "╭─── ⋆ ⋆ ⋆ ───╮\n" +
		"  🤖 *WABOT-GO* " + botMenuVersion + "\n" +
		"  " + title + "\n" +
		"╰─── ⋆ ⋆ ⋆ ───╯\n" +
		"Prefix: *" + botMenuPrefix + "* | " + subtitle + "\n" +
		"─────────────────"
}

// menuFooter returns the standard bottom-of-menu hint.
func menuFooter(extra string) string {
	out := "─────────────────\n"
	if extra != "" {
		out += extra + "\n"
	}
	out += "Ketik *" + botMenuPrefix + "menu* untuk buka lagi."
	return out
}

func handleMenu(client *whatsmeow.Client, msg *events.Message) string {
	if msg != nil && isOwnerMsg(msg) {
		return menuOwner()
	}
	if msg != nil && msg.Info.IsGroup && isGroupAdmin(client, msg) {
		return menuAdmin()
	}
	return menuUser()
}

// ──────────────────────────────────────────────────────────────────────────────
// USER MENU
// ──────────────────────────────────────────────────────────────────────────────

func menuUser() string {
	return menuHeader("👤 MENU USER", "Akses umum") + "\n\n" +
		menuPluginsSection(false) +
		menuFooter("")
}

// ──────────────────────────────────────────────────────────────────────────────
// ADMIN GRUP MENU
// ──────────────────────────────────────────────────────────────────────────────

func menuAdmin() string {
	return menuHeader("🛡️ MENU ADMIN GRUP", "Khusus admin grup ini") + "\n\n" +
		menuPluginsSection(false) +
		menuFooter("")
}

// ──────────────────────────────────────────────────────────────────────────────
// OWNER MENU
// ──────────────────────────────────────────────────────────────────────────────

func menuOwner() string {
	return menuHeader("👑 MENU OWNER", "Full akses") + "\n\n" +
		menuPluginsSection(true) +
		menuFooter("")
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "menu",
		Aliases:     []string{"help", "listcmd"},
		Description: "menu",
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMenu(client, msg)
		},
	})
}
