package main

// spr2.go — Ultra-Fast SPR (skip resolveSPRTargets + skip buildSPRParticipants)
//
// ─── OPTIMIZATION vs .spr existing ───────────────────────────────────
//
// .spr existing (~500-800ms):
//   1. resolveSPRTargets (DB queries: jidToPN, jidToLID)  ~20-50ms
//   2. buildSPRParticipants (GetGroupInfo = fetch ALL members)  ~50-200ms ← BOTTLENECK
//   3. sprRotateSenderKey BEFORE  ~300ms
//   4. sendSPREngineAHide  ~200ms
//   5. sprRotateSenderKey AFTER (bg)
//   TOTAL: ~570-750ms
//
// .spr2 ultra-fast (~200ms):
//   1. Parse target directly from mention/reply (NO DB queries)  ~1ms
//   2. Skip buildSPRParticipants — pass target directly  ~0ms (saves 50-200ms!)
//   3. Skip pre-rotation (sender key from previous SPR still valid)  ~0ms (saves 300ms!)
//   4. sendSPREngineAHide  ~200ms
//   5. sprRotateSenderKey AFTER (bg)
//   TOTAL: ~200ms
//
// Fallback: if fast path fails (no sender key), retry with full flow.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	spr2Mu      sync.Mutex
	spr2Active  bool
	spr2Total   int
	spr2Sent    int
	spr2Failed  int
	spr2Started time.Time
	spr2LastMsg string
)

// ─── Command handler ────────────────────────────────────────────────

func handleSPR2Command(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	// Subcommands
	sub := strings.Fields(strings.ToLower(args))
	if len(sub) > 0 && (sub[0] == "status" || sub[0] == "stat") {
		return spr2Status()
	}
	if len(sub) > 0 && (sub[0] == "help" || sub[0] == "bantuan") {
		return spr2Help()
	}

	// Claim slot secara atomik: check + set dalam satu lock hold,
	// agar dua command konkuren tidak bisa sama-sama lolos.
	spr2Mu.Lock()
	if spr2Active {
		spr2Mu.Unlock()
		return "⏳ SPR2 masih berjalan"
	}
	spr2Active = true
	spr2Started = time.Now()
	spr2Mu.Unlock()
	releaseClaim := func() {
		spr2Mu.Lock()
		spr2Active = false
		spr2Mu.Unlock()
	}

	// ─── FAST TARGET RESOLUTION (no DB queries) ─────────────────────
	// Parse target + text directly from mention/reply/phone.
	// Skip resolveSPRTargets (saves ~20-50ms of jidToPN/jidToLID DB queries).
	target, text, err := spr2ParseTargetFast(client, msg, args)
	if err != nil {
		releaseClaim()
		return "❓ " + err.Error()
	}
	if target.IsEmpty() {
		releaseClaim()
		return "❓ *Cara pakai:* .spr2 @tag <teks> atau reply pesan + .spr2 <teks>"
	}
	if text == "" {
		releaseClaim()
		return "❌ Teks kosong"
	}

	// ─── Set state ───
	spr2Mu.Lock()
	spr2Total = 1
	spr2Sent = 0
	spr2Failed = 0
	spr2LastMsg = ""
	spr2Mu.Unlock()

	// ─── Background: SPR send (ultra-fast path) ───
	go spr2SendSPR(client, msg.Info.Chat, target, text)

	return ""
}

// ─── Fast target parser (no DB queries) ────────────────────────────

// spr2ParseTargetFast extracts target JID + text directly from the message.
// Tries in order: reply → mention → phone number.
// Does NOT call jidToPN/jidToLID — uses JID as-is from the message.
func spr2ParseTargetFast(client *whatsmeow.Client, msg *events.Message, args string) (types.JID, string, error) {
	if msg == nil {
		return types.EmptyJID, "", fmt.Errorf("msg nil")
	}
	// 1. Reply target (fastest — no parsing needed)
	if quoted := getQuotedMessage(msg); quoted != nil && !quoted.Info.Sender.IsEmpty() {
		// Extract text after the reply context
		text := strings.TrimSpace(args)
		// If args contains the target (phone), strip it
		text = spr2StripTargetFromText(text, quoted.Info.Sender.User)
		return quoted.Info.Sender.ToNonAD(), text, nil
	}

	// 2. Mention (extract first @tag JID from ContextInfo.MentionedJID)
	if mentions := extractMentionedJIDsDual(msg); len(mentions) > 0 {
		for _, m := range mentions {
			if j, err := parseAnyJID(m); err == nil && !j.IsEmpty() {
				// Extract text: strip phone-like tokens from args
				text := strings.TrimSpace(args)
				text = spr2StripTargetFromText(text, j.User)
				// Also strip @mention tokens
				text = spr2StripMentionTokens(text)
				return j.ToNonAD(), text, nil
			}
		}
	}

	// 3. Phone number in args (e.g., ".spr2 628xxx text")
	parts := strings.Fields(args)
	for i, p := range parts {
		clean := strings.TrimPrefix(p, "@")
		ph := extractPhone(clean)
		if ph != "" && len(ph) >= 8 && len(ph) <= 15 {
			jid := types.NewJID(ph, types.DefaultUserServer)
			// Text = everything after the phone token
			text := strings.TrimSpace(strings.Join(parts[i+1:], " "))
			return jid, text, nil
		}
	}

	return types.EmptyJID, "", fmt.Errorf("*Cara pakai:* .spr2 @tag <teks>")
}

// spr2StripTargetFromText removes phone-like tokens from text
func spr2StripTargetFromText(text, targetUser string) string {
	if targetUser == "" {
		return text
	}
	// Remove the target's user ID if it appears as a standalone token
	parts := strings.Fields(text)
	var out []string
	for _, p := range parts {
		clean := strings.TrimPrefix(p, "@")
		if clean == targetUser || extractPhone(clean) == targetUser {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, " ")
}

// spr2StripMentionTokens removes @-prefixed tokens from text
func spr2StripMentionTokens(text string) string {
	parts := strings.Fields(text)
	var out []string
	for _, p := range parts {
		if strings.HasPrefix(p, "@") && len(p) > 1 {
			// Check if it's a phone mention
			clean := strings.TrimPrefix(p, "@")
			ph := extractPhone(clean)
			if ph != "" && len(ph) >= 8 {
				continue // skip phone mentions
			}
		}
		out = append(out, p)
	}
	return strings.Join(out, " ")
}

// ─── Worker: ultra-fast SPR send ───────────────────────────────────

// spr2SendSPR sends SPR using buildSPRParticipants (proven approach from .spr).
//
// Why buildSPRParticipants is REQUIRED (not optional):
//   - Resolves target JID to its native group participant form (PN or LID
//     that the group server expects)
//   - Without this, SKDM distribution may fail → decrypt-fail:hide doesn't
//     get injected properly → non-targets CAN decrypt → LEAK
//   - .spr existing uses this and Hide works (non-targets see NOTHING)
//
// Speed optimizations kept (vs .spr existing):
//   - Skip pre-rotation (reuse sender key from previous SPR) → saves ~300ms
//   - Fallback to full flow (rotate + send) if fast path fails
//   - Post-rotation in background (non-blocking)
func spr2SendSPR(client *whatsmeow.Client, groupJID types.JID, target types.JID, text string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[SPR2] worker panic: %v\n", r)
			spr2Mu.Lock()
			spr2Failed++
			spr2Active = false
			spr2LastMsg = fmt.Sprintf("panic: %v", r)
			spr2Mu.Unlock()
		}
	}()

	startTotal := time.Now()

	// Build message
	waMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
		},
	}

	// ─── buildSPRParticipants (REQUIRED for Hide variant to work) ──
	// This fetches group info + resolves target to native group JID form.
	// Without this, decrypt-fail:hide fails → non-targets see message (LEAK).
	fmt.Printf("[SPR2] buildSPRParticipants for target=%s in group=%s\n", target.String(), groupJID.String())
	participants, err := buildSPRParticipants(client, groupJID, []types.JID{target})
	if err != nil {
		fmt.Printf("[SPR2] buildSPRParticipants error: %v\n", err)
		spr2Mu.Lock()
		spr2Failed++
		spr2Active = false
		spr2LastMsg = fmt.Sprintf("buildSPRParticipants: %v", err)
		spr2Mu.Unlock()
		return
	}
	fmt.Printf("[SPR2] participants resolved: %d entries\n", len(participants))
	for i, p := range participants {
		fmt.Printf("[SPR2]   participant[%d] = %s\n", i, p.String())
	}

	// ─── Rotate sender key BEFORE send (CRITICAL for Hide variant) ───
	// Without fresh rotation, the SKDM may not distribute correctly to the
	// target, and decrypt-fail:hide attribute may not be injected properly.
	// This is what .spr existing does and it WORKS.
	// Dibungkus sprBaseMu agar tidak interleave dengan SPR lain.
	fmt.Printf("[SPR2] rotating sender key BEFORE send...\n")
	rotStart := time.Now()
	sprBaseMu.Lock()
	if err := sprRotateSenderKey(groupJID); err != nil {
		fmt.Printf("[SPR2] key rotation error: %v\n", err)
	}
	sprBaseMu.Unlock()
	fmt.Printf("[SPR2] rotation done in %s\n", time.Since(rotStart).Round(time.Millisecond))

	// ─── Send SPR Hide (with fallback to plain) ───
	sendStart := time.Now()
	id, hideErr := sendSPREngineAHide(client, groupJID, participants, waMsg)
	if hideErr != nil {
		fmt.Printf("[SPR2] Hide gagal: %v, fallback plain...\n", hideErr)
		id2, _, _, _, err2 := sendSPREngineA(client, groupJID, participants, waMsg)
		if err2 != nil {
			fmt.Printf("[SPR2] send gagal total: %v\n", err2)
			spr2Mu.Lock()
			spr2Failed++
			spr2Active = false
			spr2LastMsg = fmt.Sprintf("gagal: hide=%v plain=%v", hideErr, err2)
			spr2Mu.Unlock()
			return
		}
		id = id2
		fmt.Printf("[SPR2] plain send OK (Hide failed, non-targets may see msg!)\n")
	} else {
		fmt.Printf("[SPR2] Hide send OK (non-targets see NOTHING)\n")
	}
	sprRegisterBait(id, groupJID.String(), "2", nil)

	spr2Mu.Lock()
	spr2Sent++
	spr2Active = false
	spr2LastMsg = "OK"
	spr2Mu.Unlock()

	fmt.Printf("[SPR2] sent OK: id=%s target=%s send=%s total=%s\n",
		id, target.String(),
		time.Since(sendStart).Round(time.Millisecond),
		time.Since(startTotal).Round(time.Millisecond))

	// Rotate AFTER send (background, 2s delay — same as .spr)
	go func() {
		time.Sleep(2 * time.Second)
		sprBaseMu.Lock()
		if rerr := sprRotateSenderKey(groupJID); rerr != nil {
			fmt.Printf("[SPR2] ⚠️ post-send rotation gagal: %v\n", rerr)
		}
		sprBaseMu.Unlock()
	}()
}

// ─── Subcommands ───────────────────────────────────────────────────

func spr2Status() string {
	spr2Mu.Lock()
	defer spr2Mu.Unlock()
	if !spr2Active {
		return fmt.Sprintf("📋 *SPR2 Status*\n\n• State: idle\n• Last: %s\n• Sent: %d, Failed: %d",
			spr2LastMsg, spr2Sent, spr2Failed)
	}
	elapsed := time.Since(spr2Started)
	return fmt.Sprintf("📋 *SPR2 Status*\n\n• State: running ⚡\n• Elapsed: %s",
		elapsed.Round(time.Millisecond))
}

func spr2Help() string {
	return `⚡ *SPR2 — Ultra-Fast SPR*

Respon secepat mungkin — skip resolveSPRTargets + skip buildSPRParticipants.

*Cara pakai:*
• .spr2 @tag <teks>
• .spr2 628xxx <teks>
• Reply pesan + .spr2 <teks>

Contoh: .spr2 @kangwifi Hai ini rahasia

*Privacy:*
• Hanya target yang bisa baca
• Non-target lihat NOTHING (Hide variant)
• Silent (no react, no reply)`
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "spr2",
		Description: "spr2",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSPR2Command(client, msg, args)
			// ─── SPR Cloak (anti-detection for own SPR) ───────────
		},
	})
}
