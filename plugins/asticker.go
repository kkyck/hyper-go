package main

// asticker.go — .asticker: audio sticker (stiker bersuara).
//
// Memakai field BARU dari proto WhatsApp terbaru (whatsmeow 2026-09-29):
//   StickerMessage.audioMessage — oneof "audio", field 26.
// Stiker dikirim sebagai StickerMessage biasa (webp) + AudioMessage di
// dalam oneof audio. Client WhatsApp terbaru me-render-nya sebagai
// stiker yang bisa mengeluarkan suara.
//
// Usage:
//   .asticker [teks] — reply ke audio/voice note.
//   Contoh: .asticker halo (reply VN) → stiker teks "halo" + suara VN.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// audioStickerMaxBytes — batas ukuran audio (stiker + audio jangan berat).
const audioStickerMaxBytes = 1 << 20 // 1MB

func handleAudioStickerCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(botClient, msg) {
		return "❌ Hanya owner yang bisa membuat sticker saat self mode"
	}

	text := strings.TrimSpace(args)
	if text == "help" {
		return "🔊 *Audio Sticker*\n\nMembuat audio sticker (stiker bersuara) — fitur baru WhatsApp.\n\n❓ *Cara pakai:* .asticker [teks] (reply audio/voice note)\nContoh: `.asticker halo`\nTanpa teks → stiker bertuliskan AUDIO"
	}

	quoted := getQuotedMessage(msg)
	fmt.Printf("[asticker] quoted=%v\n", quoted != nil && quoted.Message != nil)
	if quoted == nil || quoted.Message == nil {
		return "❌ Reply ke audio/voice note dulu\n❓ *Cara pakai:* .asticker halo (sambil reply VN)"
	}
	aud := quoted.Message.GetAudioMessage()
	fmt.Printf("[asticker] aud=%v mime=%q seconds=%v len=%d\n", aud != nil, aud.GetMimetype(), aud.GetSeconds(), len(aud.GetURL()))
	if aud == nil {
		return "❌ Yang di-reply harus audio/voice note"
	}

	// Download audio dari pesan yang di-reply
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	audioData, err := client.Download(ctx, aud)
	fmt.Printf("[asticker] download: len=%d err=%v\n", len(audioData), err)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download audio: %v", err)
	}
	if len(audioData) == 0 {
		return "❌ Audio kosong"
	}
	if len(audioData) > audioStickerMaxBytes {
		return fmt.Sprintf("❌ Audio terlalu besar (%s, max 1MB)", formatBytes(uint64(len(audioData))))
	}

	// Visual stiker: teks dari args, default "AUDIO"
	// (block font hanya ASCII — emoji seperti 🔊 jadi "?")
	if text == "" {
		text = "AUDIO"
	}
	webpBuf, err := createStickerFromText(text)
	fmt.Printf("[asticker] webp: len=%d err=%v\n", len(webpBuf), err)
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat stiker: %v", err)
	}

	// Upload webp (pakai jalur stiker biasa)
	stickerMsg, err := uploadSticker(client, webpBuf, false, "")
	fmt.Printf("[asticker] uploadSticker err=%v\n", err)
	if err != nil {
		return fmt.Sprintf("❌ Gagal upload stiker: %v", err)
	}

	// Upload audio
	upCtx, upCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer upCancel()
	up, err := client.Upload(upCtx, audioData, whatsmeow.MediaAudio)
	fmt.Printf("[asticker] uploadAudio: url=%v err=%v\n", up.URL != "", err)
	if err != nil {
		return fmt.Sprintf("❌ Gagal upload audio: %v", err)
	}
	mime := aud.GetMimetype()
	if mime == "" {
		mime = "audio/ogg; codecs=opus"
	}

	// ── Field BARU proto 2026-09-29: oneof audio di StickerMessage ──
	// Waveform placeholder (32 bar) — samakan bentuk AudioMessage resmi
	waveform := make([]byte, 32)
	for i := range waveform {
		waveform[i] = 50
	}
	stickerMsg.Audio = &waE2E.StickerMessage_AudioMessage{
		AudioMessage: &waE2E.AudioMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			FileSHA256:    up.FileSHA256,
			FileEncSHA256: up.FileEncSHA256,
			FileLength:    proto.Uint64(uint64(len(audioData))),
			Seconds:       aud.Seconds,
			PTT:           proto.Bool(false),
			Waveform:      waveform,
		},
	}

	out := &waE2E.Message{StickerMessage: stickerMsg}
	out = humanizeMessage(out)

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()
	resp, err := client.SendMessage(sendCtx, msg.Info.Chat, out)
	fmt.Printf("[asticker] send: id=%q ts=%v err=%v\n", resp.ID, resp.Timestamp, err)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "asticker",
		Aliases:     []string{"astiker"},
		Description: "asticker",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAudioStickerCommand(client, msg, args)
		},
	})
}
