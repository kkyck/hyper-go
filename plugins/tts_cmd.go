package main

import (
	"fmt"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

func init() {
	RegisterPlugin(Plugin{
		Command:     "tts",
		Aliases:     []string{"suara"},
		Description: "Text-to-Speech gratis (Edge Neural → Google fallback)",
		Category:    "AI",
		Handler:     handleTTSCommand,
	})
}

func handleTTSCommand(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
	if args == "" {
		return "❓ *Cara pakai:* .tts <teks>\nContoh: .tts halo apa kabar"
	}
	if len(args) > 500 {
		return "❌ Teks maksimal 500 karakter."
	}

	audio, provider, err := ttsSynthesize(args)
	if err != nil {
		return fmt.Sprintf("❌ TTS gagal: %v", err)
	}

	// Kirim sebagai voice note pakai helper yang ada
	err = sendAudioMessage(client, msg.Info.Chat, audio, "tts.mp3", msg)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim audio: %v", err)
	}

	fmt.Printf("[TTS] sent via %s: %d bytes\n", provider, len(audio))
	return "" // silent sukses
}
