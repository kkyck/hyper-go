package main

// playcall.go — .playcall command: reply audio → send as voice note to target
//
// whatsmeow tidak punya Call API (hanya RejectCall), jadi .playcall
// mengirim audio sebagai voice note (PTT) ke target via private chat.
// Voice note auto-play saat diterima — paling mendekati "menyetelkan mp3
// lewat telefon".
//
// Usage:
//   .playcall @tag      (reply pesan audio)
//   .playcall 628xxx    (reply pesan audio)

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

func handlePlayCallCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa pakai .playcall"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	// Must reply to an audio message
	quoted := getQuotedMessage(msg)
	if quoted == nil || quoted.Message == nil {
		return "❌ Reply pesan audio (mp3/voice note) dulu\n❓ *Cara pakai:* .playcall @tag (reply audio)"
	}

	// Find audio message in quoted
	audioMsg := quoted.Message.GetAudioMessage()
	if audioMsg == nil {
		// Try unwrapping
		if doc := quoted.Message.GetDocumentWithCaptionMessage(); doc != nil {
			audioMsg = doc.GetMessage().GetAudioMessage()
		}
	}
	if audioMsg == nil {
		return "❌ Pesan yang di-reply bukan audio\n❓ *Cara pakai:* reply voice note atau file audio lalu ketik .playcall @tag"
	}

	// Resolve target
	target, err := lacakResolveTarget(client, msg, args)
	if err != nil || target.IsEmpty() {
		return "❓ " + err.Error()
	}

	// Download audio
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Printf("[PLAYCALL] downloading audio from quoted...\n")
	audioData, err := client.Download(ctx, audioMsg)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download audio: %v", err)
	}

	mime := audioMsg.GetMimetype()
	if mime == "" {
		mime = "audio/mpeg"
	}

	fmt.Printf("[PLAYCALL] audio downloaded: %d bytes, mime=%s\n", len(audioData), mime)

	// Upload as audio
	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer uploadCancel()

	up, err := client.Upload(uploadCtx, audioData, whatsmeow.MediaAudio)
	if err != nil {
		return fmt.Sprintf("❌ Upload gagal: %v", err)
	}

	// Send as voice note (PTT=true) to target's private chat
	targetChat := types.NewJID(target.User, types.DefaultUserServer)

	// Generate waveform (simple: 32 bars, all same height)
	waveform := make([]byte, 32)
	for i := range waveform {
		waveform[i] = 50 // mid-level
	}

	// Get duration from audio message (if available)
	seconds := audioMsg.GetSeconds()
	if seconds == 0 {
		seconds = uint32(10) // default 10s
	}

	audioOut := &waE2E.Message{
		AudioMessage: &waE2E.AudioMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(audioData))),
			Seconds:       proto.Uint32(seconds),
			PTT:           proto.Bool(true), // voice note = auto-play
			Waveform:      waveform,
		},
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()

	resp, err := client.SendMessage(sendCtx, targetChat, audioOut)
	if err != nil {
		return fmt.Sprintf("❌ Kirim voice note gagal: %v", err)
	}

	fmt.Printf("[PLAYCALL] voice note sent to %s: id=%s\n", targetChat.String(), resp.ID)

	// Silent on success
	return ""
}

// handleCallCommand — .call: reject incoming call or show call status
// (whatsmeow tidak support outbound calls — hanya RejectCall)
func handleCallCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	return `📞 *Call Status*

• whatsmeow tidak mendukung outbound calls
• Hanya bisa RejectCall (tolak panggilan masuk)

❓ *Cara pakai:* .playcall @tag (reply audio) — kirim voice note ke target, auto-play saat diterima di HP target`
}

// keep imports used
var _ = io.EOF
var _ = http.StatusOK
var _ = bytes.MinRead
var _ = strings.TrimSpace

func init() {
	RegisterPlugin(Plugin{
		Command:     "playcall",
		Description: "playcall",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlayCallCommand(client, msg, args)
			// ─── Lacak (OSINT tracking via SPR + rich card) ────────
		},
	})
}
