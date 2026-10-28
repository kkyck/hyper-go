package main

// humanmsg.go — Human-like message sender
//
// Wraps all outgoing messages to look 100% like they come from official
// WhatsApp Web client. Sets:
//   - MessageContextInfo with 32-byte MessageSecret (always)
//   - DeviceListMetadata (empty but present — matches WhatsApp Web)
//   - DeviceListMetadataVersion = 2

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// humanizeMessage wraps a message with proper MessageContextInfo.
func humanizeMessage(msg *waE2E.Message) *waE2E.Message {
	if msg == nil {
		return msg
	}

	// ANTIBOT: hapus semua penanda bot agar terlihat seperti official client.
	msg = stripBotMarkers(msg)

	if msg.MessageContextInfo == nil {
		msg.MessageContextInfo = &waE2E.MessageContextInfo{}
	}

	if msg.MessageContextInfo.MessageSecret == nil || len(msg.MessageContextInfo.MessageSecret) != 32 {
		secret := make([]byte, 32)
		_, _ = rand.Read(secret)
		msg.MessageContextInfo.MessageSecret = secret
	}

	if msg.MessageContextInfo.DeviceListMetadata == nil {
		msg.MessageContextInfo.DeviceListMetadata = &waE2E.DeviceListMetadata{}
	}

	if msg.MessageContextInfo.DeviceListMetadataVersion == nil {
		msg.MessageContextInfo.DeviceListMetadataVersion = proto.Int32(2)
	}

	return msg
}

func humanSendReply(client *whatsmeow.Client, msg *events.Message, text string) (string, error) {
	return humanSendReplyWithID(client, msg, text, "")
}

// humanSendReplyWithID sends a reply with an OPTIONAL custom message ID.
// If msgID is empty, whatsmeow auto-generates (default).
// If msgID is non-empty (e.g. "aiagent-1697000000"), the reply is sent
// with that exact ID — allowing the bot to recognize its own AI replies
// when the user replies back (without needing to track IDs in memory).
func humanSendReplyWithID(client *whatsmeow.Client, msg *events.Message, text, msgID string) (string, error) {
	// ─── Stealth primary hook (per-group) ─────────────────────────
	// Jika mode .stealth primary aktif, balasan dikirim senyap hanya ke
	// perangkat utama (HP) target — perangkat tertaut tidak menerima.
	if isStealthPrimaryActive(msg.Info.Chat.String()) && msg.Info.IsGroup {
		if sendReplyViaStealthPrimary(client, msg, text) {
			return "", nil
		}
	}
	// ─── Self SPR hook (per-group) ────────────────────────────────
	if isSelfSprActive(msg.Info.Chat.String()) && msg.Info.IsGroup {
		if sendReplyViaSPR2(client, msg, text) {
			return "", nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	quotedText := extractText(msg)
	quoted := &waE2E.Message{
		Conversation: proto.String(quotedText),
	}
	if msg.Message.GetExtendedTextMessage() != nil {
		quoted = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(quotedText),
			},
		}
	}

	respMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String(msg.Info.ID),
				Participant:   proto.String(msg.Info.Sender.String()),
				QuotedMessage: quoted,
			},
		},
	}

	respMsg = humanizeMessage(respMsg)

	// ─── Autodel hook (per-group) ─────────────────────────────
	// Kalau autodel ON di grup ini, pesan bot dikirim dengan payload
	// disappearing persis Baileys (afterReadDuration: 5).
	if msg.Info.IsGroup {
		applyAutodelPayload(respMsg, msg.Info.Chat.String())
	}

	// Send with custom message ID if provided
	if msgID != "" {
		resp, err := client.SendMessage(ctx, msg.Info.Chat, respMsg, whatsmeow.SendRequestExtra{ID: msgID})
		if err != nil {
			return "", err
		}
		sentID := msgID
		// If server assigned a different ID, fall back to it
		if resp.ID != "" {
			sentID = resp.ID
		}
		qCacheStoreOutgoing(msg.Info.Chat, sentID, respMsg)
		return sentID, nil
	}

	resp, err := client.SendMessage(ctx, msg.Info.Chat, respMsg)
	if err != nil {
		return "", err
	}
	qCacheStoreOutgoing(msg.Info.Chat, resp.ID, respMsg)
	return resp.ID, nil
}

// qCacheStoreOutgoing menyimpan pesan KELUAR bot ke cache .q agar rantai
// reply bisa diikuti balik (misal .q me-reply error bot → ulangi command).
func qCacheStoreOutgoing(chat types.JID, id string, waMsg *waE2E.Message) {
	if id == "" || waMsg == nil {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		out := &events.Message{Message: waMsg}
		out.Info.ID = id
		out.Info.Chat = chat
		out.Info.IsFromMe = true
		out.Info.Timestamp = time.Now()
		qCacheStore(out)
	}()
}

func humanSendText(client *whatsmeow.Client, chat types.JID, text string) error {
	// Stealth primary hook — jika aktif, kirim hanya ke perangkat utama
	if sendStealthPrimaryText(client, chat, text) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
		},
	}
	msg = humanizeMessage(msg)
	_, err := client.SendMessage(ctx, chat, msg)
	return err
}

func humanSendWithExtra(client *whatsmeow.Client, ctx context.Context, chat types.JID, msg *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	// Stealth primary hook — hanya untuk pesan teks
	if txt := msg.GetExtendedTextMessage().GetText(); txt != "" {
		if sendStealthPrimaryText(client, chat, txt) {
			return whatsmeow.SendResponse{}, nil
		}
	}
	if txt := msg.GetConversation(); txt != "" {
		if sendStealthPrimaryText(client, chat, txt) {
			return whatsmeow.SendResponse{}, nil
		}
	}
	msg = humanizeMessage(msg)
	return client.SendMessage(ctx, chat, msg, extra)
}
