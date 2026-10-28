package main

/**
 * 🔒 selfmode.go — Self mode + per-group permissions + trust
 * ════════════════════════════════════════════════════════════
 * Global self mode default = ON. When ON, only owner can trigger commands
 * everywhere — EXCEPT groups that have been opened (.self off in that group)
 * or users that have been trusted (.trust @user).
 *
 * Per-group:
 *   - Default: group is "locked" → only owner + trusted users can use user cmds
 *   - Owner: .self off  (di dalam grup) → buka grup, semua member bisa pakai fitur user
 *   - Owner: .self on   (di dalam grup) → kunci lagi
 *   - Owner: .trust @tag → beri kepercayaan ke orang itu di grup ini
 *   - Owner: .untrust @tag → cabut
 *   - Owner: .trust group / .trust all → sama seperti .self off di grup
 *   - Owner: .trust list → lihat daftar trusted di grup
 *
 * Admin grup features (kick, promote, tagall, guard, dll) tetap hanya
 * owner ATAU admin grup yang bisa trigger — member biasa tidak bisa.
 *
 * Env:
 *   OWNER_JID   → comma-separated list of owner JIDs
 *   OWNER_NAME  → display name
 *
 * Persistence:
 *   ./data/selfmode.json  — global self mode
 *   ./data/group_perms.json — per-group public + trusted users
 */

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

type SelfModeState struct {
	SelfMode  bool   `json:"self_mode"`
	UpdatedAt int64  `json:"updated_at"`
	BotJID    string `json:"bot_jid"`
}

// GroupPerm holds permission overrides for one group.
type GroupPerm struct {
	GroupJID     string   `json:"group_jid"`
	Public       bool     `json:"public"`        // true = .self off di grup ini
	TrustedUsers []string `json:"trusted_users"` // phone or full JID
	UpdatedAt    int64    `json:"updated_at"`
}

var (
	selfModeMu     sync.RWMutex
	selfModeState  = SelfModeState{SelfMode: true, UpdatedAt: 0}
	selfModeFile   = dataDir + "/selfmode.json"
	groupPermsMu   sync.RWMutex
	groupPerms     = make(map[string]*GroupPerm) // key = group JID
	groupPermsFile = dataDir + "/group_perms.json"
)

func loadSelfMode() {
	selfModeMu.Lock()
	defer selfModeMu.Unlock()
	_ = loadJSON(selfModeFile, &selfModeState)
	if selfModeState.UpdatedAt == 0 {
		selfModeState.SelfMode = true
	}
	loadGroupPerms()
}

func saveSelfMode() {
	selfModeMu.Lock()
	defer selfModeMu.Unlock()
	selfModeState.UpdatedAt = time.Now().UnixMilli()
	_ = saveJSON(selfModeFile, &selfModeState)
}

func setSelfMode(on bool) {
	selfModeMu.Lock()
	selfModeState.SelfMode = on
	selfModeState.UpdatedAt = time.Now().UnixMilli()
	selfModeMu.Unlock()
	saveSelfMode()
}

func isSelfMode() bool {
	selfModeMu.RLock()
	defer selfModeMu.RUnlock()
	return selfModeState.SelfMode
}

// ─── Group permissions ───────────────────────────────────────

func loadGroupPerms() {
	groupPermsMu.Lock()
	defer groupPermsMu.Unlock()
	var list []GroupPerm
	if err := loadJSON(groupPermsFile, &list); err != nil {
		return
	}
	groupPerms = make(map[string]*GroupPerm)
	for i := range list {
		p := list[i]
		// copy to heap so pointer stays valid
		cp := p
		groupPerms[cp.GroupJID] = &cp
	}
}

// persistGroupPermsLocked writes to disk. Caller MUST hold groupPermsMu (R or W).
func persistGroupPermsLocked() error {
	list := make([]GroupPerm, 0, len(groupPerms))
	for _, p := range groupPerms {
		list = append(list, *p)
	}
	return saveJSON(groupPermsFile, list)
}

func persistGroupPerms() error {
	groupPermsMu.RLock()
	defer groupPermsMu.RUnlock()
	return persistGroupPermsLocked()
}

func getGroupPerm(groupJID string) *GroupPerm {
	groupPermsMu.Lock()
	defer groupPermsMu.Unlock()
	if p, ok := groupPerms[groupJID]; ok {
		return p
	}
	p := &GroupPerm{GroupJID: groupJID, Public: false, TrustedUsers: nil}
	groupPerms[groupJID] = p
	return p
}

func setGroupPublic(groupJID string, public bool) {
	groupPermsMu.Lock()
	p, ok := groupPerms[groupJID]
	if !ok {
		p = &GroupPerm{GroupJID: groupJID}
		groupPerms[groupJID] = p
	}
	p.Public = public
	p.UpdatedAt = time.Now().UnixMilli()
	_ = persistGroupPermsLocked()
	groupPermsMu.Unlock()
}

func isGroupPublic(groupJID string) bool {
	groupPermsMu.RLock()
	defer groupPermsMu.RUnlock()
	if p, ok := groupPerms[groupJID]; ok {
		return p.Public
	}
	return false
}

func normalizeTrustKey(jid string) string {
	return extractPhone(jid)
}

func isTrustedInGroup(groupJID, senderJID string) bool {
	// Resolve phone OUTSIDE the lock (may hit store/cache)
	phone := normalizeTrustKey(senderJID)
	if phone != "" && botClient != nil && strings.Contains(senderJID, "@lid") {
		if parsed, err := types.ParseJID(senderJID); err == nil {
			if ph := phoneOf(botClient, parsed); ph != "" {
				phone = ph
			}
		}
	}
	groupPermsMu.RLock()
	defer groupPermsMu.RUnlock()
	p, ok := groupPerms[groupJID]
	if !ok || p == nil {
		return false
	}
	for _, t := range p.TrustedUsers {
		if normalizeTrustKey(t) == phone {
			return true
		}
	}
	return false
}

// isTrustedMsg uses full sender identity (PN + LID) against the group trust list.
func isTrustedMsg(msg *events.Message) bool {
	if msg == nil || !msg.Info.IsGroup {
		return false
	}
	groupJID := msg.Info.Chat.String()
	// Resolve identity outside lock
	phones, _ := senderIdentity(botClient, msg)
	if len(phones) == 0 {
		return isTrustedInGroup(groupJID, msg.Info.Sender.String())
	}
	groupPermsMu.RLock()
	defer groupPermsMu.RUnlock()
	p, ok := groupPerms[groupJID]
	if !ok || p == nil {
		return false
	}
	trusted := map[string]struct{}{}
	for _, t := range p.TrustedUsers {
		trusted[normalizeTrustKey(t)] = struct{}{}
	}
	for _, ph := range phones {
		if _, ok := trusted[ph]; ok {
			return true
		}
	}
	return false
}

func addTrust(groupJID, userJID string) bool {
	phone := normalizeTrustKey(userJID)
	if phone == "" && botClient != nil {
		phone = normalizeToPhoneKey(botClient, userJID)
	}
	if phone == "" {
		return false
	}
	groupPermsMu.Lock()
	defer groupPermsMu.Unlock()
	p, ok := groupPerms[groupJID]
	if !ok {
		p = &GroupPerm{GroupJID: groupJID}
		groupPerms[groupJID] = p
	}
	for _, t := range p.TrustedUsers {
		if normalizeTrustKey(t) == phone {
			return false // already trusted
		}
	}
	p.TrustedUsers = append(p.TrustedUsers, phone+"@s.whatsapp.net")
	p.UpdatedAt = time.Now().UnixMilli()
	_ = persistGroupPermsLocked()
	return true
}

func removeTrust(groupJID, userJID string) bool {
	phone := normalizeTrustKey(userJID)
	if phone == "" && botClient != nil {
		phone = normalizeToPhoneKey(botClient, userJID)
	}
	groupPermsMu.Lock()
	defer groupPermsMu.Unlock()
	p, ok := groupPerms[groupJID]
	if !ok || p == nil {
		return false
	}
	var fresh []string
	found := false
	for _, t := range p.TrustedUsers {
		if normalizeTrustKey(t) == phone {
			found = true
			continue
		}
		fresh = append(fresh, t)
	}
	p.TrustedUsers = fresh
	p.UpdatedAt = time.Now().UnixMilli()
	_ = persistGroupPermsLocked()
	return found
}

func listTrusted(groupJID string) []string {
	groupPermsMu.RLock()
	defer groupPermsMu.RUnlock()
	p, ok := groupPerms[groupJID]
	if !ok || p == nil {
		return nil
	}
	out := make([]string, len(p.TrustedUsers))
	copy(out, p.TrustedUsers)
	return out
}

// ─── Owner helpers ───────────────────────────────────────────
// Env:
//   OWNER_JID  → nomor/JID owner, comma-separated
//                contoh: 62812xxx  atau  62812xxx@s.whatsapp.net
//                boleh campur LID: 123456789012345@lid
//   OWNER_LID  → LID owner, comma-separated (opsional, untuk backup jika JID belum ada)
//                contoh: 123456789012345  atau  123456789012345@lid
// Cukup isi salah satu (JID atau LID). Boleh keduanya.

func normalizeOwnerEntry(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "@")
	if p == "" {
		return ""
	}
	low := strings.ToLower(p)
	if strings.Contains(low, "@lid") {
		user := p
		if i := strings.Index(p, "@"); i > 0 {
			user = p[:i]
		}
		return user + "@lid"
	}
	if strings.Contains(p, "@") {
		return p // already full JID
	}
	// pure digits → default phone PN
	return p + "@s.whatsapp.net"
}

func normalizeOwnerLID(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "@")
	if p == "" {
		return ""
	}
	if i := strings.Index(p, "@"); i > 0 {
		p = p[:i]
	}
	return p + "@lid"
}

func ownerJIDs() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	add := func(p string) {
		if p == "" {
			return
		}
		key := stripDevice(p)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}

	for _, part := range strings.Split(os.Getenv("OWNER_JID"), ",") {
		add(normalizeOwnerEntry(part))
	}
	for _, part := range strings.Split(os.Getenv("OWNER_LID"), ",") {
		add(normalizeOwnerLID(part))
	}
	return out
}

func ownerPhones() []string {
	jids := ownerJIDs()
	out := make([]string, 0, len(jids))
	seen := map[string]struct{}{}
	for _, j := range jids {
		if strings.Contains(j, "@lid") {
			if botClient != nil {
				if parsed, err := types.ParseJID(j); err == nil {
					if ph := phoneOf(botClient, parsed); ph != "" {
						if _, ok := seen[ph]; !ok {
							seen[ph] = struct{}{}
							out = append(out, ph)
						}
					}
				}
			}
			continue
		}
		ph := extractPhone(j)
		if ph == "" {
			continue
		}
		if _, ok := seen[ph]; ok {
			continue
		}
		seen[ph] = struct{}{}
		out = append(out, ph)
	}
	return out
}

func isOwner(jid string) bool {
	owners := ownerJIDs()
	if len(owners) == 0 || jid == "" {
		return false
	}
	ownerPhones := map[string]struct{}{} // only real PN phones
	ownerLIDs := map[string]struct{}{}   // LID user ids
	ownerKeys := map[string]struct{}{}   // full jid strings
	for _, o := range owners {
		ownerKeys[o] = struct{}{}
		ownerKeys[stripDevice(o)] = struct{}{}
		if strings.Contains(o, "@lid") {
			ownerLIDs[extractPhone(o)] = struct{}{}
		} else {
			if ph := extractPhone(o); ph != "" {
				ownerPhones[ph] = struct{}{}
			}
		}
	}
	if _, ok := ownerKeys[jid]; ok {
		return true
	}
	if _, ok := ownerKeys[stripDevice(jid)]; ok {
		return true
	}
	ph := extractPhone(jid)
	if strings.Contains(jid, "@lid") {
		if _, ok := ownerLIDs[ph]; ok {
			return true
		}
	} else if ph != "" {
		if _, ok := ownerPhones[ph]; ok {
			return true
		}
	}
	// Dual resolve: LID↔PN via cache/store
	if botClient != nil {
		if parsed, err := types.ParseJID(jid); err == nil {
			for _, form := range dualForms(botClient, parsed) {
				fs := form.String()
				if _, ok := ownerKeys[fs]; ok {
					return true
				}
				if isPN(form) {
					if _, ok := ownerPhones[form.User]; ok {
						return true
					}
				}
				if isLID(form) {
					if _, ok := ownerLIDs[form.User]; ok {
						return true
					}
				}
			}
			if p := phoneOf(botClient, parsed); p != "" {
				if _, ok := ownerPhones[p]; ok {
					return true
				}
			}
		}
	}
	return false
}

func isOwnerMsg(msg *events.Message) bool {
	if msg == nil || msg.Info.Sender.IsEmpty() {
		return false
	}
	if isOwner(msg.Info.Sender.String()) {
		return true
	}
	if !msg.Info.SenderAlt.IsEmpty() && isOwner(msg.Info.SenderAlt.String()) {
		return true
	}
	phones, jidsList := senderIdentity(botClient, msg)
	owners := ownerJIDs()
	ownerPhones := map[string]struct{}{}
	ownerLIDs := map[string]struct{}{}
	ownerKeys := map[string]struct{}{}
	for _, o := range owners {
		ownerKeys[stripDevice(o)] = struct{}{}
		if strings.Contains(o, "@lid") {
			ownerLIDs[extractPhone(o)] = struct{}{}
		} else if ph := extractPhone(o); ph != "" {
			ownerPhones[ph] = struct{}{}
		}
	}
	for _, p := range phones {
		if _, ok := ownerPhones[p]; ok {
			return true
		}
	}
	for _, j := range jidsList {
		if _, ok := ownerKeys[stripDevice(j)]; ok {
			return true
		}
		if strings.Contains(j, "@lid") {
			if _, ok := ownerLIDs[extractPhone(j)]; ok {
				return true
			}
		}
		if isOwner(j) {
			return true
		}
	}
	return false
}

func stripDevice(jid string) string {
	if i := strings.LastIndex(jid, ":"); i > 0 {
		if at := strings.Index(jid, "@"); at > i {
			return jid[:i] + jid[at:]
		}
	}
	return jid
}

// ─── Main gate ───────────────────────────────────────────────

// gateCommand decides whether a user command may proceed.
// Rules:
//  1. Owner always allowed.
//  2. If global self mode is OFF → everyone allowed (public bot).
//  3. In private chat: only owner (when global self ON).
//  4. In group:
//     - if group is Public (.self off di grup) → all members can use USER commands
//     - if sender is trusted in that group → allowed
//     - otherwise denied silently (100% no response in locked groups)
func gateCommand(msg *events.Message) (bool, string) {
	learnMappingFromMessage(botClient, msg)

	if isOwnerMsg(msg) {
		return true, ""
	}

	// Private chat: ikut global self mode
	if !msg.Info.IsGroup {
		if !isSelfMode() {
			return true, ""
		}
		return false, "🔒 *Self Mode Aktif (Private)*\n\n• Hanya owner\n• Di *grup*, atur dengan *.self on/off* (per grup)"
	}

	// ── Grup: PER-GRUP saja ──
	// Default terkunci → 100% silent.
	// Lolos hanya: owner | trusted | grup public (.self off / .trust group)
	// Admin grup BIASA TIDAK lolos kecuali di-trust / grup dibuka.
	groupJID := msg.Info.Chat.String()
	if isGroupPublic(groupJID) {
		return true, ""
	}
	if isTrustedMsg(msg) {
		return true, ""
	}
	return false, ""
}

// groupAllowsSender — silent total di grup terkunci.
// Admin grup tidak otomatis boleh; harus owner / trust / public.
func groupAllowsSender(msg *events.Message) bool {
	if msg == nil {
		return false
	}
	if isOwnerMsg(msg) {
		return true
	}
	if !msg.Info.IsGroup {
		return !isSelfMode() // private mengikuti global
	}
	groupJID := msg.Info.Chat.String()
	if isGroupPublic(groupJID) {
		return true
	}
	return isTrustedMsg(msg)
}

// handleSelfCommand processes .self on|off|status
// When used inside a group, on/off toggles THAT group's public flag.
// When used in private chat, on/off toggles global self mode.
func handleSelfCommand(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa mengatur self mode"
	}
	parts := strings.Fields(strings.ToLower(args))
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}

	inGroup := msg.Info.IsGroup
	groupJID := ""
	if inGroup {
		groupJID = msg.Info.Chat.String()
	}

	switch sub {
	case "on":
		if !inGroup {
			return "⚠️ *Self Mode Grup*\n\n*.self on/off* untuk kunci/buka *grup* harus diketik *di dalam grup*\n\nDi private:\n• *.self status*\n• *.self global on|off* — hanya private chat\n\nTiap grup punya status *sendiri* (tidak ikut global)"
		}
		setGroupPublic(groupJID, false)
		return "🔒 *Grup Ini Dikunci*\n\n• Hanya *owner* + *trusted* yang bisa command\n• Admin grup biasa = diam (kecuali di-trust / .self off)\n• Grup lain tidak berubah"
	case "off":
		if !inGroup {
			return "⚠️ *Self Mode Grup*\n\n• *.self on/off* harus di *grup*\n• Private: *.self global on|off*\n• Status: *.self status*"
		}
		setGroupPublic(groupJID, true)
		return "🌐 *Grup Ini Dibuka (Public)*\n\n• Semua member di *grup ini* bisa fitur user\n• Grup lain tetap statusnya masing-masing"
	case "global":
		if len(parts) < 2 {
			return "❓ *Cara pakai:* *.self global* on|off — hanya *private chat*, bukan grup"
		}
		switch parts[1] {
		case "on":
			setSelfMode(true)
			return "🔒 *Global Private Self ON*\n\n• Private hanya owner\n• *Grup tidak berubah*"
		case "off":
			setSelfMode(false)
			return "🌐 *Global Private Self OFF*\n\n• Private terbuka\n• *Grup tidak berubah*"
		default:
			return "❓ *Cara pakai:* *.self global* on|off"
		}
	case "status", "":
		globalState := "AKTIF 🔒"
		if !isSelfMode() {
			globalState = "OFF (public mode) 🌐"
		}
		owners := ownerJIDs()
		ownerList := "(tidak di-set, set env OWNER_JID)"
		if len(owners) > 0 {
			ownerList = ""
			for i, o := range owners {
				if i > 0 {
					ownerList += "\n"
				}
				ownerList += "• @" + extractPhone(o)
			}
		}
		bot := selfModeState.BotJID
		if bot == "" {
			bot = "(belum terdeteksi)"
		}

		groupInfo := ""
		if inGroup {
			pub := isGroupPublic(groupJID)
			status := "TERKUNCI 🔒 (hanya owner + trusted — admin biasa diam)"
			if pub {
				status = "PUBLIK 🌐 (semua member bisa fitur user)"
			}
			trusted := listTrusted(groupJID)
			tList := "(kosong)"
			if len(trusted) > 0 {
				tList = ""
				for _, t := range trusted {
					tList += "\n• @" + extractPhone(t)
				}
			}
			groupInfo = fmt.Sprintf(`

📦 *Status Grup Ini*
• Mode: %s
• Trusted users:%s

*Perintah di grup:*
• .self on   → kunci grup
• .self off  → buka grup (public)
• .trust @tag → beri trust ke orang
• .untrust @tag → cabut trust
• .trust list → daftar trusted
• .trust group → buka grup (alias .self off)`, status, tList)
		}

		return fmt.Sprintf(`🔒 *Self Mode Status*

• Global: %s
• Owner:
%s
• Bot JID: %s
%s

*Penting:* beda grup = beda status self.

*Di GRUP:*
• .self on  → kunci *grup ini*
• .self off → buka *grup ini*

*Di PRIVATE:*
• .self global on|off
• .self status`, globalState, ownerList, bot, groupInfo)
	}
	return "❓ *Cara pakai:* *.self* on|off|status"
}

// handleTrustCommand — .trust @tag | .trust group | .trust list | .untrust @tag
func handleTrustCommand(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa mengatur trust"
	}
	if !msg.Info.IsGroup {
		return "❌ Fitur .trust hanya bisa dipakai *di dalam grup*"
	}

	groupJID := msg.Info.Chat.String()
	parts := strings.Fields(strings.ToLower(args))
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}

	switch sub {
	case "list", "ls", "status":
		trusted := listTrusted(groupJID)
		pub := isGroupPublic(groupJID)
		mode := "TERKUNCI 🔒"
		if pub {
			mode = "PUBLIK 🌐"
		}
		if len(trusted) == 0 {
			return fmt.Sprintf("📋 *Trust List*\n\n• Mode grup: %s\n• Trusted: (kosong)\n\nCara: *.trust @tag*", mode)
		}
		var lines []string
		for _, t := range trusted {
			lines = append(lines, "• @"+extractPhone(t))
		}
		return fmt.Sprintf("📋 *Trust List*\n\n• Mode grup: %s\n• Trusted (%d):\n%s", mode, len(trusted), strings.Join(lines, "\n"))

	case "group", "all", "public":
		setGroupPublic(groupJID, true)
		return "🌐 *Grup Di-trust / Dibuka*\n\nSemua member sekarang bisa memakai fitur user di grup ini"

	case "off", "lock", "untrustgroup":
		setGroupPublic(groupJID, false)
		return "🔒 *Trust Grup Dicabut*\n\nGrup dikunci lagi — hanya owner + trusted user"

	default:
		// Expect @mention or phone
		mentioned := extractMentionedJIDs(msg)
		target := ""
		if len(mentioned) > 0 {
			target = mentioned[0]
		} else {
			raw := strings.TrimSpace(args)
			raw = strings.TrimPrefix(raw, "@")
			phone := extractPhone(raw)
			if phone != "" {
				target = phone + "@s.whatsapp.net"
			}
		}
		if target == "" {
			return `❓ *Cara pakai:* *.trust*

• *.trust @tag* — beri kepercayaan ke orang (bisa pakai cmd di grup ini)
• *.untrust @tag* — cabut kepercayaan
• *.trust list* — lihat daftar
• *.trust group* — buka grup (semua member)
• *.trust off* — kunci grup lagi`
		}
		// CRITICAL: Resolve LID → PN before storing trust.
		if botClient != nil && strings.Contains(target, "@lid") {
			if parsed, err := types.ParseJID(target); err == nil {
				if ph := phoneOf(botClient, parsed); ph != "" {
					target = ph + "@s.whatsapp.net"
				}
			}
		}
		if addTrust(groupJID, target) {
			return fmt.Sprintf("✅ *@%s* sekarang *trusted*\n\nOrang ini bisa memakai fitur user di grup ini meski grup terkunci", extractPhone(target))
		}
		return fmt.Sprintf("ℹ️ *@%s* sudah ada di daftar trusted", extractPhone(target))
	}
}

func handleUntrustCommand(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa mengatur trust"
	}
	if !msg.Info.IsGroup {
		return "❌ Fitur .untrust hanya di grup"
	}
	groupJID := msg.Info.Chat.String()
	mentioned := extractMentionedJIDs(msg)
	target := ""
	if len(mentioned) > 0 {
		target = mentioned[0]
	} else {
		raw := strings.TrimSpace(args)
		raw = strings.TrimPrefix(raw, "@")
		phone := extractPhone(raw)
		if phone != "" {
			target = phone + "@s.whatsapp.net"
		}
	}
	if target == "" {
		return "❓ *Cara pakai:* *.untrust* @tag"
	}
	// CRITICAL: Resolve LID → PN before removing trust.
	if botClient != nil && strings.Contains(target, "@lid") {
		if parsed, err := types.ParseJID(target); err == nil {
			if ph := phoneOf(botClient, parsed); ph != "" {
				target = ph + "@s.whatsapp.net"
			}
		}
	}
	if removeTrust(groupJID, target) {
		return fmt.Sprintf("✅ Trust *@%s* dicabut", extractPhone(target))
	}
	return fmt.Sprintf("ℹ️ *@%s* tidak ada di daftar trusted", extractPhone(target))
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "trust",
		PreGate:     true,
		Description: "trust",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTrustCommand(args, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "untrust",
		PreGate:     true,
		Description: "untrust",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleUntrustCommand(args, msg)
		},
	})
}
