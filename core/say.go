package main

// say.go — .say command: make it look like someone else said something
//
// Usage:
//   .say @user teks         → edit random message from @user
//   .say ? teks             → edit random message from replied user
//   .say @user teks -i      → SPR: only primary device sees
//   .say @user teks -I @tag → only @tag person sees the edit

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

func handleSayCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	// Parse flags (-i or -I @tag). Pass msg so mentions can be resolved.
	flags, cleanArgs := parseDmsgFlags(msg, args)
	input := strings.TrimSpace(cleanArgs)
	if input == "" {
		return "Format:\n.say @user teks\n.say ? teks\n.say @user teks -i\n.say @user teks -I @tag"
	}

	quotedMode := strings.HasPrefix(input, "?")
	var target types.JID
	var text string

	if quotedMode {
		q := getQuotedMessage(msg)
		if q == nil || q.Info.Sender.IsEmpty() {
			return "Reply pesan user yang ingin digunakan."
		}
		target = q.Info.Sender.ToNonAD()
		text = strings.TrimSpace(strings.TrimPrefix(input, "?"))
	} else {
		// Get mentions from WhatsApp's ContextInfo.MentionedJIDList.
		// The first @-prefixed token in `input` is the target mention.
		mentions := extractMentionedJIDsDual(msg)

		// Find the first @-prefixed token in the cleaned input.
		// This corresponds to the FIRST entry in MentionedJIDList.
		fields := strings.Fields(input)
		mentionPos := -1
		for i, p := range fields {
			if strings.HasPrefix(p, "@") && len(p) >= 2 {
				mentionPos = i
				break
			}
		}

		if mentionPos >= 0 && len(mentions) > 0 {
			// Resolve first mention to JID
			if j, err := parseAnyJID(mentions[0]); err == nil && !j.IsEmpty() {
				target = j.ToNonAD()
			}
		}

		// Strip the target's mention token from input.
		// The token might be "@displayname", "@phonenumber", "@liduser",
		// or might contain hidden chars (already stripped in parseDmsgFlags).
		// Strategy:
		//   1. Remove the first @-prefixed token (visible mention display)
		//   2. Also remove any raw JID string that might have leaked
		//   3. Also remove target.User (LID/PN digits) — fixes the leak
		//      where WhatsApp renders mention as "@221217021784141"
		text = input

		// 1. Remove raw JID strings (e.g. "221217021784141@lid")
		for _, m := range mentions {
			text = strings.ReplaceAll(text, m, "")
		}

		// 2. Remove the first @-prefixed token (whatever form it takes)
		fields = strings.Fields(text)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
			text = strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		}

		// 3. Remove target.User (bare digits) — fixes "@221217021784141 jelek" → "jelek"
		//    This handles the case where WA renders the mention as @<lid_user>
		//    and the JID form "@lid" suffix was never in the visible text.
		if !target.IsEmpty() {
			text = strings.ReplaceAll(text, "@"+target.User, "")
			text = strings.ReplaceAll(text, target.User, "")
		}

		// 4. Also strip dual forms (PN↔LID) in case target is LID but text shows PN
		if client != nil {
			for _, d := range dualForms(client, target) {
				if d.User != "" && d.User != target.User {
					text = strings.ReplaceAll(text, "@"+d.User, "")
					text = strings.ReplaceAll(text, d.User, "")
				}
			}
		}

		// 5. Trim leftover leading @ and whitespace
		text = strings.TrimSpace(strings.TrimLeft(text, "@ "))
	}

	if target.IsEmpty() {
		if quotedMode {
			return "Reply pesan user."
		}
		return "Mention user."
	}
	if text == "" {
		return "Masukkan teks pengganti."
	}

	// Find a random message from target in CRM cache
	stanzaID := sayFindMessageFromTarget(client, msg.Info.Chat.String(), target)
	if stanzaID == "" {
		if quotedMode {
			q := getQuotedMessage(msg)
			if q != nil && q.Info.ID != "" {
				stanzaID = q.Info.ID
			}
		}
	}
	if stanzaID == "" {
		return "❌ Tidak ada pesan dari user tersebut di cache."
	}

	chatID := msg.Info.Chat

	// Step 1: Send extendedTextMessage placeholder (isGroupStatus=true)
	tempMsg1 := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(""),
			ContextInfo: &waE2E.ContextInfo{
				IsGroupStatus: proto.Bool(true),
			},
		},
	}

	tempID1, err := dmsgSendEx(client, msg, tempMsg1, flags)
	if err != nil {
		return fmt.Sprintf("❌ Step 1 gagal: %v", err)
	}

	// Step 2: Send protocolMessage type 14 (edit) with messageId = target stanza
	editType := waE2E.ProtocolMessage_MESSAGE_EDIT
	tempMsg2 := &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(chatID.String()),
				FromMe:    proto.Bool(true),
				ID:        proto.String(tempID1),
			},
			Type: &editType,
			EditedMessage: &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(text),
					ContextInfo: &waE2E.ContextInfo{
						IsGroupStatus: proto.Bool(false),
					},
				},
			},
		},
	}

	tempID2, err := dmsgSendWithIDEx(client, msg, tempMsg2, flags, stanzaID)
	if err != nil {
		return fmt.Sprintf("❌ Step 2 gagal: %v", err)
	}

	// Step 3: Always cleanup temp messages
	time.Sleep(100 * time.Millisecond)
	dmsgCleanupTemp(client, msg, chatID, tempID1, tempID2, flags)

	return ""
}

func sayFindMessageFromTarget(client *whatsmeow.Client, chat string, target types.JID) string {
	list := crmListByChat(chat, 30)
	if len(list) == 0 {
		return ""
	}

	targetPhone := phoneOf(client, target)
	if targetPhone == "" {
		targetPhone = target.User
	}

	var candidates []*crmEntry
	for _, e := range list {
		senderPhone := extractPhone(e.Sender)
		if senderPhone != targetPhone && e.Sender != target.String() {
			senderJID, err := parseAnyJID(e.Sender)
			if err != nil || senderJID.User != target.User {
				continue
			}
		}
		if e.Content != nil {
			if ext := e.Content.GetExtendedTextMessage(); ext != nil {
				if ext.ContextInfo != nil && ext.ContextInfo.GetQuotedMessage() != nil {
					continue
				}
			}
		}
		candidates = append(candidates, e)
	}

	if len(candidates) == 0 {
		return ""
	}

	idx := rand.Intn(len(candidates))
	return candidates[idx].ID
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "say",
		Description: "say",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSayCommand(client, msg, args)
			// ─── SBPR (SPR queue mode — anteran) ───────────────────
		},
	})
}
