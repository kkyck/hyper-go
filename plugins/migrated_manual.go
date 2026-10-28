// plugins/migrated_manual.go — 12 command yang dimigrasi manual
// (logic kompleks/inline, tidak bisa auto-migrasi).
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

func init() {
	// .self — pre-gate (harus selalu respon)
	RegisterPlugin(Plugin{
		Command:     "self",
		PreGate:     true,
		Description: "Self mode & SPR",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			if strings.HasPrefix(strings.ToLower(args), "spr") {
				sprArgs := strings.TrimSpace(args[len("spr"):])
				return handleSelfSprCommand(client, msg, sprArgs)
			}
			if strings.HasPrefix(strings.ToLower(args), "autodel") {
				adArgs := strings.TrimSpace(args[len("autodel"):])
				return handleAutodelCommand(msg, adArgs)
			}
			return handleSelfCommand(args, msg)
		},
	})

	// .restart — owner only
	RegisterPlugin(Plugin{
		Command:     "restart",
		OwnerOnly:   true,
		Description: "Restart bot",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			go func() {
				time.Sleep(2 * time.Second)
				fmt.Println("🔄 Restart requested via .restart command")
				os.Exit(0)
			}()
			return "🔄 Bot akan restart dalam 2 detik..."
		},
	})

	// .version
	RegisterPlugin(Plugin{
		Command:     "version",
		Aliases:     []string{"ver"},
		Description: "Versi bot",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return "🤖 *Bot MD v2.86*\n\n• GitHub: Yz776/bot-whatsapp-golang\n• Branch: main\n• Commit: latest\n\n✨ v2.86: Auto-detect Go in ~/.local/go/bin (install via .shell bash install_go.sh)"
		},
	})

	// .halo
	RegisterPlugin(Plugin{
		Command:     "halo",
		Aliases:     []string{"hai", "hello", "hi"},
		Description: "Sapa bot",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			pushName := msg.Info.PushName
			return fmt.Sprintf("👋 Halo, *%s*! Senang bertemu denganmu.\nKetik *.menu* untuk melihat daftar perintah.", pushName)
		},
	})

	// .info
	RegisterPlugin(Plugin{
		Command:     "info",
		Description: "Info pesan",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			chatType := "Personal"
			if msg.Info.IsGroup {
				chatType = "Grup"
			}
			wib := time.FixedZone("WIB", 7*3600)
			t := msg.Info.Timestamp.In(wib).Format("02-01-2006 15:04:05")
			pushName := msg.Info.PushName
			return fmt.Sprintf("📋 *Info Pesan*\n\n• Nama: %s\n• Nomor: %s\n• Jenis: %s\n• Waktu: %s WIB",
				pushName, msg.Info.Sender.User, chatType, t)
		},
	})

	// .waktu
	RegisterPlugin(Plugin{
		Command:     "waktu",
		Description: "Waktu server",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			wib := time.FixedZone("WIB", 7*3600)
			now := time.Now().In(wib).Format("02-01-2006 15:04:05")
			return fmt.Sprintf("🕒 *Waktu Server*\n\n%s WIB", now)
		},
	})

	// .echo
	RegisterPlugin(Plugin{
		Command:     "echo",
		Description: "Ulangi teks",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			if args == "" {
				return "❓ *Cara pakai:* .echo <teks>\nContoh: .echo halo dunia"
			}
			return "🔁 " + args
		},
	})

	// .owner
	RegisterPlugin(Plugin{
		Command:     "owner",
		Description: "Kontak owner",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleOwnerListCommand(client, msg, args)
		},
	})

	// .rvo
	RegisterPlugin(Plugin{
		Command:     "rvo",
		Aliases:     []string{"readviewonce", "readvo"},
		Description: "Baca view-once",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleReadViewOnceCommand(client, msg)
		},
	})

	// .s/.sticker — dipindah ke plugin sticker terpisah (port tools-sticker.js)

	// .aistatus — owner only
	RegisterPlugin(Plugin{
		Command:     "aistatus",
		OwnerOnly:   true,
		Description: "Status AI backend",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			var sb strings.Builder
			sb.WriteString("🤖 *AI Backend Status (v2.86)*\n\n")
			sb.WriteString("• Gemini: " + geminiCBStatus() + "\n")
			sb.WriteString("• Pollinations: ready (no key)\n")
			sb.WriteString("• Chain: Gemini → Pollinations\n")
			sb.WriteString("• Active sessions: " + fmt.Sprintf("%d", geminiFreeSessionCount()) + "\n")
			sb.WriteString("• Compression: " + compressionStats() + "\n")
			return sb.String()
		},
	})
}
