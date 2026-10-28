package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

/**
 * 💤 AFK Plugin — Away From Keyboard (file-based)
 * ════════════════════════════════════════════════════════════
 * Port dari plugins/afk.ts (Baileys)
 *
 * Commands (prefix .):
 *   .afk [alasan]   → Aktifkan status AFK
 *   .afk off        → Matikan AFK manual
 *
 * Perilaku:
 *   • User AFK mengirim pesan → AFK auto-off + sapa kembali
 *   • User lain mention / @nomor yang AFK → bot balas alasan + durasi
 *
 * Storage: ./data/afk.json + in-memory cache (kept by filestore)
 */

type AFKEntry struct {
	JID      string `json:"jid"`
	Reason   string `json:"reason"`
	Since    int64  `json:"since"` // unix ms
	PushName string `json:"push_name"`
}

func formatAFKDuration(sinceMs int64) string {
	sec := (time.Now().UnixMilli() - sinceMs) / 1000
	if sec < 60 {
		return fmt.Sprintf("%d detik", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%d menit", sec/60)
	}
	if sec < 86400 {
		return fmt.Sprintf("%d jam", sec/3600)
	}
	return fmt.Sprintf("%d hari", sec/86400)
}

func handleAFKCommand(_ any, msg *events.Message, args string) string {
	jid := msg.Info.Sender.String()
	pushName := msg.Info.PushName
	if pushName == "" {
		pushName = "kak"
	}

	args = strings.TrimSpace(args)
	lower := strings.ToLower(args)

	if lower == "off" || lower == "back" {
		deleteAFKEntry(jid)
		_ = persistAFKs()
		return fmt.Sprintf("👋 Selamat datang kembali, *%s*! Status AFK dimatikan.", pushName)
	}

	// Set AFK
	reason := args
	if reason == "" {
		reason = "Tidak ada alasan"
	}
	e := &AFKEntry{
		JID:      jid,
		Reason:   reason,
		Since:    time.Now().UnixMilli(),
		PushName: pushName,
	}
	setAFK(e)
	if err := persistAFKs(); err != nil {
		return "⚠️ Gagal menyimpan status AFK."
	}
	return fmt.Sprintf("💤 *%s* sekarang AFK.\nAlasan: %s\n\nKetik *.afk off* saat sudah kembali.", pushName, reason)
}

// checkAFKMentions: jika ada mention user AFK, balas otomatis.
// Juga auto-off jika pengirim yang AFK mengirim pesan biasa.
func checkAFKMentions(client *whatsmeow.Client, _ any, msg *events.Message, text string) {
	sender := msg.Info.Sender.String()

	// Jika pengirim sedang AFK → matikan otomatis
	if e := getAFK(sender); e != nil {
		deleteAFKEntry(sender)
		_ = persistAFKs()
		dur := formatAFKDuration(e.Since)
		sendTextMsg(client, msg.Info.Chat, fmt.Sprintf("👋 *%s* kembali setelah AFK selama %s.", e.PushName, dur))
	}

	// Cek mention di ContextInfo
	if msg.Message == nil {
		return
	}
	var mentioned []string
	if ext := msg.Message.GetExtendedTextMessage(); ext != nil && ext.ContextInfo != nil {
		for _, j := range ext.GetContextInfo().GetMentionedJID() {
			mentioned = append(mentioned, j)
		}
	}
	// Juga cek pola @628xxx di teks
	for _, word := range strings.Fields(text) {
		if strings.HasPrefix(word, "@") {
			phone := extractPhone(word)
			if phone != "" {
				mentioned = append(mentioned, phone+"@s.whatsapp.net")
			}
		}
	}

	seen := make(map[string]bool)
	for _, jid := range mentioned {
		if seen[jid] {
			continue
		}
		seen[jid] = true
		e := getAFK(jid)
		if e == nil {
			// coba tanpa device suffix
			phone := extractPhone(jid)
			e = getAFK(phone + "@s.whatsapp.net")
		}
		if e != nil {
			dur := formatAFKDuration(e.Since)
			sendTextMsg(client, msg.Info.Chat, fmt.Sprintf("💤 *%s* sedang AFK.\nAlasan: %s\nSejak: %s yang lalu", e.PushName, e.Reason, dur))
		}
	}
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "afk",
		Description: "afk",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAFKCommand(nil, msg, args)
			// ─── Stats / Process ────────────────────────────────────
		},
	})
}
