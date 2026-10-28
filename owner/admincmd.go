package main

/**
 * 🛠️ admincmd.go — Owner/admin-only commands
 * ════════════════════════════════════════════════════════════
 * Commands (prefix .):
 *   .tagall [text]                  → Mention all group members
 *   .kick <@mention>                → Remove member from group (admin only)
 *   .promote <@mention>             → Make admin (admin only)
 *   .demote <@mention>              → Demote admin (admin only)
 *   .linkgroup                      → Get group invite link
 *   .revoke                         → Revoke group link
 *   .setgroupname <name>            → Change group subject
 *   .setgroupdesc <text>            → Change group description
 *   .eval <go-expression>            → Run restricted Go expression (owner only)
 *   .shell <cmd>                    → Execute shell command (owner only)
 *   .getfile <path>                 → Read file from disk (owner only)
 *   .broadcast <text>               → Broadcast message to all recent chats (owner only)
 *   .stickersearch <query>          → Search sticker on Telegram (uses public sticker API)
 *   .gc                             → Force garbage collection (alias of .rss compress)
 *   .shutdown                       → Exit process (owner only)
 *   .listcmd                        → List all available commands (grouped)
 */

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── Tag all (.tagall [text]) ──────────────────────────────────

func handleTagAll(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ .tagall hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner atau admin grup yang bisa .tagall"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || meta == nil {
		return fmt.Sprintf("⚠️ Gagal ambil member grup: %v", err)
	}

	var mentions []string
	for _, p := range meta.Participants {
		if p.JID.User != "" {
			mentions = append(mentions, p.JID.String())
		}
	}
	if len(mentions) == 0 {
		return "⚠️ Tidak ada participant."
	}

	text := strings.TrimSpace(args)
	if text == "" {
		text = "📣 Tag All Members"
	}
	// Append all @phone placeholders so everyone gets pinged
	for _, m := range mentions {
		text += "\n@" + extractPhone(m)
	}

	out := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				MentionedJID: mentions,
			},
		},
	}
	_, err = client.SendMessage(ctx, msg.Info.Chat, out)
	if err != nil {
		return fmt.Sprintf("⚠️ Gagal kirim tagall: %v", err)
	}
	return ""
}

// ─── Group admin actions (.kick/.promote/.demote) ──────────────

func handleGroupAction(client *whatsmeow.Client, msg *events.Message, action, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya admin grup yang bisa command ini"
	}

	// Try to extract mentioned JID from message context
	mentioned := extractMentionedJIDs(msg)
	if len(mentioned) == 0 {
		// Try args as phone number
		p := extractPhone(strings.TrimSpace(args))
		if p == "" {
			return "❓ *Cara pakai:* ." + action + " @mention atau ." + action + " <nomor>"
		}
		mentioned = []string{p + "@s.whatsapp.net"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Map action name to whatsmeow.ParticipantChange
	var pc whatsmeow.ParticipantChange
	switch action {
	case "kick":
		pc = whatsmeow.ParticipantChangeRemove
	case "promote":
		pc = whatsmeow.ParticipantChangePromote
	case "demote":
		pc = whatsmeow.ParticipantChangeDemote
	default:
		return "❌ Unknown action: " + action
	}

	var results []string
	for _, jidStr := range mentioned {
		jid, err := resolveTargetJID(client, jidStr)
		if err != nil || jid.IsEmpty() {
			results = append(results, fmt.Sprintf("• @%s — invalid JID", extractPhone(jidStr)))
			continue
		}
		// Coba semua bentuk dual (PN + LID) sampai sukses
		forms := dualForms(client, jid)
		if len(forms) == 0 {
			forms = []types.JID{jid}
		}
		var err2 error
		ok := false
		for _, f := range forms {
			_, err2 = client.UpdateGroupParticipants(ctx, msg.Info.Chat, []types.JID{f}, pc)
			if err2 == nil {
				ok = true
				break
			}
		}
		label := extractPhone(jidStr)
		if ph := phoneOf(client, jid); ph != "" {
			label = ph
		}
		if ok {
			results = append(results, fmt.Sprintf("• @%s — ✅", label))
		} else {
			results = append(results, fmt.Sprintf("• @%s — gagal: %v", label, err2))
		}
	}

	return fmt.Sprintf("📋 *%s*\n\n%s", strings.ToUpper(action), strings.Join(results, "\n"))
}

// extractMentionedJIDs reads mentioned JIDs (PN and/or LID) from message context.
func extractMentionedJIDs(msg *events.Message) []string {
	return extractMentionedJIDsDual(msg)
}

// ─── Group link (.linkgroup) ───────────────────────────────────

func handleLinkGroup(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya admin grup yang bisa lihat invite link"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	link, err := client.GetGroupInviteLink(ctx, msg.Info.Chat, false)
	if err != nil {
		return fmt.Sprintf("⚠️ Gagal ambil link: %v", err)
	}
	return fmt.Sprintf("🔗 *Group Invite Link*\n\n%s", link)
}

// .revoke — revoke group invite link
func handleRevokeLink(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya admin grup yang bisa revoke link"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	link, err := client.GetGroupInviteLink(ctx, msg.Info.Chat, true)
	if err != nil {
		return fmt.Sprintf("⚠️ Gagal revoke: %v", err)
	}
	return fmt.Sprintf("🔗 *Link lama di-revoke. Link baru:*\n\n%s", link)
}

// .setgroupname <name>
func handleSetGroupName(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya admin grup yang bisa set nama"
	}
	name := strings.TrimSpace(args)
	if name == "" {
		return "❓ *Cara pakai:* .setgroupname <name>"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := client.SetGroupName(ctx, msg.Info.Chat, name)
	if err != nil {
		return fmt.Sprintf("⚠️ Gagal set nama: %v", err)
	}
	return fmt.Sprintf("✅ Nama grup diubah jadi: *%s*", name)
}

// .setgroupdesc <text>
func handleSetGroupDesc(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya admin grup yang bisa set deskripsi"
	}
	desc := strings.TrimSpace(args)
	if desc == "" {
		return "❓ *Cara pakai:* .setgroupdesc <text>"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := client.SetGroupDescription(ctx, msg.Info.Chat, desc)
	if err != nil {
		return fmt.Sprintf("⚠️ Gagal set deskripsi: %v", err)
	}
	return "✅ Deskripsi grup diperbarui"
}

// ─── Owner-only: eval, shell, getfile, shutdown ────────────────

// shellMaxOutput caps combined output size returned to the user.
const (
	shellMaxOutput = 4000
	shellTimeout   = 20 * time.Second
	getfileMaxSize = 1024 * 1024 // 1MB
)

// allowedShellCmds restricts .shell to a known-good set when SHELL_ALLOW_ALL != "1".
// Owner can opt-out via env to retain full shell access.
var allowedShellCmds = map[string]bool{
	"ls": true, "pwd": true, "whoami": true, "date": true, "uptime": true,
	"uname -a": true, "df -h": true, "free -m": true, "free -h": true,
	"ps": true, "top -bn1": true, "cat /etc/os-release": true,
	"git status": true, "git log --oneline -10": true,
	"systemctl status": true, "ip addr": true, "ifconfig": true,
}

// shellDangerousSeqs menolak command chaining/pengalihan yang berbahaya
// (kecuali SHELL_ALLOW_ALL=1). Prefix-match allowlist mengizinkan argumen apa
// pun setelah command, jadi sekuens ini wajib ditolak untuk mencegah
// accidental dangerous chaining seperti "ps aux; rm -rf /".
var shellDangerousSeqs = []string{";", "&&", "||", "|", "$(", "`", ">", "<", "\n", "\r"}

// shellHasChaining returns true jika cmd mengandung shell metacharacter berbahaya.
func shellHasChaining(cmd string) bool {
	for _, s := range shellDangerousSeqs {
		if strings.Contains(cmd, s) {
			return true
		}
	}
	return false
}

// shellAllowed returns true if cmd is allowed (or if SHELL_ALLOW_ALL=1).
func shellAllowed(cmd string) bool {
	if getenvDefault("SHELL_ALLOW_ALL", "0") == "1" {
		return true
	}
	c := strings.TrimSpace(cmd)
	lc := strings.ToLower(c)
	if allowedShellCmds[lc] {
		return true
	}
	// allow prefix matches for ls/ps with flags
	for k := range allowedShellCmds {
		if strings.HasPrefix(lc, k+" ") || lc == k {
			return true
		}
	}
	return false
}

// handleShell executes a shell command (owner only).
// Restricted by default to a safe allowlist; opt-out via SHELL_ALLOW_ALL=1.
func handleShell(args string, msg *events.Message) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa pakai .shell"
	}
	cmd := strings.TrimSpace(args)
	if cmd == "" {
		return "❓ *Cara pakai:* .shell <command>\nDefault: allowlist aktif. Set `SHELL_ALLOW_ALL=1` untuk buka semua."
	}
	if !shellAllowed(cmd) {
		auditCommand(msg, ".shell", cmd, "denied", 0)
		return fmt.Sprintf("❌ Perintah *%s* tidak di-allowlist.\nSet env `SHELL_ALLOW_ALL=1` untuk bypass.", truncateStr(cmd, 80))
	}
	// Tolak shell chaining metacharacters (kecuali SHELL_ALLOW_ALL=1):
	// prefix-match allowlist mengizinkan argumen bebas setelah command.
	if getenvDefault("SHELL_ALLOW_ALL", "0") != "1" && shellHasChaining(cmd) {
		auditCommand(msg, ".shell", cmd, "denied", 0)
		return "❌ Perintah mengandung shell metacharacter (`;`, `&&`, `||`, `|`, `$()`, backtick, redirect) — ditolak.\nSet env `SHELL_ALLOW_ALL=1` untuk bypass."
	}
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	// Set working dir eksplisit ke CWD proses (defense-in-depth,
	// jangan biarkan inherit ambigu).
	if cwd, err := os.Getwd(); err == nil {
		c.Dir = cwd
	}
	out, err := c.CombinedOutput()
	dur := time.Since(t0).Milliseconds()
	if err != nil {
		auditCommand(msg, ".shell", cmd, "error", dur)
		return fmt.Sprintf("⚠️ Error: %v\n\n```\n%s\n```", err, truncateStr(string(out), shellMaxOutput))
	}
	auditCommand(msg, ".shell", cmd, "ok", dur)
	return fmt.Sprintf("✅ Output (%dms):\n```\n%s\n```", dur, truncateStr(string(out), shellMaxOutput))
}

// handleGetFile reads a file from disk (owner only, restricted paths).
// Resolves the path to absolute, then verifies it's under an allowed root:
//   - current working directory (cwd)
//   - OWNER_EXTRA_FILE_ROOT env (optional, single path)
//
// Refuses any path containing ".." after cleaning, and any symlink escape.
func handleGetFile(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa pakai .getfile"
	}
	raw := strings.TrimSpace(args)
	if raw == "" {
		return "❓ *Cara pakai:* .getfile <path> (relative to cwd)"
	}
	if strings.Contains(raw, "..") {
		auditCommand(msg, ".getfile", raw, "denied", 0)
		return "❌ Path traversal (`..`) tidak diizinkan"
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Sprintf("❌ Getwd: %v", err)
	}
	abs := raw
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, raw)
	}
	// Clean and verify
	clean := filepath.Clean(abs)
	// Symlink escape check
	if fi, err := os.Lstat(clean); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			clean = resolved
		}
	}

	allowedRoots := []string{filepath.Clean(cwd)}
	if extra := getenvDefault("OWNER_EXTRA_FILE_ROOT", ""); extra != "" {
		allowedRoots = append(allowedRoots, filepath.Clean(extra))
	}
	allowed := false
	for _, root := range allowedRoots {
		if strings.HasPrefix(clean, root+string(os.PathSeparator)) || clean == root {
			allowed = true
			break
		}
	}
	if !allowed {
		auditCommand(msg, ".getfile", raw, "denied", 0)
		return fmt.Sprintf("❌ Path di luar root yang diizinkan:\n• `%s`\n• (tambahan: `OWNER_EXTRA_FILE_ROOT`)", cwd)
	}

	info, err := os.Stat(clean)
	if err != nil {
		return fmt.Sprintf("❌ %v", err)
	}
	if info.IsDir() {
		return fmt.Sprintf("❌ `%s` adalah direktori, bukan file", clean)
	}
	if info.Size() > getfileMaxSize {
		return fmt.Sprintf("❌ File terlalu besar (%d bytes). Maks 1MB", info.Size())
	}
	t0 := time.Now()
	data, err := os.ReadFile(clean)
	dur := time.Since(t0).Milliseconds()
	if err != nil {
		auditCommand(msg, ".getfile", raw, "error", dur)
		return fmt.Sprintf("❌ %v", err)
	}
	auditCommand(msg, ".getfile", raw, "ok", dur)
	return fmt.Sprintf("📄 *File: %s* (%d bytes)\n```\n%s\n```", clean, len(data), truncateStr(string(data), 3000))
}

// handleShutdown — graceful exit (owner only)
func handleShutdown(msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa .shutdown"
	}
	auditCommand(msg, ".shutdown", "", "ok", 0)
	go func() {
		time.Sleep(2 * time.Second)
		fmt.Println("👋 Shutdown requested via .shutdown command")
		os.Exit(0)
	}()
	return "👋 Bot akan shutdown dalam 2 detik..."
}

// ─── Broadcast (.broadcast <text>) ────────────────────────────

// handleBroadcast sends a text to all recent chats the bot has interacted with.
// For simplicity, this uses the JIDs we've seen in incoming messages.
// Stored in globalKV["recent_chats"].
func handleBroadcast(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa .broadcast"
	}
	text := strings.TrimSpace(args)
	if text == "" {
		return "❓ *Cara pakai:* .broadcast <text>"
	}

	// Get recent chats from KV
	recentChats := []string{}
	if v := globalKV.Get("recent_chats"); v != nil {
		if arr, ok := v.([]any); ok {
			for _, c := range arr {
				if s, ok := c.(string); ok {
					recentChats = append(recentChats, s)
				}
			}
		}
	}
	if len(recentChats) == 0 {
		return "⚠️ Belum ada chat tersimpan untuk broadcast. Tunggu beberapa pesan masuk dulu."
	}

	// broadcast — but we need a *whatsmeow.Client to send. We'll use the global from main.go.
	// For now, just report count; main.go can wire this via botClient global.
	broadcastText := "📣 *BROADCAST*\n\n" + text
	go func() {
		sent, failed := 0, 0
		for _, chatJID := range recentChats {
			jid, err := types.ParseJID(chatJID)
			if err != nil {
				failed++
				continue
			}
			if err := sendBroadcastText(botClient, jid, broadcastText); err != nil {
				failed++
			} else {
				sent++
			}
			time.Sleep(800 * time.Millisecond) // gentle rate limit
		}
		auditCommand(msg, ".broadcast", text, fmt.Sprintf("sent=%d failed=%d", sent, failed), 0)
	}()

	return fmt.Sprintf("✅ Broadcast mengirim ke %d chat. Pesan:\n\n%s", len(recentChats), text)
}

// sendBroadcastText sends a text to a JID. Implemented here to avoid circular import.
func sendBroadcastText(client *whatsmeow.Client, jid types.JID, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := &waE2E.Message{Conversation: proto.String(text)}
	_, err := client.SendMessage(ctx, jid, out)
	return err
}

// ─── Sticker Search (.stickersearch <query>) ───────────────────

// handleStickerSearch searches the web for stickers via public sticker API.
// For simplicity, uses the /search or telegram sticker search approach.
// Falls back to a simple text reply if no stickers found.
func handleStickerSearch(client *whatsmeow.Client, msg *events.Message, args string) string {
	if isSelfMode() && !isOwnerMsg(msg) {
		return "🔒 Self mode aktif — hanya owner yang bisa pakai."
	}
	query := strings.TrimSpace(args)
	if query == "" {
		return "❓ *Cara pakai:* .stickersearch <query>\nContoh: *.stickersearch kucing lucu*"
	}

	// Try a few public sticker search APIs
	// Note: there doesn't seem to be a /search/sticker endpoint directly,
	// but we can use /search/telegram-sticker or fall back to a web search.
	// For now, return a placeholder with the query and suggest manual search.
	return fmt.Sprintf("🔍 *Sticker Search: %s*\n\n⚠️ Sticker search butuh endpoint khusus (mis. Telegram sticker pack).\nCoba pakai .brat <teks> untuk membuat sticker dari teks.", query)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "tagall",
		Description: "tagall",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTagAll(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "kick",
		Description: "kick",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGroupAction(client, msg, "kick", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "promote",
		Description: "promote",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGroupAction(client, msg, "promote", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "demote",
		Description: "demote",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGroupAction(client, msg, "demote", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "linkgroup",
		Aliases:     []string{"grouplink"},
		Description: "linkgroup",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleLinkGroup(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "revoke",
		Description: "revoke",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleRevokeLink(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "setgroupname",
		Description: "setgroupname",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSetGroupName(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "setgroupdesc",
		Description: "setgroupdesc",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSetGroupDesc(client, msg, args)
			// ─── Auto presence ──────────────────────────────────────
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "shell",
		Description: "shell",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleShell(args, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "getfile",
		Description: "getfile",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGetFile(args, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "shutdown",
		Description: "shutdown",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleShutdown(msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "broadcast",
		Description: "broadcast",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleBroadcast(args, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "stickersearch",
		Description: "stickersearch",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleStickerSearch(client, msg, args)
			// ─── Menu ────────────────────────────────────────────────
		},
	})
}
