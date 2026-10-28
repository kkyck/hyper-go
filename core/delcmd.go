package main

/**
 * 🗑️ delcmd.go — .del (delete / revoke replied message)
 * ════════════════════════════════════════════════════════════
 * Reply ke pesan lalu ketik .del
 *   - Jika pesan bot sendiri → selalu bisa dihapus (revoke)
 *   - Jika pesan orang lain di grup → bot wajib jadi admin grup
 *   - Di private chat hanya bisa hapus pesan bot sendiri
 */

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

func handleDelCommand(client *whatsmeow.Client, msg *events.Message) string {
	if msg == nil || msg.Message == nil {
		return "❓ Cara pakai: *reply* pesan yang mau dihapus, lalu ketik *.del*"
	}

	// Prefer ExtendedTextMessage context; also accept caption-style replies
	var stanzaID, participant string
	if ext := msg.Message.GetExtendedTextMessage(); ext != nil && ext.ContextInfo != nil {
		stanzaID = ext.ContextInfo.GetStanzaID()
		participant = ext.ContextInfo.GetParticipant()
	}
	if stanzaID == "" {
		if img := msg.Message.GetImageMessage(); img != nil && img.ContextInfo != nil {
			stanzaID = img.ContextInfo.GetStanzaID()
			participant = img.ContextInfo.GetParticipant()
		}
	}
	if stanzaID == "" {
		return "❓ Cara pakai: *reply* pesan yang mau dihapus, lalu ketik *.del*"
	}

	isBotMessage := isQuotedFromBot(client, participant)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if isBotMessage {
		_, err := client.RevokeMessage(ctx, msg.Info.Chat, types.MessageID(stanzaID))
		if err != nil {
			return fmt.Sprintf("⚠️ Gagal hapus pesan bot: %v", err)
		}
		return ""
	}

	if !msg.Info.IsGroup {
		return "❌ Di private chat hanya bisa menghapus pesan bot sendiri."
	}

	if !isBotGroupAdmin(client, msg.Info.Chat) {
		return "❌ Bot belum jadi *admin grup*. Jadikan bot admin dulu supaya bisa hapus pesan member."
	}

	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner atau admin grup yang bisa menghapus pesan orang lain."
	}

	_, err := client.RevokeMessage(ctx, msg.Info.Chat, types.MessageID(stanzaID))
	if err != nil {
		return fmt.Sprintf("⚠️ Gagal hapus pesan: %v\nPastikan bot adalah admin dan pesan belum terlalu lama.", err)
	}
	return "✅ Pesan dihapus."
}

// isQuotedFromBot checks if the quoted message participant is the bot (PN or LID).
func isQuotedFromBot(client *whatsmeow.Client, participant string) bool {
	if participant == "" {
		// Empty participant is ambiguous; only treat as bot in private chats handled by caller.
		// Safer: not assume bot.
		return false
	}
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return false
	}

	botPN := client.Store.ID.ToNonAD()
	botPhone := botPN.User
	part := strings.TrimSpace(participant)

	// Direct string match (with/without device)
	if part == botPN.String() || stripDevice(part) == botPN.String() {
		return true
	}
	if extractPhone(part) == botPhone && (strings.Contains(part, "@s.whatsapp.net") || strings.Contains(part, "@c.us")) {
		return true
	}

	// LID of bot
	botLID := client.Store.GetLID()
	if !botLID.IsEmpty() {
		if part == botLID.ToNonAD().String() || stripDevice(part) == botLID.ToNonAD().String() {
			return true
		}
		if extractPhone(part) == botLID.User && strings.Contains(part, "@lid") {
			return true
		}
	}

	// Resolve participant via mapping
	if parsed, err := types.ParseJID(part); err == nil {
		pn := jidToPN(client, parsed)
		if isPN(pn) && pn.User == botPhone {
			return true
		}
		lid := jidToLID(client, parsed)
		if !botLID.IsEmpty() && isLID(lid) && lid.User == botLID.User {
			return true
		}
	}
	return false
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "del",
		Aliases:     []string{"delete", "hapus"},
		PreGate:     true,
		Description: "del",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleDelCommand(client, msg)
		},
	})
}
