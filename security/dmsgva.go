package main

// dmsgva.go — Varian eksperimen dari .dmsg (delete other's message, no admin).
//
// KOMBINASI: GroupStatusMessageV2 + afterRead disappearing dalam 1 alur.
//   Step 1: temp = GroupStatusMessageV2 (FutureProofMessage) berisi
//           ExtendedTextMessage dengan ContextInfo IsGroupStatus:true
//           + disappearingMode (CHANGED_IN_CHAT/CHAT_SETTING)
//           + afterReadDuration 5 detik. afterRead DULU, dibungkus
//           grupstatus → temp tidak terlihat sebagai pesan biasa dan
//           hilang sendiri setelah dibaca.
//   Step 2: official-structure edit (messageId = stanza target), konten
//           edit pakai IsGroupStatus:false (grupstatus di bagian kedua).
//   Step 3: cleanup anti-"bekas": jeda 800ms agar edit teraplikasi,
//           lalu revoke TARGET double-tap (2x, jeda 50ms) baru temp1.
//           Double revoke menaikkan peluang client memproses hapus
//           sehingga target tidak nyangkut sebagai "Diedit".
//
// (Mode -1 / disappearing timer 60 dtk sudah DIHAPUS atas permintaan user.)
//
// Flag -i / -I @tag tetap didukung seperti .dmsg (via parseDmsgFlags).
//
// Perbaikan bug:
//   1. "Bekas" (pesan target hanya jadi "Diedit", tidak terhapus):
//      jeda 800ms antara edit dan revoke agar edit sempat teraplikasi,
//      + revoke target 2x (double-tap).
//   2. Force close di HP anggota lain: masih dalam investigasi. Urutan
//      revoke = target dulu, baru temp1 (agar penghapusan utama tidak
//      tergantung pada revoke temp yang rawan crash).
//   3. tempID2 (envelope edit) == stanzaID target (dmsgSendWithIDEx
//      mengirim dengan ID envelope = stanza korban), jadi revoke
//      target sekaligus membersihkan envelope edit — tidak ada sisa.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// handleDMsgVACommand — .dmsgva handler.
func handleDMsgVACommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	// parseDmsgFlags menangani -i / -I @tag. Token -1/-2 disaring:
	// -1 sudah dihapus, -2 jadi alias .dmsgva polos.
	flags, rest := parseDmsgFlags(msg, args)
	for _, p := range strings.Fields(rest) {
		if p == "-1" {
			return "❌ Mode -1 sudah dihapus. Pakai *.dmsgva* saja (reply pesan target)"
		}
	}

	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❌ Reply pesan yang ingin diproses"
	}
	stanzaID := quoted.Info.ID
	if stanzaID == "" {
		return "❌ Tidak bisa ambil ID pesan target"
	}

	return dmsgvaDeleteTarget(client, msg, stanzaID, flags)
}

// dmsgvaDeleteTarget — inti trik .dmsgva: hapus pesan target via
// kombinasi grupstatus+afterRead, tanpa perlu reply (stanzaID langsung).
func dmsgvaDeleteTarget(client *whatsmeow.Client, msg *events.Message, stanzaID string, flags dmsgFlags) string {
	chatID := msg.Info.Chat

	// Step 1: KOMBINASI — afterRead DULU + grupstatus.
	// Payload disappearing-message snippet pastebin HcbBdc9r
	// (expiration:0, ephemeralSettingTimestamp:now,
	// disappearingMode:{initiator:0(CHANGED_IN_CHAT), trigger:1(CHAT_SETTING)},
	// afterReadDuration:5) DIBUNGKUS GroupStatusMessageV2 (FutureProofMessage)
	// dengan IsGroupStatus:true — seperti .dmsg, agar temp tidak
	// terlihat sebagai pesan biasa, sekaligus hilang sendiri
	// setelah dibaca (afterRead).
	initiator := waE2E.DisappearingMode_CHANGED_IN_CHAT
	trigger := waE2E.DisappearingMode_CHAT_SETTING
	tempMsg1 := &waE2E.Message{
		GroupStatusMessageV2: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(""),
					ContextInfo: &waE2E.ContextInfo{
						IsGroupStatus:             proto.Bool(true),
						Expiration:                proto.Uint32(0),
						EphemeralSettingTimestamp: proto.Int64(time.Now().Unix()),
						DisappearingMode: &waE2E.DisappearingMode{
							Initiator: &initiator,
							Trigger:   &trigger,
						},
						AfterReadDuration: proto.Uint32(5),
					},
				},
			},
		},
	}

	tempID1, err := dmsgSendEx(client, msg, tempMsg1, flags)
	if err != nil {
		return fmt.Sprintf("❌ Step 1 gagal: %v", err)
	}

	// Step 2: official-structure edit (wrapped + TimestampMS), messageId = stanza target.
	// Sama seperti .dmsg — inilah yang menghapus pesan korban di client.
	// Konten edit pakai IsGroupStatus:false (grupstatus di bagian kedua,
	// mengikuti pola .dmsg).
	// NOTE: temp1 TIDAK boleh di-revoke sebelum edit ini (eksperimen
	// revoke-duluan bikin trik gagal: edit butuh temp1 masih ada).
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

	// Step 3: cleanup anti-"bekas" — jeda 100ms agar edit teraplikasi (hampir instan)
	// dulu, lalu revoke TARGET double-tap (2x, jeda 50ms) baru temp1.
	// NOTE: tempID2 == stanzaID (dmsgSendWithIDEx mengirim edit dengan
	// envelope ID = stanza korban), jadi revoke target sekaligus
	// membersihkan envelope edit.
	time.Sleep(10 * time.Millisecond)
	if !flags.useSPR {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel2()
		_, _ = client.RevokeMessage(ctx2, chatID, types.MessageID(stanzaID))
		time.Sleep(10 * time.Millisecond)
		_, _ = client.RevokeMessage(ctx2, chatID, types.MessageID(stanzaID))
		_, _ = client.RevokeMessage(ctx2, chatID, types.MessageID(tempID1))
		return ""
	}
	// SPR mode (-i / -I): pakai cleanup bawaan dmsg via SPR.
	dmsgCleanupTemp(client, msg, chatID, tempID1, tempID2, flags)

	return ""
}

// ─── ❌ Reaction trigger ────────────────────────────────────────────

// handleDMsgVAReaction — trigger .dmsgva saat owner me-reaction ❌ pada
// sebuah pesan. Tanpa perlu reply/command: hanya terlihat reaction ❌
// dari owner di pesan tersebut, lalu pesan hilang.
// Return true jika event dikonsumsi (handleMessage harus return).
//
// Dipasang SEBELUM filter IsFromMe di handleMessage, karena owner bisa
// me-react dari akun bot itu sendiri (companion phone) — event-nya
// IsFromMe=true tapi tetap harus trigger.
func handleDMsgVAReaction(client *whatsmeow.Client, msg *events.Message) bool {
	if msg == nil || msg.Message == nil {
		return false
	}
	reaction := msg.Message.GetReactionMessage()
	if reaction == nil {
		return false
	}
	// Hanya emoji ❌ (penghapusan reaction = text kosong → tidak trigger)
	if reaction.GetText() != "❌" {
		return false
	}
	if !msg.Info.IsGroup {
		return false
	}
	// Owner: JID/phone terdaftar, ATAU IsFromMe (react dari akun
	// bot sendiri via companion). Bot tidak pernah mengirim ❌
	// sendiri (cuma 👍/👎/🤖) jadi tidak ada self-trigger.
	if !isOwnerMsg(msg) && !msg.Info.IsFromMe {
		return false
	}
	targetID := reaction.GetKey().GetID()
	if targetID == "" {
		return false
	}
	flags, _ := parseDmsgFlags(msg, "")

	// Jalan async (ada sleep 800ms+300ms) + panic recovery, tanpa
	// membalas apa pun — sesuai permintaan: tidak terlihat reply.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[dmsgva-react] panic: %v\n", r)
			}
		}()
		if res := dmsgvaDeleteTarget(client, msg, targetID, flags); res != "" {
			fmt.Printf("[dmsgva-react] %s\n", res)
		}
	}()
	return true
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "dmsgva",
		Description: "dmsgva",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleDMsgVACommand(client, msg, args)
			// ─── PinVA (eksperimen pin tanpa admin) ───────────────
		},
	})
}
