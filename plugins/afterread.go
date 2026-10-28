package main

// afterread.go — .afterread <teks>: kirim SATU pesan teks dengan payload
// disappearing .autodel (hilang 5 detik setelah dibaca), tanpa perlu
// mengaktifkan mode .autodel untuk grup.
//
// Contoh: .afterread hello  → pesan "hello" terkirim dengan payload
// afterReadDuration: 5 (sama persis payload Baileys di autodel.go).
//
// Diam saat sukses (seperti .textone). Bisa dipakai di grup maupun chat pribadi.

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

func handleAfterRead(client *whatsmeow.Client, msg *events.Message, args string) string {
	text := strings.TrimSpace(args)
	if text == "" {
		return "❓ *Cara pakai:* .afterread <teks>\n\nContoh: .afterread hello"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	waMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
		},
	}
	// Paksa payload disappearing .autodel untuk pesan ini saja.
	fillAutodelPayload(waMsg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.SendMessage(ctx, msg.Info.Chat, waMsg); err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "afterread",
		Aliases:     []string{"setelahbaca"},
		Description: "Kirim teks yang hilang 5 detik setelah dibaca",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAfterRead(client, msg, args)
		},
	})
}
