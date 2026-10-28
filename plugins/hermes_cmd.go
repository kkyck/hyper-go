package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// handleHermesCommand menjalankan Hermes Agent (KangRouter) sebagai fitur bot.
// OWNER-ONLY: Hermes adalah agent yang bisa menjalankan tool/shell.
func handleHermesCommand(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa pakai .hermes"
	}
	prompt := strings.TrimSpace(args)
	if prompt == "" {
		return "❓ *Cara pakai:* .hermes <perintah/pertanyaan>\nContoh: .hermes buatkan script python hello world\n\nHermes = AI agent (bisa multi-step + pakai tools), model via KangRouter"
	}

	// Timeout 4 menit — agent bisa agak lama untuk task multi-step
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/home/hatch/.local/bin/hermes", "-z", prompt)
	cmd.Env = []string{
		"HERMES_HOME=/home/hatch/hermes",
		"HOME=/home/hatch",
		"PATH=/home/hatch/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	cmd.Dir = "/home/hatch/hermes"

	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		return "⏱️ Hermes timeout (4 menit) — coba perintah yang lebih sederhana"
	}
	if err != nil && result == "" {
		return fmt.Sprintf("❌ Hermes error: %v", err)
	}

	// Batasi panjang untuk WhatsApp (~4000 char)
	if len(result) > 3500 {
		result = result[:3500] + "\n\n…(dipotong)"
	}
	if result == "" {
		return "🤖 Hermes tidak memberikan output"
	}
	return "🤖 *Hermes*\n\n" + result
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "hermes",
		Description: "hermes",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleHermesCommand(args, msg)
			// ─── Notify (owner-only, port Baileys NIXCODE) ────────────
		},
	})
}
