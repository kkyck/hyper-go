package main

/**
 * 🔄 jidutil.go — Dual identity: PN (JID) ↔ LID
 * ════════════════════════════════════════════════════════════
 * WhatsApp group messages often use LID (@lid) instead of phone JID
 * (@s.whatsapp.net). Semua cek owner/trust/admin/mention harus dual.
 *
 * Fast path:
 *   1. SenderAlt dari pesan (gratis)
 *   2. Cache in-memory sync.Map (O(1))
 *   3. Store whatsmeow CachedLIDMap + SQLite
 *   4. Fallback: JID asli
 */

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

var errInvalidJID = errors.New("invalid jid")

var jidAltCache sync.Map // "user@server" → alt "user@server"

func cachePut(a, b types.JID) {
	if a.IsEmpty() || b.IsEmpty() {
		return
	}
	ak := a.ToNonAD().String()
	bk := b.ToNonAD().String()
	if ak == bk {
		return
	}
	jidAltCache.Store(ak, bk)
	jidAltCache.Store(bk, ak)
}

func cacheGet(jid types.JID) (types.JID, bool) {
	if jid.IsEmpty() {
		return types.EmptyJID, false
	}
	if v, ok := jidAltCache.Load(jid.ToNonAD().String()); ok {
		if s, ok2 := v.(string); ok2 && s != "" {
			if parsed, err := types.ParseJID(s); err == nil {
				return parsed, true
			}
		}
	}
	return types.EmptyJID, false
}

func isLID(jid types.JID) bool {
	return jid.Server == types.HiddenUserServer || jid.Server == types.HostedLIDServer
}

func isPN(jid types.JID) bool {
	return jid.Server == types.DefaultUserServer || jid.Server == types.LegacyUserServer
}

func resolveAlt(client *whatsmeow.Client, jid types.JID) types.JID {
	if jid.IsEmpty() {
		return types.EmptyJID
	}
	if alt, ok := cacheGet(jid); ok {
		return alt
	}
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return types.EmptyJID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	alt, err := client.Store.GetAltJID(ctx, jid.ToNonAD())
	if err != nil || alt.IsEmpty() {
		return types.EmptyJID
	}
	cachePut(jid.ToNonAD(), alt.ToNonAD())
	return alt
}

// jidToPN → phone form @s.whatsapp.net (kalau bisa)
func jidToPN(client *whatsmeow.Client, jid types.JID) types.JID {
	if jid.IsEmpty() {
		return jid
	}
	if isPN(jid) {
		return jid.ToNonAD()
	}
	if isLID(jid) {
		if alt := resolveAlt(client, jid); !alt.IsEmpty() && isPN(alt) {
			return alt.ToNonAD()
		}
	}
	return jid.ToNonAD()
}

// jidToLID → @lid form (kalau bisa)
func jidToLID(client *whatsmeow.Client, jid types.JID) types.JID {
	if jid.IsEmpty() {
		return jid
	}
	if isLID(jid) {
		return jid.ToNonAD()
	}
	if isPN(jid) {
		if alt := resolveAlt(client, jid); !alt.IsEmpty() && isLID(alt) {
			return alt.ToNonAD()
		}
	}
	return jid.ToNonAD()
}

// phoneOf → digit nomor HP terbaik dari JID (PN atau LID→PN)
func phoneOf(client *whatsmeow.Client, jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	if isPN(jid) {
		return jid.User
	}
	if isLID(jid) {
		pn := jidToPN(client, jid)
		if isPN(pn) && pn.User != "" {
			return pn.User
		}
	}
	return jid.User
}

// dualForms returns [PN, LID] variants (whichever known) for a single JID.
func dualForms(client *whatsmeow.Client, jid types.JID) []types.JID {
	if jid.IsEmpty() {
		return nil
	}
	out := []types.JID{jid.ToNonAD()}
	seen := map[string]struct{}{jid.ToNonAD().String(): {}}
	add := func(j types.JID) {
		if j.IsEmpty() {
			return
		}
		jn := j.ToNonAD()
		s := jn.String()
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, jn)
	}
	if isPN(jid) {
		add(jidToLID(client, jid))
	} else if isLID(jid) {
		add(jidToPN(client, jid))
	}
	return out
}

// parseAnyJID parses phone, PN JID, or LID JID string into types.JID.
func parseAnyJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "@")
	if raw == "" {
		return types.EmptyJID, errInvalidJID
	}
	if strings.Contains(raw, "@") {
		return types.ParseJID(raw)
	}
	// digits only → assume PN
	phone := extractPhone(raw)
	if phone == "" {
		return types.EmptyJID, errInvalidJID
	}
	return types.NewJID(phone, types.DefaultUserServer), nil
}

// resolveTargetJID turns mention/args into the best JID for group actions.
// Prefers form that matches group participant addressing when possible.
func resolveTargetJID(client *whatsmeow.Client, raw string) (types.JID, error) {
	j, err := parseAnyJID(raw)
	if err != nil || j.IsEmpty() {
		return types.EmptyJID, err
	}
	// Prefer PN for UpdateGroupParticipants (most stable); include LID via dual if needed by caller
	if isLID(j) {
		pn := jidToPN(client, j)
		if isPN(pn) {
			return pn, nil
		}
	}
	return j.ToNonAD(), nil
}

// senderIdentity collects all known phones + jid strings for the message sender.
func senderIdentity(client *whatsmeow.Client, msg *events.Message) (phones []string, jids []string) {
	if msg == nil {
		return nil, nil
	}
	seenP := map[string]struct{}{}
	seenJ := map[string]struct{}{}
	add := func(j types.JID) {
		if j.IsEmpty() {
			return
		}
		jn := j.ToNonAD()
		js := jn.String()
		if _, ok := seenJ[js]; !ok {
			seenJ[js] = struct{}{}
			jids = append(jids, js)
		}
		if isPN(jn) {
			if _, ok := seenP[jn.User]; !ok {
				seenP[jn.User] = struct{}{}
				phones = append(phones, jn.User)
			}
		}
	}

	add(msg.Info.Sender)
	add(msg.Info.SenderAlt)

	if !msg.Info.Sender.IsEmpty() && !msg.Info.SenderAlt.IsEmpty() {
		cachePut(msg.Info.Sender.ToNonAD(), msg.Info.SenderAlt.ToNonAD())
	}

	if client != nil {
		for _, base := range []types.JID{msg.Info.Sender, msg.Info.SenderAlt} {
			if base.IsEmpty() {
				continue
			}
			for _, d := range dualForms(client, base) {
				add(d)
			}
		}
	}
	return phones, jids
}

func senderPhoneBest(client *whatsmeow.Client, msg *events.Message) string {
	phones, _ := senderIdentity(client, msg)
	if len(phones) > 0 {
		return phones[0]
	}
	if msg != nil && !msg.Info.Sender.IsEmpty() {
		return msg.Info.Sender.User
	}
	return ""
}

func normalizeToPhoneKey(client *whatsmeow.Client, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "@") {
		jid, err := types.ParseJID(raw)
		if err != nil {
			return extractPhone(raw)
		}
		return phoneOf(client, jid)
	}
	return extractPhone(raw)
}

func learnMappingFromMessage(client *whatsmeow.Client, msg *events.Message) {
	if msg == nil {
		return
	}
	s, a := msg.Info.Sender, msg.Info.SenderAlt
	if s.IsEmpty() || a.IsEmpty() {
		return
	}
	cachePut(s.ToNonAD(), a.ToNonAD())
	if client != nil {
		client.StoreLIDPNMapping(context.Background(), s.ToNonAD(), a.ToNonAD())
	}
}

// jidsMatch reports whether two JIDs refer to the same person (PN or LID).
func jidsMatch(client *whatsmeow.Client, a, b types.JID) bool {
	if a.IsEmpty() || b.IsEmpty() {
		return false
	}
	an, bn := a.ToNonAD(), b.ToNonAD()
	if an.User == bn.User && an.Server == bn.Server {
		return true
	}
	if an.User == bn.User {
		return true // same user id across servers rarely, but cheap check
	}
	// Compare via phone
	pa, pb := phoneOf(client, an), phoneOf(client, bn)
	if pa != "" && pa == pb && (isPN(an) || isPN(bn) || resolveAlt(client, an).User != "" || resolveAlt(client, bn).User != "") {
		// only if at least one side resolved to real phone-like match
		if isPN(an) || isPN(bn) {
			return pa == pb
		}
		// both LID: match if mapped phones equal
		if pa == pb && len(pa) >= 8 {
			return true
		}
	}
	// dual form cross-check
	for _, da := range dualForms(client, an) {
		for _, db := range dualForms(client, bn) {
			if da.User == db.User && da.Server == db.Server {
				return true
			}
		}
	}
	return false
}

// extractMentionedJIDsDual — mentions as raw strings (PN and/or LID as WA sent them).
func extractMentionedJIDsDual(msg *events.Message) []string {
	if msg == nil || msg.Message == nil {
		return nil
	}
	var list []string
	addCtx := func(ci interface{ GetMentionedJID() []string }) {
		if ci == nil {
			return
		}
		for _, j := range ci.GetMentionedJID() {
			if j != "" {
				list = append(list, j)
			}
		}
	}
	if ext := msg.Message.GetExtendedTextMessage(); ext != nil && ext.ContextInfo != nil {
		addCtx(ext.ContextInfo)
	}
	if img := msg.Message.GetImageMessage(); img != nil && img.ContextInfo != nil {
		addCtx(img.ContextInfo)
	}
	if vid := msg.Message.GetVideoMessage(); vid != nil && vid.ContextInfo != nil {
		addCtx(vid.ContextInfo)
	}
	return list
}

// expandMentionsToDual expands each mention to both PN and LID string forms when known.
func expandMentionsToDual(client *whatsmeow.Client, mentions []string) []types.JID {
	var out []types.JID
	seen := map[string]struct{}{}
	for _, m := range mentions {
		j, err := parseAnyJID(m)
		if err != nil || j.IsEmpty() {
			continue
		}
		for _, d := range dualForms(client, j) {
			s := d.String()
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, d)
		}
	}
	return out
}

// extractPhone mengambil digit nomor dari string JID (sebelum @, tanpa suffix device).
func extractPhone(jid string) string {
	if i := strings.Index(jid, "@"); i > 0 {
		jid = jid[:i]
	}
	if i := strings.Index(jid, ":"); i > 0 {
		jid = jid[:i]
	}
	var b strings.Builder
	for _, r := range jid {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
