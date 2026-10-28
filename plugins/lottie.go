package main

// lottie.go — Sticker reaction plugin (reply sticker + .lottie → reaction).
// ════════════════════════════════════════════════════════════
//
// Usage:
//   1. Reply to ANY sticker in the chat
//   2. Type .lottie
//   3. Bot sends the SAME sticker as a "sticker reaction" (annotation)
//      to that message via MessageAssociation(STICKER_ANNOTATION=11)
//
// The sticker's URL/hashes/mediaKey are reused from the replied sticker
// — no download/re-upload needed. WA accepts because the media reference
// is valid (belongs to whoever originally sent the sticker).

import (
	"context"
	"fmt"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// sendStickerReaction sends the replied sticker as a sticker reaction
// (annotation) to the replied message itself.
func sendStickerReaction(client *whatsmeow.Client, cmdMsg *events.Message) error {
	if client == nil || cmdMsg == nil {
		return fmt.Errorf("nil")
	}

	// Get the replied-to message
	quoted := getQuotedMessage(cmdMsg)
	if quoted == nil {
		return fmt.Errorf("reply stiker dulu, lalu ketik .lottie")
	}

	// Check if the replied message is a sticker
	quotedSticker := quoted.Message.GetStickerMessage()
	if quotedSticker == nil {
		return fmt.Errorf("yang di-reply bukan stiker — reply stiker lalu .lottie")
	}

	fmt.Printf("[lottie] replied sticker: isLottie=%v, mimetype=%s\n",
		quotedSticker.GetIsLottie(), quotedSticker.GetMimetype())

	// Build parentMessageKey pointing to the replied sticker message
	parentKey := &waCommon.MessageKey{
		RemoteJID: proto.String(quoted.Info.Chat.String()),
		FromMe:    proto.Bool(quoted.Info.IsFromMe),
		ID:        proto.String(quoted.Info.ID),
	}

	// AssociationType = 11 (STICKER_ANNOTATION)
	assocType := waE2E.MessageAssociation_STICKER_ANNOTATION

	// Build the reaction message: SAME sticker + MessageAssociation
	// Reuse all fields from the replied sticker (URL, hashes, mediaKey, etc.)
	waMsg := &waE2E.Message{
		MessageContextInfo: &waE2E.MessageContextInfo{
			MessageAssociation: &waE2E.MessageAssociation{
				AssociationType:  &assocType,
				ParentMessageKey: parentKey,
			},
		},
		StickerMessage: &waE2E.StickerMessage{
			URL:           quotedSticker.URL,
			FileSHA256:    quotedSticker.FileSHA256,
			FileEncSHA256: quotedSticker.FileEncSHA256,
			MediaKey:      quotedSticker.MediaKey,
			Mimetype:      quotedSticker.Mimetype,
			DirectPath:    quotedSticker.DirectPath,
			FileLength:    quotedSticker.FileLength,
			Height:        quotedSticker.Height,
			Width:         quotedSticker.Width,
			IsLottie:      quotedSticker.IsLottie,
		},
	}

	fmt.Printf("[lottie] sending sticker reaction to msg %s...\n", quoted.Info.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.SendMessage(ctx, cmdMsg.Info.Chat, waMsg)
	if err != nil {
		fmt.Printf("[lottie] FAILED: %v\n", err)
		return fmt.Errorf("send: %w", err)
	}

	fmt.Printf("[lottie] OK, msg_id=%s\n", resp.ID)
	return nil
}

func handleLottieCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	fmt.Printf("[lottie] from %s in %s\n", msg.Info.Sender.String(), msg.Info.Chat.String())
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if err := sendStickerReaction(client, msg); err != nil {
		fmt.Printf("[lottie] error: %v\n", err)
		return fmt.Sprintf("❌ %v", err)
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "lottie",
		Description: "Sticker reaction — reply sticker + .lottie",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleLottieCommand(client, msg, args)
		},
	})
}
