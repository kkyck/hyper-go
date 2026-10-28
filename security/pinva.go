package main

// pinva.go — Command pin/unpin pesan di grup.
//
//   .pinva        (reply target) — pin pesan target
//   .pinva unpin  (reply target) — unpin pesan target
//
// Owner-only, group-only. CATATAN HASIL EKSPERIMEN (2026-10-02): pin TANPA
// admin TIDAK bisa — server WhatsApp mengembalikan 403 untuk pengirim
// non-admin. Validasi dilakukan terhadap identitas pengirim, bukan isi key,
// jadi tidak ada varian key yang bisa bypass. Command ini tetap berguna
// saat bot menjadi admin grup.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// handlePinVACommand — .pinva handler.
func handlePinVACommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	unpin := false
	for _, p := range strings.Fields(args) {
		if p == "unpin" || p == "-u" {
			unpin = true
		}
	}

	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❌ Reply pesan yang ingin di-pin"
	}
	stanzaID := quoted.Info.ID
	if stanzaID == "" {
		return "❌ Tidak bisa ambil ID pesan target"
	}
	chatID := msg.Info.Chat

	pinType := waE2E.PinInChatMessage_PIN_FOR_ALL
	if unpin {
		pinType = waE2E.PinInChatMessage_UNPIN_FOR_ALL
	}

	var participant *string
	if sender := quoted.Info.Sender; !sender.IsEmpty() {
		participant = proto.String(sender.ToNonAD().String())
	}

	pinMsg := &waE2E.Message{
		PinInChatMessage: &waE2E.PinInChatMessage{
			Key: &waCommon.MessageKey{
				RemoteJID:   proto.String(chatID.String()),
				FromMe:      proto.Bool(false),
				ID:          proto.String(stanzaID),
				Participant: participant,
			},
			Type:              pinType.Enum(),
			SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.SendMessage(ctx, chatID, pinMsg)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim pin: %v", err)
	}

	action := "Pin"
	if unpin {
		action = "Unpin"
	}
	return fmt.Sprintf("📌 %s dikirim (id=%s). Cek di grup apakah pesannya ke-pin.", action, resp.ID)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "pinva",
		Description: "pinva",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePinVACommand(client, msg, args)
			// ─── FakeMsg (edit other's msg with custom text) ──────
		},
	})
}
