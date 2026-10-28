package main

// textone.go — .textone <teks>: kirim pesan teks view-once (sekali lihat).
//
// Membungkus ExtendedTextMessage dalam ViewOnceMessage (field 37),
// pola yang sama dengan view-once di richcard.go. Penerima melihat bubble
// "1" yang hilang setelah dibuka sekali.

import (
	"context"
	"fmt"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

func handleTextOne(client *whatsmeow.Client, msg *events.Message, args string) string {
	text := strings.TrimSpace(args)
	if text == "" {
		return "❓ *Cara pakai:* .textone <teks>"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	inner := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
		},
	}
	voMsg := &waE2E.Message{
		ViewOnceMessage: &waE2E.FutureProofMessage{Message: inner},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.SendMessage(ctx, msg.Info.Chat, voMsg); err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "textone",
		Aliases:     []string{"tonce", "sekalilihat"},
		Description: "Kirim teks view-once (sekali lihat)",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTextOne(client, msg, args)
		},
	})
}
