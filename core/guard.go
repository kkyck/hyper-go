package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

/**
 * 🛡️ Group Guard Plugin — Jaga grup (file-based)
 * ════════════════════════════════════════════════════════════
 * Port dari plugins/group-guard.ts, antilink.ts, antispam.ts, welcome.ts
 *
 * Commands (prefix .):
 *   .guard status              → Status semua fitur
 *   .guard antilink on|off     → Hapus pesan berisi URL
 *   .guard antispam on|off     → Rate-limit pesan beruntun
 *   .welcome on|off|set|status → Sambut member baru
 *   .guard leave on|off|set    → Pesan saat member keluar
 *
 * Storage: ./data/group_guard.json
 */

type GuardSettings struct {
	WelcomeOn    bool   `json:"welcome_on"`
	WelcomeText  string `json:"welcome_text"`
	LeaveOn      bool   `json:"leave_on"`
	LeaveText    string `json:"leave_text"`
	Antilink     bool   `json:"antilink"`
	Antispam     bool   `json:"antispam"`
	SpamLimit    int    `json:"spam_limit"`
	SpamWindowMs int64  `json:"spam_window_ms"`
}

type GuardDoc struct {
	GroupJID string        `json:"group_jid"`
	Settings GuardSettings `json:"settings"`
}

func defaultGuardSettings() GuardSettings {
	return GuardSettings{
		WelcomeOn:    true,
		WelcomeText:  "👋 Selamat datang @user di *@group*!\nSemoga betah ya 🌟",
		LeaveOn:      true,
		LeaveText:    "👋 @user telah keluar dari grup.",
		Antilink:     false,
		Antispam:     false,
		SpamLimit:    5,
		SpamWindowMs: 10_000,
	}
}

var (
	spamTracker   = make(map[string][]int64)
	spamTrackerMu sync.Mutex
	urlRegex      = regexp.MustCompile(`(?i)(https?://|www\.|wa\.me/|chat\.whatsapp\.com/)[^\s]+`)
)

func containsLink(text string) bool {
	return urlRegex.MatchString(text)
}

func checkSpam(groupJID, senderJID string, limit int, windowMs int64) bool {
	key := groupJID + "\n" + senderJID
	now := time.Now().UnixMilli()
	spamTrackerMu.Lock()
	defer spamTrackerMu.Unlock()
	var fresh []int64
	for _, t := range spamTracker[key] {
		if now-t < windowMs {
			fresh = append(fresh, t)
		}
	}
	fresh = append(fresh, now)
	if len(fresh) > 100 {
		fresh = fresh[len(fresh)-100:]
	}
	spamTracker[key] = fresh
	return len(fresh) > limit
}

// cleanupSpamTracker hapus entry kadaluarsa (anti memory-leak).
func cleanupSpamTracker() {
	spamTrackerMu.Lock()
	defer spamTrackerMu.Unlock()
	now := time.Now().UnixMilli()
	for k, v := range spamTracker {
		if len(v) == 0 || now-v[len(v)-1] > 3600000 {
			delete(spamTracker, k)
		}
	}
	if len(spamTracker) > 10000 {
		for k := range spamTracker {
			delete(spamTracker, k)
			if len(spamTracker) <= 10000 {
				break
			}
		}
	}
}

// applyGuardFilters: true = pesan sudah ditindak (hapus/warn), skip command.
func applyGuardFilters(client *whatsmeow.Client, _ any, msg *events.Message, text string) bool {
	if !msg.Info.IsGroup {
		return false
	}
	groupJID := msg.Info.Chat.String()
	doc := getGroupGuard(groupJID)
	s := doc.Settings

	if s.Antilink && containsLink(text) {
		_, _ = client.RevokeMessage(context.Background(), msg.Info.Chat, msg.Info.ID)
		sendTextMsg(client, msg.Info.Chat, "🚫 *Antilink aktif!* Link tidak diizinkan di grup ini")
		return true
	}

	if s.Antispam {
		limit := s.SpamLimit
		if limit <= 0 {
			limit = 5
		}
		window := s.SpamWindowMs
		if window <= 0 {
			window = 10_000
		}
		if checkSpam(groupJID, msg.Info.Sender.String(), limit, window) {
			_, _ = client.RevokeMessage(context.Background(), msg.Info.Chat, msg.Info.ID)
			sendTextMsg(client, msg.Info.Chat, "⚠️ *Antispam!* Terlalu banyak pesan. Tunggu sebentar")
			return true
		}
	}
	return false
}

func handleGroupInfo(client *whatsmeow.Client, _ any, evt *events.GroupInfo) {
	if evt == nil {
		return
	}
	groupJID := evt.JID.String()

	// 🔒 SILENT GATE: di grup TERKUNCI (belum .self off), bot 100% diam.
	// Welcome / leave TIDAK boleh muncul — bocor kalau dipancang di grup yang belum dibuka.
	if !isGroupPublic(groupJID) {
		return
	}

	doc := getGroupGuard(groupJID)
	s := doc.Settings

	groupName := groupJID
	if meta, err := client.GetGroupInfo(context.Background(), evt.JID); err == nil && meta != nil && meta.Name != "" {
		groupName = meta.Name
	}

	wib := time.FixedZone("WIB", 7*3600)
	now := time.Now().In(wib)

	if s.WelcomeOn {
		for _, jid := range evt.Join {
			phone := extractPhone(jid.String())
			text := s.WelcomeText
			text = strings.ReplaceAll(text, "@user", "@"+phone)
			text = strings.ReplaceAll(text, "@group", groupName)
			text = strings.ReplaceAll(text, "@date", now.Format("02-01-2006"))
			text = strings.ReplaceAll(text, "@time", now.Format("15:04"))
			sendTextMsg(client, evt.JID, text)
		}
	}
	if s.LeaveOn {
		for _, jid := range evt.Leave {
			phone := extractPhone(jid.String())
			text := s.LeaveText
			text = strings.ReplaceAll(text, "@user", "@"+phone)
			text = strings.ReplaceAll(text, "@group", groupName)
			sendTextMsg(client, evt.JID, text)
		}
	}
}

func handleGuardCommand(client *whatsmeow.Client, _ any, msg *events.Message, cmd, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Perintah guard hanya bisa dipakai di *grup*"
	}
	// Only owner or group admin can manage guard settings
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner atau admin grup yang bisa mengatur guard"
	}

	groupJID := msg.Info.Chat.String()
	doc := getGroupGuard(groupJID)

	parts := strings.Fields(args)
	sub := ""
	if len(parts) > 0 {
		sub = strings.ToLower(parts[0])
	}

	onOff := func(b bool) string {
		if b {
			return "ON ✅"
		}
		return "OFF"
	}

	// .guard status / .guard
	if cmd == ".guard" && (sub == "" || sub == "status") {
		s := doc.Settings
		return fmt.Sprintf(`🛡️ *Group Guard Status*

• Welcome: %s
• Leave: %s
• Antilink: %s
• Antispam: %s (%d msg / %ds)

*Perintah:*
• .guard antilink on|off
• .guard antispam on|off
• .welcome on|off
• .welcome set <teks>
• .guard leave on|off
• .guard leave set <teks>
• .guard status`,
			onOff(s.WelcomeOn), onOff(s.LeaveOn), onOff(s.Antilink),
			onOff(s.Antispam), s.SpamLimit, s.SpamWindowMs/1000)
	}

	// antilink
	if cmd == ".antilink" || sub == "antilink" {
		val := sub
		if cmd == ".antilink" && len(parts) > 0 {
			val = strings.ToLower(parts[0])
		} else if len(parts) > 1 {
			val = strings.ToLower(parts[1])
		}
		on := val == "on" || val == "1" || val == "true"
		doc.Settings.Antilink = on
		_ = persistGuards()
		if on {
			return "✅ *Antilink* diaktifkan"
		}
		return "✅ *Antilink* dimatikan"
	}

	// antispam
	if cmd == ".antispam" || sub == "antispam" {
		val := ""
		if cmd == ".antispam" && len(parts) > 0 {
			val = strings.ToLower(parts[0])
		} else if len(parts) > 1 {
			val = strings.ToLower(parts[1])
		}
		on := val == "on" || val == "1" || val == "true"
		doc.Settings.Antispam = on
		_ = persistGuards()
		if on {
			return fmt.Sprintf("✅ *Antispam* aktif (max %d pesan / %ds)", doc.Settings.SpamLimit, doc.Settings.SpamWindowMs/1000)
		}
		return "✅ *Antispam* dimatikan"
	}

	// welcome
	if cmd == ".welcome" || sub == "welcome" {
		action := sub
		if cmd == ".welcome" {
			action = sub
		} else if len(parts) > 1 {
			action = strings.ToLower(parts[1])
		}
		switch action {
		case "on", "1", "true":
			doc.Settings.WelcomeOn = true
			_ = persistGuards()
			return "✅ Welcome *aktif*"
		case "off", "0", "false":
			doc.Settings.WelcomeOn = false
			_ = persistGuards()
			return "✅ Welcome *dimatikan*"
		case "set":
			var teks string
			if cmd == ".welcome" {
				teks = strings.TrimSpace(strings.Join(parts[1:], " "))
			} else {
				teks = strings.TrimSpace(strings.Join(parts[2:], " "))
			}
			if teks == "" {
				return "❓ *Cara pakai:* *.welcome* set <teks>\nVariabel: @user @group @date @time"
			}
			doc.Settings.WelcomeText = teks
			_ = persistGuards()
			return "✅ Teks welcome disimpan"
		case "status":
			return fmt.Sprintf("📋 *Welcome*\n\n• Status: %s\n• Teks: %s", onOff(doc.Settings.WelcomeOn), doc.Settings.WelcomeText)
		default:
			return "❓ *Cara pakai:* *.welcome* on|off|set|status"
		}
	}

	// leave / goodbye
	if sub == "leave" || sub == "goodbye" {
		if len(parts) < 2 {
			return "❓ *Cara pakai:* *.guard* leave on|off\nAtau: *.guard* leave set <teks>"
		}
		action := strings.ToLower(parts[1])
		switch action {
		case "on":
			doc.Settings.LeaveOn = true
			_ = persistGuards()
			return "✅ Leave message *aktif*"
		case "off":
			doc.Settings.LeaveOn = false
			_ = persistGuards()
			return "✅ Leave message *dimatikan*"
		case "set":
			teks := strings.TrimSpace(strings.Join(parts[2:], " "))
			if teks == "" {
				return "❓ *Cara pakai:* *.guard* leave set <teks>"
			}
			doc.Settings.LeaveText = teks
			_ = persistGuards()
			return "✅ Teks leave disimpan"
		}
	}

	return "❓ Ketik *.guard status* untuk melihat pengaturan"
}

// isGroupAdmin checks if the sender is an admin of the group where the message came from.
func isGroupAdmin(client *whatsmeow.Client, msg *events.Message) bool {
	if msg == nil || !msg.Info.IsGroup {
		return false
	}
	meta, err := client.GetGroupInfo(context.Background(), msg.Info.Chat)
	if err != nil || meta == nil {
		return false
	}
	// Collect all known identities of sender (PN + LID)
	phones, jids := senderIdentity(client, msg)
	phoneSet := map[string]struct{}{}
	for _, p := range phones {
		phoneSet[p] = struct{}{}
	}
	jidSet := map[string]struct{}{}
	for _, j := range jids {
		jidSet[j] = struct{}{}
		// also bare user@server without device
		if i := len(j); i > 0 {
			jidSet[j] = struct{}{}
		}
	}
	senderUser := msg.Info.Sender.User
	senderServer := msg.Info.Sender.Server
	if !msg.Info.SenderAlt.IsEmpty() {
		jidSet[msg.Info.SenderAlt.ToNonAD().String()] = struct{}{}
	}

	for _, p := range meta.Participants {
		match := false
		if p.JID.User == senderUser && p.JID.Server == senderServer {
			match = true
		}
		if !match && !p.LID.IsEmpty() {
			if _, ok := jidSet[p.LID.ToNonAD().String()]; ok {
				match = true
			}
			if p.LID.User == senderUser {
				match = true
			}
		}
		if !match && !p.PhoneNumber.IsEmpty() {
			if _, ok := phoneSet[p.PhoneNumber.User]; ok {
				match = true
			}
			if _, ok := jidSet[p.PhoneNumber.ToNonAD().String()]; ok {
				match = true
			}
		}
		if !match {
			if _, ok := jidSet[p.JID.ToNonAD().String()]; ok {
				match = true
			}
		}
		if match {
			return p.IsAdmin || p.IsSuperAdmin
		}
	}
	return false
}

// isBotGroupAdmin checks if the BOT itself is an admin of the given group chat.
// (isGroupAdmin checks the message SENDER — do not confuse the two.)
func isBotGroupAdmin(client *whatsmeow.Client, chat types.JID) bool {
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return false
	}
	meta, err := client.GetGroupInfo(context.Background(), chat)
	if err != nil || meta == nil {
		return false
	}
	botJID := client.Store.ID.ToNonAD()
	for _, p := range meta.Participants {
		match := p.JID.ToNonAD() == botJID
		if !match && !p.LID.IsEmpty() {
			match = p.LID.ToNonAD() == botJID ||
				(p.LID.User == botJID.User && p.LID.Server == botJID.Server)
		}
		if !match {
			match = p.JID.User == botJID.User && p.JID.Server == botJID.Server
		}
		if match {
			return p.IsAdmin || p.IsSuperAdmin
		}
	}
	return false
}

func sendTextMsg(client *whatsmeow.Client, chat types.JID, text string) {
	ctx := context.Background()
	msg := &waE2E.Message{Conversation: proto.String(text)}
	_, _ = client.SendMessage(ctx, chat, msg)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "guard",
		Aliases:     []string{"antilink", "antispam", "welcome"},
		Description: "guard",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGuardCommand(client, nil, msg, cmd, args)
			// ─── AFK ──────────────────────────────────────────────────
		},
	})
}
