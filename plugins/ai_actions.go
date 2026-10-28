package main

// ai_actions.go — AI Agent action execution via natural language
// ════════════════════════════════════════════════════════════
//
// AI agent pattern: user asks in natural language → AI detects intent
// + extracts parameters → EXECUTES action → returns result.
//
// EXAMPLES:
//   User: "tolong kick kangwifi"
//   Bot:  ✅ kangwifi telah dikick dari grup
//
//   User: "promote si budi jadi admin"
//   Bot:  ✅ budi telah dipromote jadi admin
//
//   User: "ganti nama grup jadi Test Group"
//   Bot:  ✅ Nama grup diubah ke "Test Group"
//
//   User: "tag semua member"
//   Bot:  [tagall execution]
//
// ACTION INTENTS:
//   - kick:     "kick X", "keluarkan X", "tendang X"
//   - promote:  "promote X", "jadikan X admin", "angkat X jadi admin"
//   - demote:   "demote X", "cabut admin X", "turunkan X"
//   - tagall:   "tag semua", "panggil semua", "mention semua"
//   - setname:  "ganti nama grup jadi X", "rename grup ke X"
//   - setdesc:  "ganti deskripsi grup jadi X", "ubah desc ke X"
//   - link:     "buat link grup", "link invite"
//   - revoke:   "revoke link", "reset link grup"
//
// TARGET EXTRACTION:
//   - @tag mention → use MentionedJIDList
//   - Phone number → parse as JID
//   - Name match   → search participant list for matching name
//   - Reply target → use replied message sender
//
// All actions are ROLE-GATED:
//   - kick/promote/demote: owner or admin grup
//   - tagall: owner or admin
//   - setname/setdesc: owner or admin
//   - revoke link: owner or admin

import (
	"context"
	"fmt"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Execute action ───────────────────────────────────────────────

// ─── Individual actions ───────────────────────────────────────────

func aiActionKick(client *whatsmeow.Client, msg *events.Message, targetHint string) string {
	targetJID, targetLabel, err := aiResolveTarget(client, msg, targetHint)
	if err != nil {
		return "❓ " + err.Error()
	}
	// Pakai JID yang sudah ter-resolve (targetJID.User = nomor telepon).
	// Label bisa berupa nama panggilan yang tidak bisa di-parse ulang
	// oleh handleGroupAction -> pakai nomor agar kick by name tetap jalan.
	args := targetJID.User
	result := handleGroupAction(client, msg, "kick", args)
	if strings.Contains(result, "✅") || strings.Contains(result, "berhasil") {
		return fmt.Sprintf("✅ %s telah dikick dari grup", targetLabel)
	}
	return result
}

func aiActionPromote(client *whatsmeow.Client, msg *events.Message, targetHint string) string {
	targetJID, targetLabel, err := aiResolveTarget(client, msg, targetHint)
	if err != nil {
		return "❓ " + err.Error()
	}
	args := targetJID.User
	result := handleGroupAction(client, msg, "promote", args)
	if strings.Contains(result, "✅") || strings.Contains(result, "berhasil") {
		return fmt.Sprintf("✅ %s telah dipromote jadi admin", targetLabel)
	}
	return result
}

func aiActionDemote(client *whatsmeow.Client, msg *events.Message, targetHint string) string {
	targetJID, targetLabel, err := aiResolveTarget(client, msg, targetHint)
	if err != nil {
		return "❓ " + err.Error()
	}
	args := targetJID.User
	result := handleGroupAction(client, msg, "demote", args)
	if strings.Contains(result, "✅") || strings.Contains(result, "berhasil") {
		return fmt.Sprintf("✅ %s telah didemote dari admin", targetLabel)
	}
	return result
}

// ─── Target resolution ────────────────────────────────────────────

// aiResolveTarget extracts target JID from various sources:
//  1. @tag mention (MentionedJIDList)
//  2. Phone number in text
//  3. Name match in group participant list
//  4. Reply target (if replying to someone)
//
// Returns (JID, label, error).
func aiResolveTarget(client *whatsmeow.Client, msg *events.Message, hint string) (types.JID, string, error) {
	hint = strings.TrimSpace(hint)

	// 1. Try @tag mention first (most reliable)
	mentions := extractMentionedJIDsDual(msg)
	if len(mentions) > 0 {
		if j, err := parseAnyJID(mentions[0]); err == nil && !j.IsEmpty() {
			label := j.User
			if hint != "" {
				label = strings.TrimPrefix(hint, "@")
			}
			return j, label, nil
		}
	}

	// 2. Try phone number
	if hint != "" {
		phone := strings.TrimPrefix(hint, "@")
		phone = strings.TrimPrefix(phone, "+")
		phone = strings.TrimPrefix(phone, "0")
		if !strings.HasPrefix(phone, "62") && len(phone) >= 8 && len(phone) <= 15 {
			phone = "62" + phone
		}
		if len(phone) >= 8 && len(phone) <= 15 {
			jid := types.NewJID(phone, types.DefaultUserServer)
			return jid, phone, nil
		}
	}

	// 3. Try name match in group participants
	if client != nil && msg.Info.IsGroup && hint != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
		if err == nil && meta != nil {
			hintLower := strings.ToLower(strings.TrimSpace(hint))
			for _, p := range meta.Participants {
				// Check if JID user matches hint
				if strings.Contains(p.JID.User, hintLower) {
					return p.JID, p.JID.User, nil
				}
			}
		}
	}

	// 4. Try reply target
	if quoted := getQuotedMessage(msg); quoted != nil && !quoted.Info.Sender.IsEmpty() {
		sender := quoted.Info.Sender.ToNonAD()
		return sender, sender.User, nil
	}

	return types.EmptyJID, "", fmt.Errorf("target tidak ditemukan. Pakai @tag mention atau nomor telepon.")
}

// keep imports used
var _ = whatsmeow.MediaAudio
