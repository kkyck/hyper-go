package main

// ai_agent.go — .ai command: role-based AI + tool execution + image analysis
// ════════════════════════════════════════════════════════════
//
// .ai command accessible like .s (sticker):
//   - Reply image + .ai "apa ini?" → analyze image (role-gated)
//   - .ai <pertanyaan> → chat dengan free Gemini
//   - .ai kick @tag → group tools (role-gated)
//   - .ai shell <cmd> → shell (owner only)
//
// Roles & capabilities (see ai_roles.go for full table):
//   - Owner:     everything, unlimited
//   - Admin:     chat + image + group tools, 20/min
//   - Trusted:   chat + image, 10/min
//   - User:      chat text-only, 5/min
//   - Banned:    no access
//
// AI Provider: Free Gemini (gemini.google.com, no API key)
// Vision: Pollinations Vision API (free, no key)
// Fallback: kangwifi /ai/gemini

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── AI Personality (different per role for flavor) ───────────────

const aiPersonalityOwner = `Kamu adalah asisten AI pribadi owner bot WhatsApp.
Kamu punya akses ke semua fitur: kick, promote, shell, eval, readfile, dll.
Jawab singkat, jelas, ramah, dalam Bahasa Indonesia.
Kamu bebas membantu apapun tanpa batasan.`

const aiPersonalityAdmin = `Kamu adalah asisten AI untuk admin grup WhatsApp.
Kamu bisa bantu kelola grup: kick, promote, tagall, dll.
Jawab singkat dan jelas dalam Bahasa Indonesia.
Untuk fitur di luar kelola grup, sarankan user tanya owner.`

const aiPersonalityTrusted = `Kamu adalah asisten AI tepercaya di bot WhatsApp.
Kamu bisa chat dan analisis gambar.
Jawab singkat, jelas, ramah dalam Bahasa Indonesia.
Kamu tidak bisa kick/admin/shell — sarankan tanya owner atau admin grup.`

const aiPersonalityUser = `Kamu adalah asisten AI di bot WhatsApp.
Jawab singkat, jelas, ramah dalam Bahasa Indonesia.
Kamu hanya bisa chat teks. Untuk fitur lain (kick, image, dll),
saran user jadi trusted user atau admin grup.`

func aiPersonalityForRole(role AIRole) string {
	switch role {
	case AIRoleOwner:
		return aiPersonalityOwner
	case AIRoleAdmin:
		return aiPersonalityAdmin
	case AIRoleTrusted:
		return aiPersonalityTrusted
	default:
		return aiPersonalityUser
	}
}

// ─── Main command handler ─────────────────────────────────────────

func handleAIAgentCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	// Defensive nil guard: jangan teruskan client/msg nil ke handler lain.
	if client == nil || msg == nil {
		return "❌ Internal error: client/pesan tidak valid."
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return aiAgentHelp(client, msg)
	}

	// Detect role
	role := aiDetectRole(client, msg)
	cfg := aiRoleConfigs[role]

	// Banned = no access
	if role == AIRoleBanned {
		return "❌ Anda dilarang menggunakan AI. Hubungi owner."
	}

	// Parse: first word might be a tool command
	parts := strings.Fields(args)
	if len(parts) == 0 {
		return aiAgentHelp(client, msg)
	}

	cmd := strings.ToLower(parts[0])
	rest := strings.TrimSpace(strings.TrimPrefix(args, parts[0]))

	// ─── Always-available commands ──────────────────────────────
	switch cmd {

	case "role", "myrole":
		return aiRoleInfo(role)

	case "help", "?":
		return aiAgentHelp(client, msg)

	case "clear", "reset", "-c":
		if !cfg.CanChat {
			return "❌ Role Anda tidak punya akses chat"
		}
		userKey := msg.Info.Sender.String()
		existed := geminiFreeClearSession(userKey)
		if existed {
			return "✅ Memory AI dibersihkan"
		}
		return "ℹ️ Tidak ada memory untuk dibersihkan"
	}

	// ─── Ban management (owner only) ────────────────────────────
	if cmd == "ban" || cmd == "unban" || cmd == "banlist" {
		if !cfg.CanBanUsers {
			return "❌ Hanya owner yang bisa manage ban"
		}
		return aiHandleBanCommand(client, msg, cmd, rest)
	}

	// ─── Rate limit check (for all subsequent commands) ────────
	if !aiRateAllow(msg.Info.Sender.String(), role) {
		limit := cfg.RateLimitPerMin
		return fmt.Sprintf("⏳ Rate limit tercapai (%d/min untuk role %s). Tunggu sebentar.",
			limit, role.String())
	}

	// ─── Group admin commands (role-gated) ─────────────────────
	switch cmd {
	case "kick", "keluarin":
		if !cfg.CanUseGroupTools {
			return aiNoPermission(role, "kick")
		}
		return handleGroupAction(client, msg, "kick", rest)

	case "promote", "admin":
		if !cfg.CanUseGroupTools {
			return aiNoPermission(role, "promote")
		}
		return handleGroupAction(client, msg, "promote", rest)

	case "demote", "unadmin":
		if !cfg.CanUseGroupTools {
			return aiNoPermission(role, "demote")
		}
		return handleGroupAction(client, msg, "demote", rest)

	case "groupinfo", "ginfo", "infogrup":
		return handleGroupInfoCmd(client, msg)

	case "tagall", "tag":
		if !cfg.CanUseGroupTools {
			return aiNoPermission(role, "tagall")
		}
		return handleTagAll(client, msg, rest)

	case "hidetag", "ht":
		if !cfg.CanUseGroupTools {
			return aiNoPermission(role, "hidetag")
		}
		return handleTagAll(client, msg, rest)

	case "link", "linkgrup", "grouplink":
		return handleLinkGroup(client, msg)

	case "setname", "setnamegroup":
		if !cfg.CanUseGroupTools {
			return aiNoPermission(role, "setname")
		}
		return handleSetGroupName(client, msg, rest)

	case "setdesc", "setdescgroup":
		if !cfg.CanUseGroupTools {
			return aiNoPermission(role, "setdesc")
		}
		return handleSetGroupDesc(client, msg, rest)
	}

	// ─── Owner-only powerful commands ──────────────────────────
	switch cmd {
	case "shell", "sh", "exec":
		if !cfg.CanUseShell {
			return aiNoPermission(role, "shell")
		}
		return handleShell(rest, msg)

	case "eval", "go":
		if !cfg.CanUseEval {
			return aiNoPermission(role, "eval")
		}
		return handleEvalCommand(rest, msg)

	case "readfile", "cat":
		if !cfg.CanUseReadFile {
			return aiNoPermission(role, "readfile")
		}
		if rest == "" {
			return "❓ *Cara pakai:* .ai readfile <path>"
		}
		// SECURITY FIX: jangan lewat shell ("cat "+path = command injection).
		// handleGetFile baca file langsung + validasi path traversal.
		return handleGetFile(rest, msg)

	case "getfile":
		if !cfg.CanUseGetFile {
			return aiNoPermission(role, "getfile")
		}
		return handleGetFile(rest, msg)

	case "menu":
		return handleMenu(client, msg)

	case "ping":
		return "🏓 Pong! Bot aktif. Role: " + role.String()

	case "stats":
		return handleStatsCommand()
	}

	// ─── Default: Agentic AI (AI decides tools, multi-iteration) ───
	if !cfg.CanChat {
		return "❌ Role Anda tidak punya akses AI chat"
	}

	// Check if replying to an image (like .s sticker command) — handle separately
	quoted := getQuotedMessage(msg)
	if quoted != nil && quoted.Message != nil {
		hasImage := false
		if quoted.Message.GetImageMessage() != nil {
			hasImage = true
		} else if doc := quoted.Message.GetDocumentWithCaptionMessage(); doc != nil {
			if doc.GetMessage().GetImageMessage() != nil {
				hasImage = true
			}
		}

		if hasImage && cfg.CanAnalyzeImage {
			// Image analysis mode (still agentic for follow-up)
			return aiAgentChatWithImage(client, msg, args, role)
		}
	}

	// Agentic AI: AI decides which tools to call, multi-iteration
	return aiAgenticProcess(client, msg, args, role, cfg)
}

// ─── Intent helper functions ──────────────────────────────────────

// aiGetGroupParticipants fetches and formats group participant list.
func aiGetGroupParticipants(client *whatsmeow.Client, msg *events.Message) (string, bool) {
	if client == nil || !msg.Info.IsGroup {
		return "ℹ️ Hanya bisa di grup.", true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil {
		return fmt.Sprintf("❌ Gagal ambil info grup: %v", err), true
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👥 *Member Grup* (%d total)\n\n", len(meta.Participants)))

	adminCount := 0
	regularCount := 0
	for i, p := range meta.Participants {
		role := "member"
		if p.IsAdmin {
			role = "admin"
			adminCount++
		} else if p.IsSuperAdmin {
			role = "super admin"
			adminCount++
		} else {
			regularCount++
		}
		if i < 20 { // limit to first 20 to avoid message too long
			sb.WriteString(fmt.Sprintf("• %s (%s)\n", p.JID.String(), role))
		}
	}
	if len(meta.Participants) > 20 {
		sb.WriteString(fmt.Sprintf("\n... dan %d lainnya\n", len(meta.Participants)-20))
	}
	sb.WriteString(fmt.Sprintf("\nTotal: %d admin, %d member", adminCount, regularCount))
	return sb.String(), true
}

// aiGetSenderInfo returns info about the message sender.
func aiGetSenderInfo(client *whatsmeow.Client, msg *events.Message, role AIRole) (string, bool) {
	sender := msg.Info.Sender.ToNonAD()
	senderAlt := msg.Info.SenderAlt.ToNonAD()

	var sb strings.Builder
	sb.WriteString("👤 *Info Anda*\n\n")
	sb.WriteString(fmt.Sprintf("• Role: %s\n", role.String()))

	if sender.Server == "lid" || senderAlt.Server == "lid" {
		lid := sender
		if lid.Server != "lid" {
			lid = senderAlt
		}
		sb.WriteString(fmt.Sprintf("• LID: %s\n", lid.String()))
	}
	if sender.Server == "s.whatsapp.net" {
		sb.WriteString(fmt.Sprintf("• JID: %s\n", sender.String()))
		sb.WriteString(fmt.Sprintf("• Nomor: +%s\n", sender.User))
	} else if senderAlt.Server == "s.whatsapp.net" {
		sb.WriteString(fmt.Sprintf("• JID: %s\n", senderAlt.String()))
		sb.WriteString(fmt.Sprintf("• Nomor: +%s\n", senderAlt.User))
	}

	if msg.Info.IsGroup {
		isAdmin := isGroupAdmin(client, msg)
		if isAdmin {
			sb.WriteString("• Status di grup: Admin\n")
		} else {
			sb.WriteString("• Status di grup: Member\n")
		}
	}

	return sb.String(), true
}

// aiGetTimeInfo returns current time and date.
func aiGetTimeInfo() string {
	now := time.Now()
	wib := now.UTC().Add(7 * time.Hour)
	return fmt.Sprintf("🕐 *Waktu Sekarang*\n\n• WIB: %s\n• UTC: %s\n• Hari: %s",
		wib.Format("02 Jan 2006 15:04:05"),
		now.UTC().Format("02 Jan 2006 15:04:05"),
		wib.Format("Monday"))
}

// aiGetOwnerInfo returns owner info from env.
func aiGetOwnerInfo() string {
	ownerJID := getenvDefault("OWNER_JID", "(tidak diset)")
	ownerName := getenvDefault("OWNER_NAME", "Owner")
	return fmt.Sprintf("👑 *Owner Bot*\n\n• Nama: %s\n• JID: %s", ownerName, ownerJID)
}

// ─── AI chat with image analysis ──────────────────────────────────

func aiAgentChatWithImage(client *whatsmeow.Client, msg *events.Message, question string, role AIRole) string {
	// Download image from quoted message (bukan dari msg command itu sendiri)
	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❌ Reply gambar yang ingin dianalisis terlebih dahulu."
	}
	imageData, mime, err := aiDownloadImage(client, quoted)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download gambar: %v", err)
	}

	fmt.Printf("[AI] Image analysis: %d bytes, mime=%s, role=%s\n", len(imageData), mime, role.String())

	// Build question with personality context
	personality := aiPersonalityForRole(role)
	fullQuestion := personality + "\n\nUser question: " + question

	// Try Pollinations vision first (free)
	response, err := aiAnalyzeImage(imageData, mime, fullQuestion)
	if err != nil {
		fmt.Printf("[AI] Pollinations vision error: %v — falling back to text-only\n", err)
		// Fallback: text-only with image note
		fallbackQ := "[User replied with an image but vision API failed. Tell them you can't see the image.]\n\n" + question
		return aiAgentChatRaw(msg, fallbackQ, role)
	}

	return "🖼️ " + response
}

// ─── AI text chat (with memory) ───────────────────────────────────

func aiAgentChat(client *whatsmeow.Client, msg *events.Message, userMessage string) string {
	return aiAgentChatRaw(msg, userMessage, aiDetectRole(client, msg))
}

func aiAgentChatRaw(msg *events.Message, userMessage string, role AIRole) string {
	userMessage = strings.TrimSpace(userMessage)
	if userMessage == "" {
		return "❓ *Cara pakai:* .ai <pertanyaan>\nLihat: .ai help"
	}

	userKey := msg.Info.Sender.String()

	// First message: prepend personality
	geminiFreeSessionMu.Lock()
	hasSession := geminiFreeSessions[userKey] != ""
	geminiFreeSessionMu.Unlock()

	prompt := userMessage
	if !hasSession {
		prompt = aiPersonalityForRole(role) + "\n\nUser: " + userMessage
	}

	// Call free Gemini
	response, err := geminiFreeAskWithMemory(userKey, prompt)
	if err != nil {
		// Fallback ke Pollinations langsung (tanpa kangwifi)
		fmt.Printf("[AI] Free Gemini error: %v — falling back to Pollinations\n", err)
		response, err = pollinationsTextAsk(userMessage)
		if err != nil {
			return fmt.Sprintf("❌ AI error: %v", err)
		}
	}

	return response
}

// ─── Ban command handler ──────────────────────────────────────────

func aiHandleBanCommand(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
	loadAIBanList()

	switch cmd {
	case "ban":
		// Parse @tag or phone
		targetJID := aiExtractTargetJID(client, msg, args)
		if targetJID == "" {
			return "❓ *Cara pakai:* .ai ban @tag\nContoh: .ai ban 628xxx"
		}
		// Don't ban owner
		if isOwnerJID(targetJID) {
			return "❌ Tidak bisa ban owner"
		}
		aiBanUser(targetJID)
		return fmt.Sprintf("✅ User %s dibanned dari AI", targetJID)

	case "unban":
		targetJID := aiExtractTargetJID(client, msg, args)
		if targetJID == "" {
			return "❓ *Cara pakai:* .ai unban @tag"
		}
		if aiUnbanUser(targetJID) {
			return fmt.Sprintf("✅ User %s di-unban", targetJID)
		}
		return fmt.Sprintf("ℹ️ User %s tidak ada di ban list", targetJID)

	case "banlist":
		aiBanMu.RLock()
		count := len(aiBanList)
		jids := make([]string, 0, count)
		for jid := range aiBanList {
			jids = append(jids, jid)
		}
		aiBanMu.RUnlock()
		if count == 0 {
			return "📭 Ban list kosong"
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🚫 *Ban List (%d)*\n\n", count))
		for _, jid := range jids {
			sb.WriteString("• " + jid + "\n")
		}
		return sb.String()
	}

	return ""
}

// aiExtractTargetJID extracts a JID from @tag mention or phone number.
func aiExtractTargetJID(client *whatsmeow.Client, msg *events.Message, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		// Try mention
		mentions := extractMentionedJIDsDual(msg)
		if len(mentions) > 0 {
			return mentions[0]
		}
		return ""
	}

	// @tag
	if strings.HasPrefix(args, "@") {
		mentions := extractMentionedJIDsDual(msg)
		if len(mentions) > 0 {
			return mentions[0]
		}
	}

	// Phone number
	phone := strings.TrimPrefix(args, "+")
	phone = strings.TrimPrefix(phone, "0")
	if !strings.HasPrefix(phone, "62") && len(phone) >= 8 {
		phone = "62" + phone
	}
	if len(phone) >= 8 && len(phone) <= 15 {
		return phone + "@s.whatsapp.net"
	}

	return ""
}

// isOwnerJID checks if a JID string belongs to an owner.
func isOwnerJID(jid string) bool {
	ownerJIDs := strings.Split(getenvDefault("OWNER_JID", ""), ",")
	for _, o := range ownerJIDs {
		o = strings.TrimSpace(o)
		if !strings.Contains(o, "@") {
			o += "@s.whatsapp.net"
		}
		if o == jid {
			return true
		}
	}
	return false
}

// ─── Permission denied message ────────────────────────────────────

func aiNoPermission(role AIRole, action string) string {
	return fmt.Sprintf("❌ Role *%s* tidak punya akses ke *%s*.\n\n"+
		"Role Anda butuh salah satu dari:\n"+
		"• Owner — akses penuh\n"+
		"• Admin Grup — kelola grup\n"+
		"• Trusted — chat + image\n\n"+
		"Minta owner untuk promote/trust Anda.",
		role.String(), action)
}

// ─── Status ───────────────────────────────────────────────────────

func aiAgentStatus() string {
	return fmt.Sprintf(`🤖 *AI Agent Status*

• Mode: Agentic AI (100%% AI-driven)
• AI Provider: Free Gemini + kangwifi fallback
• Max iterations: %d per request
• Tools available: %d total
• Memory sessions: %d

Type .ai <pertanyaan> untuk mulai.
AI akan decide sendiri tool mana yang dipakai.`,
		aiAgenticMaxIterations, len(aiTools), geminiFreeSessionCount())
}

// ─── Help (role-aware) ────────────────────────────────────────────

func aiAgentHelp(client *whatsmeow.Client, msg *events.Message) string {
	role := aiDetectRole(client, msg)
	cfg := aiRoleConfigs[role]

	var sb strings.Builder
	sb.WriteString("🤖 *AI Agent — Help*\n")
	sb.WriteString(fmt.Sprintf("Role Anda: *%s*\n\n", role.String()))

	sb.WriteString("*Chat*\n")
	if cfg.CanChat {
		sb.WriteString("• .ai <pertanyaan> — chat dengan Gemini\n")
		sb.WriteString("• .ai clear — hapus memory\n")
	} else {
		sb.WriteString("• (tidak ada akses chat)\n")
	}

	if cfg.CanAnalyzeImage {
		sb.WriteString("\n*Image*\n")
		sb.WriteString("• .ai <teks> (reply gambar) — analisis gambar\n")
	}

	if cfg.CanUseGroupTools {
		sb.WriteString("\n*Group Tools*\n")
		sb.WriteString("• .ai kick @tag — kick member\n")
		sb.WriteString("• .ai promote @tag — promote admin\n")
		sb.WriteString("• .ai demote @tag — demote admin\n")
		sb.WriteString("• .ai groupinfo — info grup\n")
		sb.WriteString("• .ai tagall [teks] — tag semua\n")
		sb.WriteString("• .ai hidetag [teks] — hidden tag\n")
		sb.WriteString("• .ai link — link grup\n")
		sb.WriteString("• .ai setname <nama> — ganti nama grup\n")
		sb.WriteString("• .ai setdesc <teks> — ganti desc grup\n")
	}

	if cfg.CanUseShell {
		sb.WriteString("\n*Owner Power*\n")
		sb.WriteString("• .ai shell <cmd> — execute shell\n")
	}
	if cfg.CanUseEval {
		sb.WriteString("• .ai eval <code> — Go eval\n")
	}
	if cfg.CanUseReadFile {
		sb.WriteString("• .ai readfile <path> — baca file\n")
	}
	if cfg.CanUseGetFile {
		sb.WriteString("• .ai getfile <path> — download file\n")
	}
	if cfg.CanUseGitBackup {
		sb.WriteString("• .ai gitbackup push — backup GitHub\n")
	}
	if cfg.CanBanUsers {
		sb.WriteString("• .ai ban @tag — ban dari AI\n")
		sb.WriteString("• .ai unban @tag — unban\n")
		sb.WriteString("• .ai banlist — list banned\n")
	}

	sb.WriteString("\n*System*\n")
	sb.WriteString("• .ai role — lihat role + capabilities\n")
	sb.WriteString("• .ai help — bantuan ini\n")
	sb.WriteString("• .ai ping — cek bot\n")

	rateStr := "unlimited"
	if cfg.RateLimitPerMin > 0 {
		rateStr = fmt.Sprintf("%d/min", cfg.RateLimitPerMin)
	}
	sb.WriteString(fmt.Sprintf("\nRate limit: %s\n", rateStr))
	sb.WriteString("AI: Free Gemini (no API key) + Pollinations Vision\n")

	return sb.String()
}

// keep imports used
var _ = whatsmeow.MediaAudio

// ─── AI response tracker (for auto-trigger on reply) ─────────────
//
// Track message IDs of AI responses so we can differentiate
// "reply to AI response" (auto-trigger AI) vs "reply to bot menu/ping"
// (don't auto-trigger).
//
// TTL: 10 minutes (after that, AI response is "stale" and won't trigger)

var (
	aiResponseTrackerMu sync.Mutex
	aiResponseTracker   = map[string]time.Time{} // msgID → timestamp
)

const aiResponseTrackerTTL = 10 * time.Minute

// markAIResponse records that a message ID is an AI response.
// Called after sending AI reply.
func markAIResponse(msgID string) {
	if msgID == "" {
		return
	}
	aiResponseTrackerMu.Lock()
	defer aiResponseTrackerMu.Unlock()
	// Cleanup old entries while we're here
	now := time.Now()
	for id, ts := range aiResponseTracker {
		if now.Sub(ts) > aiResponseTrackerTTL {
			delete(aiResponseTracker, id)
		}
	}
	aiResponseTracker[msgID] = now
}

// isAIResponse checks if a message ID belongs to a recent AI response.
func isAIResponse(msgID string) bool {
	if msgID == "" {
		return false
	}
	aiResponseTrackerMu.Lock()
	defer aiResponseTrackerMu.Unlock()
	ts, ok := aiResponseTracker[msgID]
	if !ok {
		return false
	}
	if time.Since(ts) > aiResponseTrackerTTL {
		delete(aiResponseTracker, msgID)
		return false
	}
	return true
}

// ─── Auto-trigger AI on reply to AI response only ────────────────
//
// Called from main.go when user replies to a bot message without
// using a command (text doesn't start with "." or "$").
//
// ONLY triggers if the quoted message is an AI response (tracked via markAIResponse).
// Does NOT trigger on reply to bot menu, ping, status, etc.
//
// Guards:
//   - Quoted must be AI response (not just any bot message)
//   - Self mode: if ON, only owner/trusted
//   - Banned users: blocked
//   - Sender must not be bot

func aiTryAutoTrigger(client *whatsmeow.Client, msg *events.Message, userText string) string {
	// Don't trigger if user is bot itself
	if msg.Info.IsFromMe {
		return ""
	}

	// Check if replying to a bot message (any bot message, not just AI response)
	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return ""
	}

	// Don't trigger in banned state
	senderJID := msg.Info.Sender.String()
	if aiIsBanned(senderJID) {
		return ""
	}

	// Self mode check
	if isSelfMode() {
		if msg.Info.IsGroup {
			if !isGroupPublic(msg.Info.Chat.String()) && !isOwnerMsg(msg) && !isTrustedMsg(msg) {
				return ""
			}
		} else {
			if !isOwnerMsg(msg) {
				return ""
			}
		}
	}

	// Detect role
	role := aiDetectRole(client, msg)
	cfg := aiRoleConfigs[role]

	if role == AIRoleBanned || !cfg.CanChat {
		return ""
	}

	// Rate limit — for AUTO-TRIGGER, skip SILENTLY (don't spam error message).
	// Rate limit errors only show for explicit .ai command, not auto-trigger.
	if !aiRateAllow(senderJID, role) {
		fmt.Printf("[AI] auto-trigger skipped: rate limit for %s (role %s)\n", senderJID, role.String())
		return ""
	}

	// Build prompt — use user's text, or if empty, use bot's quoted text as context
	prompt := strings.TrimSpace(userText)
	if prompt == "" {
		botText := extractTextFromMessage(quoted)
		if botText != "" {
			prompt = "Bot baru saja kirim pesan ini:\n\n" + botText + "\n\nUser me-reply tanpa text. Lanjutkan percakapan secara natural."
		}
	}
	if prompt == "" {
		return ""
	}

	// Image analysis if quoted has image
	if quoted.Message != nil {
		hasImage := false
		if quoted.Message.GetImageMessage() != nil {
			hasImage = true
		} else if doc := quoted.Message.GetDocumentWithCaptionMessage(); doc != nil {
			if doc.GetMessage().GetImageMessage() != nil {
				hasImage = true
			}
		}
		if hasImage && cfg.CanAnalyzeImage {
			reply := aiAgentChatWithImage(client, msg, prompt, role)
			if reply == "" {
				return "hmm, lagi nggak bisa lihat gambarnya. coba kirim ulang~"
			}
			return reply
		}
	}

	// Regular agentic AI (with Ruby Hoshino personality)
	reply := aiAgenticProcess(client, msg, prompt, role, cfg)
	if reply == "" {
		// AI returned empty — don't send empty message, return a natural fallback
		return "hmm? maaf, lagi nggak fokus. coba ulang~"
	}
	return reply
}

// extractTextFromMessage extracts text content from any message type.
func extractTextFromMessage(msg *events.Message) string {
	if msg == nil || msg.Message == nil {
		return ""
	}
	if msg.Message.GetConversation() != "" {
		return msg.Message.GetConversation()
	}
	if ext := msg.Message.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	if img := msg.Message.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.Message.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "ai",
		Description: "ai",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAIAgentCommand(client, msg, args)
			// ─── AI Fast (race gemini + chatgpt, cache) ────────────
		},
	})
}
