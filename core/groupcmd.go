package main

/**
 * 📋 groupcmd.go — Group info & hidetag commands
 * ════════════════════════════════════════════════════════════
 * Commands (prefix .):
 *   .groupinfo / .ginfo → detail grup
 *   .hidetag / .ht / .everyone <text> → mention all members
 */

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

func handleGroupInfoCmd(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Perintah ini hanya untuk *grup*."
	}
	ctx := context.Background()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || meta == nil {
		return fmt.Sprintf("⚠️ Gagal ambil info grup: %v", err)
	}

	created := "-"
	if !meta.GroupCreated.IsZero() {
		created = meta.GroupCreated.In(time.FixedZone("WIB", 7*3600)).Format("02-01-2006 15:04")
	}

	owner := "-"
	if !meta.OwnerJID.IsEmpty() {
		owner = meta.OwnerJID.User
	}

	announce := "Tidak"
	if meta.IsAnnounce {
		announce = "Ya (hanya admin kirim pesan)"
	}
	locked := "Tidak"
	if meta.IsLocked {
		locked = "Ya (hanya admin edit info)"
	}

	return fmt.Sprintf(`👥 *Info Grup*

• Nama: *%s*
• ID: %s
• Owner: %s
• Member: *%d*
• Dibuat: %s
• Mode pengumuman: %s
• Info dikunci: %s
• Deskripsi:
%s`,
		meta.Name,
		meta.JID.String(),
		owner,
		len(meta.Participants),
		created,
		announce,
		locked,
		strings.TrimSpace(meta.Topic),
	)
}

func handleHidetag(client *whatsmeow.Client, msg *events.Message, text string) string {
	if !msg.Info.IsGroup {
		return "❌ Hidetag hanya di *grup*."
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner atau admin grup yang bisa .hidetag"
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "❓ Cara pakai: *.hidetag <pesan>*\nContoh: *.hidetag Gabung meeting jam 8*"
	}

	ctx := context.Background()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || meta == nil {
		return fmt.Sprintf("⚠️ Gagal ambil member grup: %v", err)
	}

	var mentions []string
	for _, p := range meta.Participants {
		if p.JID.User != "" {
			mentions = append(mentions, p.JID.String())
		}
	}
	if len(mentions) == 0 {
		return "⚠️ Tidak ada participant yang bisa di-mention."
	}

	msgOut := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				MentionedJID: mentions,
			},
		},
	}
	_, err = client.SendMessage(ctx, msg.Info.Chat, msgOut)
	if err != nil {
		return fmt.Sprintf("⚠️ Gagal kirim hidetag: %v", err)
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "groupinfo",
		Aliases:     []string{"ginfo"},
		Description: "groupinfo",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGroupInfoCmd(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "hidetag",
		Aliases:     []string{"ht", "everyone"},
		Description: "hidetag",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleHidetag(client, msg, args)
			// ─── Sticker ─────────────────────────────────────────────
		},
	})
}
