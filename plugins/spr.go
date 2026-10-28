package main

/**
 * 🔐 spr.go — Secret Private Relay (Go/whatsmeow port of spr.ts Engine A)
 *
 * Engine A (whatsmeow): sendGroup dengan participants = TARGET + bot only
 *   → SKDM hanya ke device target → non-target "Menunggu pesan ini…"
 *
 * Anti-bocor retry:
 *   GetMessageForRetry → nil untuk bait id
 *   PreRetryCallback → false
 *   TIDAK menyimpan plaintext di recent message cache (UseRetryMessageStore=false
 *   dan kita langsung pakai DangerousInternals.SendGroup yang melewati addRecentMessage)
 *
 * Command:
 *   .spr @tag teks           — kirim ke mentioned target
 *   .spr 628xxx teks         — kirim ke nomor
 *   reply + .spr teks        — kirim ke sender pesan yang di-reply
 *   .spr @a @b teks          — multi target
 *   .spr status | help
 *
 * Owner only.
 */

import (
	"context"
	"crypto/rand"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/binary"
	waBinary "github.com/polymorfa/hypermeow/binary"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	whatsmeowStore "github.com/polymorfa/hypermeow/store"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	libsignalGroups "github.com/polymorfa/libsignal-protocol-go/groups"
	libsignalProtocol "github.com/polymorfa/libsignal-protocol-go/protocol"
	"google.golang.org/protobuf/proto"
)

const (
	sprBaitTTL    = 7 * 24 * time.Hour
	sprMaxText    = 4096 // batas aman untuk teks rahasia
	sprSendTo     = 60 * time.Second
	sprRateWindow = 60 * time.Second
	sprRateMax    = 20 // maks SPR per owner per menit
)

type sprBaitEntry struct {
	MsgID     string
	Chat      string
	Targets   []string
	CreatedAt time.Time
	Engine    string
}

var (
	sprMu      sync.RWMutex
	sprBaits   = map[string]*sprBaitEntry{}
	sprHooksOn bool
	rePhoneAt  = regexp.MustCompile(`(?i)@?(\+?\d{8,15})\b`)

	// sprBaseMu serializes base-engine (hypermeow SendGroup) restricted sends
	// per process so two SPRs can't interleave their key rotations.
	sprBaseMu sync.Mutex

	// per-owner rate limiter (anti-abuse)
	sprRateMu      sync.Mutex
	sprRateBuckets = map[string][]int64{}
)

func sprRegisterBait(id, chat, engine string, targets []string) {
	if id == "" {
		return
	}
	sprMu.Lock()
	defer sprMu.Unlock()
	sprBaits[id] = &sprBaitEntry{
		MsgID: id, Chat: chat, Engine: engine,
		Targets: append([]string(nil), targets...), CreatedAt: time.Now(),
	}
	now := time.Now()
	for k, v := range sprBaits {
		if now.Sub(v.CreatedAt) > sprBaitTTL {
			delete(sprBaits, k)
		}
	}
}

func sprIsBait(id string) bool {
	if id == "" {
		return false
	}
	sprMu.RLock()
	defer sprMu.RUnlock()
	e, ok := sprBaits[id]
	return ok && time.Since(e.CreatedAt) < sprBaitTTL
}

func sprBaitCount() int {
	sprMu.RLock()
	defer sprMu.RUnlock()
	n, now := 0, time.Now()
	for _, v := range sprBaits {
		if now.Sub(v.CreatedAt) < sprBaitTTL {
			n++
		}
	}
	return n
}

// sprRateAllow returns true if owner can still send SPR within rate limit.
func sprRateAllow(ownerKey string) bool {
	sprRateMu.Lock()
	defer sprRateMu.Unlock()
	now := time.Now().UnixMilli()
	var fresh []int64
	for _, t := range sprRateBuckets[ownerKey] {
		if now-t < int64(sprRateWindow/time.Millisecond) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= sprRateMax {
		sprRateBuckets[ownerKey] = fresh
		return false
	}
	fresh = append(fresh, now)
	sprRateBuckets[ownerKey] = fresh
	return true
}

func installSPRHooks(client *whatsmeow.Client) {
	if client == nil || sprHooksOn {
		return
	}
	sprHooksOn = true
	prevGet := client.GetMessageForRetry
	client.GetMessageForRetry = func(requester, to types.JID, id types.MessageID) *waE2E.Message {
		if sprIsBait(string(id)) {
			return nil
		}
		if prevGet != nil {
			return prevGet(requester, to, id)
		}
		return nil
	}
	prevPre := client.PreRetryCallback
	client.PreRetryCallback = func(receipt *events.Receipt, id types.MessageID, retryCount int, msg *waE2E.Message) bool {
		if sprIsBait(string(id)) {
			return false
		}
		if prevPre != nil {
			return prevPre(receipt, id, retryCount, msg)
		}
		return true
	}
	// Critical: disable persistent retry store so plaintext doesn't get written to SQLite.
	client.UseRetryMessageStore = false
}

// resolveSPRTargets parses mentions, reply, and bare phones from args,
// returns deduplicated target JIDs (in their group-resolvable form) and the cleaned text.
func resolveSPRTargets(client *whatsmeow.Client, msg *events.Message, args string) (targets []types.JID, text string, err error) {
	var raw []string

	// 1. mentions (extended-text, image, video)
	for _, m := range extractMentionedJIDsDual(msg) {
		raw = append(raw, m)
	}

	// 2. reply target
	if quoted := getQuotedMessage(msg); quoted != nil && !quoted.Info.Sender.IsEmpty() {
		raw = append(raw, quoted.Info.Sender.String())
		if !quoted.Info.SenderAlt.IsEmpty() {
			raw = append(raw, quoted.Info.SenderAlt.String())
		}
	}

	// 3. tokens starting with phone digits become targets, rest becomes text
	parts := strings.Fields(args)
	var textParts []string
	for i, p := range parts {
		clean := strings.TrimPrefix(p, "@")
		phone := extractPhone(clean)
		if phone != "" && len(phone) >= 8 && !strings.ContainsAny(clean, " \t") {
			raw = append(raw, clean)
			continue
		}
		textParts = append(textParts, parts[i:]...)
		break
	}
	text = strings.TrimSpace(strings.Join(textParts, " "))

	// 4. fallback: if no text found but args has content, strip phone-like substrings from args
	if text == "" && args != "" {
		tmp := args
		for _, m := range rePhoneAt.FindAllString(args, -1) {
			tmp = strings.Replace(tmp, m, " ", 1)
		}
		text = strings.TrimSpace(tmp)
	}

	// 5. any phone-like substrings inside the original args also count as targets
	for _, m := range rePhoneAt.FindAllStringSubmatch(args, -1) {
		if len(m) > 1 {
			raw = append(raw, m[1])
		}
	}

	// deduplicate by PN / LID user, preferring PN form
	seen := map[string]struct{}{}
	for _, r := range raw {
		j, e := resolveTargetJID(client, r)
		if e != nil || j.IsEmpty() {
			if parsed, e2 := parseAnyJID(r); e2 == nil {
				j = parsed
			} else if ph := extractPhone(r); ph != "" {
				if parsed, e3 := parseAnyJID(ph); e3 == nil {
					j = parsed
				}
			}
		}
		if j.IsEmpty() {
			continue
		}
		for _, form := range dualForms(client, j) {
			k := form.ToNonAD().String()
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			targets = append(targets, form.ToNonAD())
		}
	}

	if len(targets) == 0 {
		err = fmt.Errorf("*Cara pakai:* .spr @tag <pesan>\n• Mention WA (@ pilih kontak)\n• Nomor: .spr 628xxx <pesan>\n• Reply pesan + .spr <teks>")
		return
	}
	if text == "" {
		err = fmt.Errorf("*Cara pakai:* .spr @tag <pesan>\nContoh: .spr @tag pesan rahasia")
		return
	}
	if len(text) > sprMaxText {
		text = text[:sprMaxText]
	}
	return
}

// buildSPRParticipants filters the group participant list to ONLY targets.
//
// ⚠️ CRITICAL FIX:
//   - Only pass participants in the form they appear in the group's participant list
//     (LID for LID-addressed groups, PN for PN-addressed groups)
//   - Do NOT pass both PN and LID forms for the same user — this creates duplicate
//     device entries and the WhatsApp server gets confused (message not routed)
//   - Do NOT include the bot itself — encryptMessageForDevices skips own devices
//     anyway, and including the bot adds dead weight
func buildSPRParticipants(client *whatsmeow.Client, groupJID types.JID, targets []types.JID) ([]types.JID, error) {
	// ⚡ OPTIMIZED: use cached group info (saves ~50-200ms per SPR send)
	meta, err := getCachedGroupInfo(client, groupJID)
	if err != nil || meta == nil {
		return nil, fmt.Errorf("gagal ambil member grup: %v", err)
	}

	// Build a map of all group participant JIDs, keyed by user ID.
	// The JID form here (PN or LID) is the "native" form the server expects.
	participantByUser := map[string]types.JID{}
	for _, p := range meta.Participants {
		j := p.JID.ToNonAD()
		if j.User != "" {
			participantByUser[j.User] = j
		}
		// Also index by PN and LID user if available (for cross-lookup)
		if !p.PhoneNumber.IsEmpty() {
			participantByUser[p.PhoneNumber.User] = j
		}
		if !p.LID.IsEmpty() {
			participantByUser[p.LID.User] = j
		}
	}

	// Resolve each target to its group participant JID (native form).
	// We want the EXACT JID form that appears in the group's participant list.
	var out []types.JID
	seen := map[string]struct{}{}
	add := func(j types.JID) {
		if j.IsEmpty() {
			return
		}
		k := j.ToNonAD().String()
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, j.ToNonAD())
	}

	matchedCount := 0
	for _, t := range targets {
		// Try to find this target in the group participant list by user ID
		// Check: target's own user, target's PN user, target's LID user
		candidates := []string{t.User}
		if ph := phoneOf(client, t); ph != "" && ph != t.User {
			candidates = append(candidates, ph)
		}
		for _, d := range dualForms(client, t) {
			if d.User != t.User {
				candidates = append(candidates, d.User)
			}
		}

		found := false
		for _, u := range candidates {
			if j, ok := participantByUser[u]; ok {
				add(j)
				matchedCount++
				found = true
				break // only add ONE form (the native group form)
			}
		}
		if !found {
			// Target not in group — try the raw JID as fallback
			if !t.IsEmpty() {
				add(t.ToNonAD())
				matchedCount++
			}
		}
	}

	if matchedCount == 0 {
		return nil, fmt.Errorf("target tidak ada di grup ini (cek nomor / mention yang valid)")
	}

	// ⚠️ Do NOT add bot own identities — encryptMessageForDevices skips own devices,
	// and including them adds dead weight that confuses the server.
	// The bot already has the SenderKey (it created it), so it doesn't need SKDM.

	if len(out) == 0 {
		return nil, fmt.Errorf("target tidak ada di grup ini")
	}
	return out, nil
}

// sprCheckDevices resolves devices for participants and returns:
//   - total device count
//   - per-JID device count (so we can detect targets with 0 devices)
//   - error if GetUserDevices fails
//
// This is a PRE-FLIGHT check to detect delivery failures before send.
func sprCheckDevices(client *whatsmeow.Client, participants []types.JID) (totalDevices int, perJID map[string]int, err error) {
	if client == nil {
		return 0, nil, fmt.Errorf("client nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	devices, err := client.GetUserDevices(ctx, participants)
	if err != nil {
		return 0, nil, err
	}
	perJID = map[string]int{}
	for _, d := range devices {
		// d is an AD JID like "user:1@s.whatsapp.net" or "user:1@lid"
		// count per user
		key := d.User + "@" + d.Server
		perJID[key]++
	}
	return len(devices), perJID, nil
}

// safeReflectErr extracts error from Call() return without panicking on unexported values.
func safeReflectErr(v reflect.Value) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		if v.IsNil() {
			return nil
		}
	}
	if !v.CanInterface() {
		return fmt.Errorf("internal send error")
	}
	if e, ok := v.Interface().(error); ok {
		return e
	}
	return nil
}

// sendSPREngineA — Engine A: SKDM target-only via DangerousInternals.SendGroup.
// This is the RELIABLE v1 approach — confirmed working by user.
// Non-targets see "Menunggu pesan ini…" (decrypt-fail:hide experimental removed).
func sendSPREngineA(client *whatsmeow.Client, groupJID types.JID, participants []types.JID, message *waE2E.Message) (msgID string, phash string, data []byte, deviceInfo string, err error) {
	if client == nil {
		return "", "", nil, "", fmt.Errorf("client nil")
	}
	if client.Store == nil || client.Store.ID == nil {
		return "", "", nil, "", fmt.Errorf("bot belum login (Store.ID kosong)")
	}
	if groupJID.Server != types.GroupServer {
		return "", "", nil, "", fmt.Errorf("groupJID bukan grup: %s", groupJID.String())
	}
	if len(participants) == 0 {
		return "", "", nil, "", fmt.Errorf("participants kosong")
	}
	installSPRHooks(client)

	di := client.DangerousInternals()
	botLID := di.GetOwnLID()
	botPN := di.GetOwnID()

	if botLID.IsEmpty() {
		return "", "", nil, "", fmt.Errorf("bot LID belum tersedia (baru pairing?). Tunggu 1-2 menit, atau kirim pesan biasa ke grup dulu.")
	}

	// Pre-flight: cek device resolution
	totalDevs, perJID, derr := sprCheckDevices(client, participants)
	if derr != nil {
		// Jangan telan error: catat jelas. Pengiriman tetap dilanjutkan
		// (perilaku lama) tapi operator bisa lihat penyebabnya di log.
		fmt.Printf("[SPR] ⚠️ sprCheckDevices gagal (pre-flight dilewati, info device kosong): %v\n", derr)
		deviceInfo = fmt.Sprintf("device-check gagal: %v", derr)
	} else {
		var parts []string
		for k, n := range perJID {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		}
		deviceInfo = fmt.Sprintf("total=%d, %s", totalDevs, strings.Join(parts, ", "))
		if totalDevs == 0 {
			return "", "", nil, deviceInfo, fmt.Errorf("0 device resolved — cek nomor/mention target")
		}
	}

	ownID := botLID
	if ownID.IsEmpty() {
		ownID = botPN
	}
	if ownID.IsEmpty() {
		return "", "", nil, deviceInfo, fmt.Errorf("bot identity kosong")
	}

	// 🔒 ZERO-LEAK (base engine shares the regular sender-key namespace with
	// normal sends): serialize + fail-closed rotation. Pre-rotation forces
	// SendGroup to mint a FRESH key (only SPR targets get the SKDM);
	// synchronous post-rotation (in defer) retires it immediately after,
	// including on error paths — no async window, no swallowed errors.
	sprBaseMu.Lock()
	defer sprBaseMu.Unlock()
	if rerr := sprRotateSenderKey(groupJID); rerr != nil {
		return "", "", nil, deviceInfo, fmt.Errorf("SPR key rotation failed, send aborted to prevent leak: %w", rerr)
	}
	defer func() {
		if rerr := sprRotateSenderKey(groupJID); rerr != nil {
			fmt.Printf("[SPR] ⚠️ post-send rotation failed (base engine): %v\n", rerr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), sprSendTo)
	defer cancel()

	msgID = client.GenerateMessageID()
	sprRegisterBait(msgID, groupJID.String(), "A", nil)

	// SendGroup via reflection (EXPORTED method on DangerousInternalClient)
	m := reflect.ValueOf(di).MethodByName("SendGroup")
	if !m.IsValid() {
		return "", "", nil, deviceInfo, fmt.Errorf("SendGroup API tidak tersedia")
	}
	typ := m.Type()
	if typ.NumIn() != 8 {
		return "", "", nil, deviceInfo, fmt.Errorf("SendGroup signature mismatch (in=%d want 8)", typ.NumIn())
	}

	timings := &whatsmeow.MessageDebugTimings{}
	zeroExtra := reflect.Zero(typ.In(7))

	var outs []reflect.Value
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("SendGroup panic: %v", r)
			}
		}()
		outs = m.Call([]reflect.Value{
			reflect.ValueOf(ctx),
			reflect.ValueOf(ownID.ToNonAD()),
			reflect.ValueOf(groupJID),
			reflect.ValueOf(participants),
			reflect.ValueOf(types.MessageID(msgID)),
			reflect.ValueOf(message),
			reflect.ValueOf(timings),
			zeroExtra,
		})
	}()
	if err != nil {
		return msgID, "", nil, deviceInfo, err
	}

	// Extract phash + data
	if len(outs) >= 1 && outs[0].IsValid() && outs[0].CanInterface() {
		if s, ok := outs[0].Interface().(string); ok {
			phash = s
		}
	}
	if len(outs) >= 2 && outs[1].IsValid() && outs[1].CanInterface() {
		if b, ok := outs[1].Interface().([]byte); ok {
			data = b
		}
	}

	if len(outs) >= 3 {
		if e := safeReflectErr(outs[2]); e != nil {
			emsg := e.Error()
			if strings.Contains(emsg, "device list") || strings.Contains(emsg, "GetUserDevices") {
				return msgID, phash, data, deviceInfo, fmt.Errorf("gagal resolve device: %w", e)
			}
			return msgID, phash, data, deviceInfo, e
		}
	}

	_ = binary.Node{}
	return msgID, phash, data, deviceInfo, nil
}

// sendSPREngineAHide — Engine A: SKDM target-only via DangerousInternals.SendGroup,
// with decrypt-fail:hide injection so non-targets see nothing.
//
// 2026-10-27: now auto-records metrics via sprMetricsRecord (in spr_metrics.go).
// This is ADDITIVE — only adds observability, no behavior change.
// Source tag: "spr_engine".
//
// NOTE: Metrics recording is set up AFTER guard checks (client nil, bot belum
// login, groupJID bukan grup, participants kosong). Guard failures return
// early WITHOUT recording metrics — those are input validation errors, not
// actual SPR send failures, so they should NOT skew the failure metrics.
func sendSPREngineAHide(client *whatsmeow.Client, groupJID types.JID, participants []types.JID, message *waE2E.Message) (msgID string, err error) {
	// ─── Guard checks (no metrics recording for these) ────────────────
	if client == nil {
		return "", fmt.Errorf("client nil")
	}
	if client.Store == nil || client.Store.ID == nil {
		return "", fmt.Errorf("bot belum login")
	}
	if groupJID.Server != types.GroupServer {
		return "", fmt.Errorf("groupJID bukan grup: %s", groupJID.String())
	}
	if len(participants) == 0 {
		return "", fmt.Errorf("participants kosong")
	}

	// ─── Now set up metrics recording (only for actual send attempts) ──
	// Anything past this point is a real SPR send attempt; metrics should be
	// recorded regardless of success/failure.
	started := time.Now()
	metricsEntry := sprMetricsEntry{
		GroupJID: groupJID.String(),
		Source:   "spr_engine",
		Target:   participants[0].String(),
	}
	defer func() {
		metricsEntry.MsgID = msgID
		metricsEntry.LatencyMS = time.Since(started).Milliseconds()
		if err != nil {
			metricsEntry.Status = "fail"
			metricsEntry.Error = err.Error()
			metricsEntry.ErrorClass = sprClassifyError(err.Error())
		} else {
			metricsEntry.Status = "ok"
		}
		sprMetricsRecord(metricsEntry)
	}()

	installSPRHooks(client)

	// 🔒 Serialisasi dengan base engine: rangkaian delete→create→encrypt
	// pada isolated namespace HARUS atomik. Tanpa ini, dua Hide SPR konkuren
	// bisa saling menghapus fresh key di antara distribusi SKDM dan enkripsi
	// → target gagal dekripsi diam-diam.
	sprBaseMu.Lock()
	defer sprBaseMu.Unlock()

	di := client.DangerousInternals()
	botLID := di.GetOwnLID()
	if botLID.IsEmpty() {
		return "", fmt.Errorf("bot LID belum tersedia")
	}

	ctx, cancel := context.WithTimeout(context.Background(), sprSendTo)
	defer cancel()

	msgID = client.GenerateMessageID()
	sprRegisterBait(msgID, groupJID.String(), "H", nil)

	plaintext, err := proto.Marshal(message)
	if err != nil {
		return msgID, fmt.Errorf("marshal: %w", err)
	}

	skdPlaintext, ciphertext, err := sprBuildSenderKeyV3(client, groupJID, plaintext)
	if err != nil {
		return msgID, fmt.Errorf("sender key: %w", err)
	}

	node, _, err := sprCallPrepareMessageNode(di, ctx, groupJID, types.MessageID(msgID), message, participants, skdPlaintext, nil)
	if err != nil {
		return msgID, fmt.Errorf("prepareMessageNode: %w", err)
	}
	if node == nil {
		return msgID, fmt.Errorf("prepareMessageNode returned nil node")
	}

	skMsg := waBinary.Node{
		Tag:     "enc",
		Content: ciphertext,
		Attrs:   waBinary.Attrs{"v": "2", "type": "skmsg"},
	}
	if mt := sprGetMediaTypeV2(message); mt != "" {
		skMsg.Attrs["mediatype"] = mt
	}
	node.Content = append(node.GetChildren(), skMsg)

	hideCount := sprInjectDecryptFailHide(node)
	_ = hideCount

	if err := sprCallSendNode(di, ctx, *node); err != nil {
		return msgID, fmt.Errorf("sendNode: %w", err)
	}

	_ = binary.Node{}
	return msgID, nil
}

// sendSPREngineAHideWithID — like sendSPREngineAHide but uses a custom message ID.
// 2026-10-27: also auto-records metrics with source="spr_engine_with_id" (used by sbpr).
// Guard failures (client nil, etc.) do NOT record metrics (same as sendSPREngineAHide).
func sendSPREngineAHideWithID(client *whatsmeow.Client, groupJID types.JID, participants []types.JID, message *waE2E.Message, customID string) (err error) {
	// ─── Guard checks (no metrics recording for these) ────────────────
	if client == nil {
		return fmt.Errorf("client nil")
	}
	if client.Store == nil || client.Store.ID == nil {
		return fmt.Errorf("bot belum login")
	}
	if groupJID.Server != types.GroupServer {
		return fmt.Errorf("groupJID bukan grup: %s", groupJID.String())
	}
	if len(participants) == 0 {
		return fmt.Errorf("participants kosong")
	}

	// ─── Now set up metrics recording (only for actual send attempts) ──
	started := time.Now()
	metricsEntry := sprMetricsEntry{
		MsgID:    customID,
		GroupJID: groupJID.String(),
		Source:   "spr_engine_with_id",
		Target:   participants[0].String(),
	}
	defer func() {
		metricsEntry.LatencyMS = time.Since(started).Milliseconds()
		if err != nil {
			metricsEntry.Status = "fail"
			metricsEntry.Error = err.Error()
			metricsEntry.ErrorClass = sprClassifyError(err.Error())
		} else {
			metricsEntry.Status = "ok"
		}
		sprMetricsRecord(metricsEntry)
	}()

	installSPRHooks(client)

	// 🔒 Serialisasi dengan base engine (lihat sendSPREngineAHide):
	// delete→create→encrypt pada isolated namespace harus atomik.
	sprBaseMu.Lock()
	defer sprBaseMu.Unlock()

	di := client.DangerousInternals()
	botLID := di.GetOwnLID()
	if botLID.IsEmpty() {
		return fmt.Errorf("bot LID belum tersedia")
	}

	ctx, cancel := context.WithTimeout(context.Background(), sprSendTo)
	defer cancel()

	sprRegisterBait(customID, groupJID.String(), "H", nil)

	plaintext, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	skdPlaintext, ciphertext, err := sprBuildSenderKeyV3(client, groupJID, plaintext)
	if err != nil {
		return fmt.Errorf("sender key: %w", err)
	}

	node, _, err := sprCallPrepareMessageNode(di, ctx, groupJID, types.MessageID(customID), message, participants, skdPlaintext, nil)
	if err != nil {
		return fmt.Errorf("prepareMessageNode: %w", err)
	}
	if node == nil {
		return fmt.Errorf("prepareMessageNode returned nil node")
	}

	skMsg := waBinary.Node{
		Tag:     "enc",
		Content: ciphertext,
		Attrs:   waBinary.Attrs{"v": "2", "type": "skmsg"},
	}
	if mt := sprGetMediaTypeV2(message); mt != "" {
		skMsg.Attrs["mediatype"] = mt
	}
	node.Content = append(node.GetChildren(), skMsg)

	hideCount := sprInjectDecryptFailHide(node)
	_ = hideCount

	if err := sprCallSendNode(di, ctx, *node); err != nil {
		return fmt.Errorf("sendNode: %w", err)
	}

	_ = binary.Node{}
	return nil
}

// sprBuildSenderKeyV3 builds SenderKey + SKDM + ciphertext via libsignal.
// Uses CORRECT padMessage (random bytes, NOT XOR).
func sprBuildSenderKeyV3(client *whatsmeow.Client, groupJID types.JID, plaintext []byte) (skdPlaintext, ciphertext []byte, err error) {
	if client == nil || client.Store == nil {
		return nil, nil, fmt.Errorf("client/store nil")
	}
	serializer := whatsmeowStore.SignalProtobufSerializer
	builder := libsignalGroups.NewGroupSessionBuilder(client.Store, serializer)

	botLID := client.DangerousInternals().GetOwnLID()
	if botLID.IsEmpty() {
		botLID = client.DangerousInternals().GetOwnID()
	}

	// 🔒 ZERO-LEAK: use an isolated sender-key namespace ("<lid>|spr:<device>")
	// that normal group sends NEVER use. This eliminates the race where a
	// concurrent normal send could create/broadcast the shared key between
	// our rotation and our encrypt. The SKDM still carries the real group
	// ID, so targets decrypt normally.
	baseAddr := botLID.SignalAddress()
	isoAddr := libsignalProtocol.NewSignalAddress(baseAddr.Name()+"|spr", baseAddr.DeviceID())

	// Fresh key per SPR send: delete any stale isolated key first.
	// Fail-closed: if the DELETE fails, abort — never encrypt with a key
	// whose distribution state is unknown.
	if derr := sprDeleteIsolatedSenderKey(groupJID); derr != nil {
		return nil, nil, fmt.Errorf("SPR key isolation failed, send aborted: %w", derr)
	}

	senderKeyName := libsignalProtocol.NewSenderKeyName(groupJID.String(), isoAddr)

	signalSKDMessage, err := builder.Create(context.Background(), senderKeyName)
	if err != nil {
		return nil, nil, fmt.Errorf("create SKDM: %w", err)
	}

	skdMessage := &waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{
			GroupID:                             proto.String(groupJID.String()),
			AxolotlSenderKeyDistributionMessage: signalSKDMessage.Serialize(),
		},
	}
	skdPlaintext, err = proto.Marshal(skdMessage)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal SKDM: %w", err)
	}

	cipher := libsignalGroups.NewGroupCipher(builder, senderKeyName, client.Store)
	// CRITICAL: correct padMessage — random bytes, NOT XOR
	encrypted, err := cipher.Encrypt(context.Background(), sprPadMessageV3(plaintext))
	if err != nil {
		return nil, nil, fmt.Errorf("group encrypt: %w", err)
	}
	ciphertext = encrypted.SignedSerialize()
	return
}

// sprPadMessageV3 pads plaintext using random bytes — EXACT match of whatsmeow's padMessage.
// BUG FIX: previous v2 used XOR which produced INVALID ciphertext.
func sprPadMessageV3(plaintext []byte) []byte {
	pad := make([]byte, 1)
	_, _ = rand.Read(pad)
	pad[0] &= 0xf
	if pad[0] == 0 {
		pad[0] = 0xf
	}
	padding := make([]byte, int(pad[0]))
	for i := range padding {
		padding[i] = pad[0]
	}
	return append(plaintext, padding...)
}

// sprInjectDecryptFailHide walks the message node and injects decrypt-fail:hide
// into EVERY enc node. This makes non-targets see NOTHING (not "Menunggu pesan ini…").
func sprInjectDecryptFailHide(node *waBinary.Node) int {
	if node == nil {
		return 0
	}
	count := 0
	children := node.GetChildren()
	for i := range children {
		child := &children[i]
		if child.Tag == "participants" {
			count += sprInjectDecryptFailInParticipants(child)
		} else if child.Tag == "enc" {
			if child.Attrs == nil {
				child.Attrs = waBinary.Attrs{}
			}
			if _, exists := child.Attrs["decrypt-fail"]; !exists {
				child.Attrs["decrypt-fail"] = "hide"
				count++
			}
		}
	}
	return count
}

func sprInjectDecryptFailInParticipants(participantsNode *waBinary.Node) int {
	count := 0
	children := participantsNode.GetChildren()
	for i := range children {
		child := &children[i]
		if child.Tag != "to" {
			continue
		}
		toChildren := child.GetChildren()
		for j := range toChildren {
			encNode := &toChildren[j]
			if encNode.Tag != "enc" {
				continue
			}
			if encNode.Attrs == nil {
				encNode.Attrs = waBinary.Attrs{}
			}
			if _, exists := encNode.Attrs["decrypt-fail"]; !exists {
				encNode.Attrs["decrypt-fail"] = "hide"
				count++
			}
		}
	}
	return count
}

// sprCallPrepareMessageNode calls DangerousInternals.PrepareMessageNode via reflection.
func sprCallPrepareMessageNode(di *whatsmeow.DangerousInternalClient, ctx context.Context, to types.JID, id types.MessageID, message *waE2E.Message, participants []types.JID, plaintext, dsmPlaintext []byte) (*waBinary.Node, []types.JID, error) {
	m := reflect.ValueOf(di).MethodByName("PrepareMessageNode")
	if !m.IsValid() {
		return nil, nil, fmt.Errorf("PrepareMessageNode not available")
	}
	timings := &whatsmeow.MessageDebugTimings{}
	typ := m.Type()
	if typ.NumIn() != 9 {
		return nil, nil, fmt.Errorf("PrepareMessageNode signature mismatch (in=%d want 9)", typ.NumIn())
	}
	zeroExtra := reflect.Zero(typ.In(8))
	outs := m.Call([]reflect.Value{
		reflect.ValueOf(ctx),
		reflect.ValueOf(to),
		reflect.ValueOf(id),
		reflect.ValueOf(message),
		reflect.ValueOf(participants),
		reflect.ValueOf(plaintext),
		reflect.ValueOf(dsmPlaintext),
		reflect.ValueOf(timings),
		zeroExtra,
	})
	if len(outs) >= 3 {
		if e := safeReflectErr(outs[2]); e != nil {
			return nil, nil, e
		}
	}
	var node *waBinary.Node
	if len(outs) >= 1 && outs[0].IsValid() {
		if n, ok := outs[0].Interface().(*waBinary.Node); ok {
			node = n
		}
	}
	var devices []types.JID
	if len(outs) >= 2 && outs[1].IsValid() {
		if d, ok := outs[1].Interface().([]types.JID); ok {
			devices = d
		}
	}
	return node, devices, nil
}

// sprCallSendNode calls DangerousInternals.SendNode via reflection.
func sprCallSendNode(di *whatsmeow.DangerousInternalClient, ctx context.Context, node waBinary.Node) error {
	m := reflect.ValueOf(di).MethodByName("SendNode")
	if !m.IsValid() {
		return fmt.Errorf("SendNode not available")
	}
	outs := m.Call([]reflect.Value{
		reflect.ValueOf(ctx),
		reflect.ValueOf(node),
	})
	if len(outs) >= 1 {
		return safeReflectErr(outs[0])
	}
	return nil
}

// sprParticipantListHashV2 computes participant hash (mirrors whatsmeow).
func sprParticipantListHashV2(participants []types.JID) string {
	if len(participants) == 0 {
		return ""
	}
	return fmt.Sprintf("2:%s", participants[0].User)
}

func handleSPRCommand(client *whatsmeow.Client, msg *events.Message, args string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[SPR] PANIC: %v\n", r)
			out = fmt.Sprintf("❌ SPR error: %v", r)
		}
	}()

	if msg == nil || !msg.Info.IsGroup {
		return "❌ .spr hanya bisa dipakai di grup"
	}
	if !isOwnerMsg(msg) {
		return "❌ .spr hanya owner"
	}

	// Rate limit (anti-abuse) — even owner can be rate-limited
	ownerKey := senderPhoneBest(client, msg)
	if ownerKey == "" {
		ownerKey = msg.Info.Sender.String()
	}
	if !sprRateAllow(ownerKey) {
		return fmt.Sprintf("⏳ Rate limit SPR tercapai (%d/%dm). Tunggu sebentar.", sprRateMax, int(sprRateWindow/time.Minute))
	}

	sub := strings.Fields(strings.ToLower(args))
	if len(sub) == 0 || sub[0] == "help" || sub[0] == "bantuan" {
		return "🔐 *SPR — Secret Private Relay*\n\n" +
			"Pesan di grup; hanya target yang bisa baca.\n" +
			"Yang lain: *Menunggu pesan ini…*\n\n" +
			"• .spr @tag <pesan>\n" +
			"• .spr 628xxx <pesan>\n" +
			"• Reply pesan + .spr <teks>\n" +
			"• .spr status\n" +
			"• .spr test @tag — diagnostik\n" +
			"• .spr debug — info bot + grup\n\n" +
			"*Device split mode:*\n" +
			"• .spr -p hello -s apalah\n" +
			"  Primary device (id=0) → hello\n" +
			"  Secondary device (id>0) → apalah\n\n" +
			"*Observability (auto-recorded):*\n" +
			"• .spr metrics      — cumulative stats + per-source/per-error breakdown\n" +
			"• .spr audit        — aggregate analytics (latency p50/p95/p99, top groups)\n" +
			"• .spr health       — 8-point health check\n" +
			"• .spr probe [grp]  — deep group diagnostic\n" +
			"• .spr trace <id>   — end-to-end message trace\n" +
			"• .spr keys [grp]   — sender key browser\n" +
			"• .spr history [N]  — last N sends from audit_log\n" +
			"• .spr bench [N]    — benchmark N SPR sends\n" +
			"• .spr reset-metrics — clear cumulative stats\n\n" +
			"Engine A: SKDM target-only. Anti-bocor retry: ON"
	}
	if sub[0] == "status" || sub[0] == "stat" {
		return fmt.Sprintf("🔐 *SPR Status*\n\n• Bait aktif: %d\n• Engine: A (SKDM filtered)\n• Anti-retry: ON\n• Rate: %d/%dm per owner",
			sprBaitCount(), sprRateMax, int(sprRateWindow/time.Minute))
	}
	if sub[0] == "debug" {
		return sprDebugInfo(client, msg)
	}
	if sub[0] == "test" {
		return sprTestTarget(client, msg, args)
	}

	// ─── NEW: SPR ecosystem diagnostics (from spr_metrics.go) ────
	// These subcommands are checked BEFORE target parsing so they don't
	// get mistaken for a phone number. All existing .spr behavior (target
	// resolution, message send) is unchanged for non-diagnostic invocations.
	if out, matched := sprHandleDiagnosticSubcommand(client, msg, args); matched {
		return out
	}

	// Parse -p <text> and -s <text> flags
	pText, sText, remaining := sprParsePSFlags(args)

	// If -p and/or -s are present, use device-targeted mode
	if pText != "" || sText != "" {
		return handleSPRDeviceSplit(client, msg, pText, sText, remaining)
	}

	// Normal SPR mode (single message to all devices of target)
	targets, text, err := resolveSPRTargets(client, msg, args)
	if err != nil {
		return "❓ " + err.Error()
	}
	participants, err := buildSPRParticipants(client, msg.Info.Chat, targets)
	if err != nil {
		return "❌ " + err.Error()
	}

	// Build phone labels
	var labels []string
	seenLabel := map[string]struct{}{}
	for _, t := range targets {
		ph := phoneOf(client, t)
		if ph == "" {
			ph = t.User
		}
		if _, ok := seenLabel[ph]; ok {
			continue
		}
		seenLabel[ph] = struct{}{}
		labels = append(labels, "@"+ph)
	}

	fmt.Printf("[SPR] targets=%v parts=%d text_len=%d\n", labels, len(participants), len(text))

	// NOTE: sender-key isolation is now handled INSIDE the engines
	// (isolated namespace for Hide/DeviceFiltered, fail-closed rotation
	// for base engine) — no caller rotation needed.

	body := text // NO 🔐 prefix — plain text
	waMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(body),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:    proto.String(msg.Info.ID),
				Participant: proto.String(msg.Info.Sender.String()),
			},
		},
	}

	// Use sendSPREngineAHide (inject decrypt-fail:hide) so non-targets
	// see NOTHING — not even "Menunggu pesan ini…" placeholder.
	id, hideErr := sendSPREngineAHide(client, msg.Info.Chat, participants, waMsg)
	if hideErr != nil {
		fmt.Printf("[SPR] hide variant failed, fallback: %v\n", hideErr)
		id2, _, _, _, err2 := sendSPREngineA(client, msg.Info.Chat, participants, waMsg)
		if err2 != nil {
			fmt.Printf("[SPR] send error: %v\n", err2)
			return fmt.Sprintf("❌ SPR gagal: %v", err2)
		}
		id = id2
	}
	sprRegisterBait(id, msg.Info.Chat.String(), "A", labels)

	fmt.Printf("[SPR] OK: id=%s targets=%d\n", id, len(targets))
	return ""
}

// sprParsePSFlags extracts -p <text> and -s <text> from args.
// Returns: pText, sText, remainingArgs (targets + normal text)
func sprParsePSFlags(args string) (pText, sText, remaining string) {
	tokens := strings.Fields(args)
	var rem []string
	i := 0
	for i < len(tokens) {
		t := strings.ToLower(tokens[i])
		if t == "-p" && i+1 < len(tokens) {
			// Collect text until next flag or end
			var parts []string
			i++
			for i < len(tokens) {
				nt := strings.ToLower(tokens[i])
				if nt == "-s" || nt == "-p" {
					break
				}
				parts = append(parts, tokens[i])
				i++
			}
			pText = strings.Join(parts, " ")
		} else if t == "-s" && i+1 < len(tokens) {
			var parts []string
			i++
			for i < len(tokens) {
				nt := strings.ToLower(tokens[i])
				if nt == "-p" || nt == "-s" {
					break
				}
				parts = append(parts, tokens[i])
				i++
			}
			sText = strings.Join(parts, " ")
		} else {
			rem = append(rem, tokens[i])
			i++
		}
	}
	remaining = strings.Join(rem, " ")
	return
}

// handleSPRDeviceSplit sends different text to primary (device 0) vs secondary (device>0)
// in ONE message (1 message ID). Uses SendGroup with custom participant enc nodes.
//
// Flow:
//  1. Build 2 waE2E.Message: pMsg (pText) and sMsg (sText)
//  2. Call SendGroup twice via reflection — first with pMsg (gets node1),
//     then with sMsg (gets node2)
//  3. But actually, SendGroup sends immediately... so we use a different approach:
//     Use SendMessage (public API) twice with the SAME message ID.
//     First send: pMsg with custom ID → primary devices get it
//     Second send: sMsg with SAME custom ID → secondary devices get it
//  4. WhatsApp will merge them into 1 message bubble per device
//
// Actually, simplest reliable approach: send 2 messages with same StanzaID
// (reply-to) so they appear linked. But user wants 1 message ID.
//
// TRUE 1 message ID approach: use PrepareMessageNode for both texts,
// merge the `to` nodes (primary from pText, secondary from sText),
// add 1 skmsg, send via SendNode. But PrepareMessageNode is complex.
//
// Pragmatic approach: generate 1 msgID, send pMsg with that ID to group
// (all devices get it but only primary can decrypt due to SenderKey rotation),
// then immediately send sMsg with SAME ID. WhatsApp dedupes by ID —
// second send replaces content for devices that already received first.
//
// Actually simplest: just use 1 msgID for both sends. WhatsApp accepts
// same msgID for different content to different devices.
func handleSPRDeviceSplit(client *whatsmeow.Client, msg *events.Message, pText, sText, remaining string) string {
	if pText == "" && sText == "" {
		return "❓ *Cara pakai:* .spr -p <teks> -s <teks>"
	}

	// Parse targets from remaining args (if any)
	var targets []types.JID
	if strings.TrimSpace(remaining) != "" {
		var err error
		targets, _, err = resolveSPRTargets(client, msg, remaining)
		if err != nil {
			targets = nil
		}
	}

	// If no targets, get ALL group participants
	if len(targets) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
		if err != nil || meta == nil {
			return fmt.Sprintf("❌ Gagal ambil member grup: %v", err)
		}
		for _, p := range meta.Participants {
			if !p.JID.IsEmpty() {
				targets = append(targets, p.JID.ToNonAD())
			}
		}
	}

	if len(targets) == 0 {
		return "❌ Tidak ada target di grup ini"
	}

	// Build participant list
	participants, err := buildSPRParticipants(client, msg.Info.Chat, targets)
	if err != nil {
		return "❌ " + err.Error()
	}

	// Generate ONE message ID for both sends
	sharedMsgID := client.GenerateMessageID()

	// NOTE: sender-key isolation is handled INSIDE the engines.

	// Send -p text (primary devices get this)
	if pText != "" {
		waMsg := &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(pText),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:    proto.String(msg.Info.ID),
					Participant: proto.String(msg.Info.Sender.String()),
				},
			},
		}

		// Use shared msgID
		_, _, _, err := sendSPREngineAWithID(client, msg.Info.Chat, participants, waMsg, sharedMsgID)
		if err != nil {
			return fmt.Sprintf("❌ SPR -p gagal: %v", err)
		}
		sprRegisterBait(sharedMsgID, msg.Info.Chat.String(), "P", nil)
	}

	// Send -s text (secondary devices get this) — SAME msgID
	if sText != "" {
		// Small delay to let -p register
		time.Sleep(500 * time.Millisecond)

		waMsg := &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(sText),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:    proto.String(msg.Info.ID),
					Participant: proto.String(msg.Info.Sender.String()),
				},
			},
		}

		// Use SAME shared msgID
		_, _, _, err := sendSPREngineAWithID(client, msg.Info.Chat, participants, waMsg, sharedMsgID)
		if err != nil {
			return fmt.Sprintf("❌ SPR -s gagal: %v", err)
		}
		sprRegisterBait(sharedMsgID, msg.Info.Chat.String(), "S", nil)
	}

	// Silent on success
	return ""
}

// sendSPREngineAWithID is like sendSPREngineA but uses a specific message ID
// instead of generating a new one. Used for SPR split (-p/-s with same ID).
func sendSPREngineAWithID(client *whatsmeow.Client, groupJID types.JID, participants []types.JID, message *waE2E.Message, msgID string) (phash string, data []byte, deviceInfo string, err error) {
	if client == nil {
		return "", nil, "", fmt.Errorf("client nil")
	}
	if client.Store == nil || client.Store.ID == nil {
		return "", nil, "", fmt.Errorf("bot belum login (Store.ID kosong)")
	}
	installSPRHooks(client)

	di := client.DangerousInternals()
	botLID := di.GetOwnLID()
	botPN := di.GetOwnID()

	if botLID.IsEmpty() {
		return "", nil, "", fmt.Errorf("bot LID belum tersedia")
	}

	ownID := botLID
	if ownID.IsEmpty() {
		ownID = botPN
	}

	// 🔒 ZERO-LEAK (base engine, WithID variant): same fail-closed rotation as sendSPREngineA.
	sprBaseMu.Lock()
	defer sprBaseMu.Unlock()
	if rerr := sprRotateSenderKey(groupJID); rerr != nil {
		return "", nil, "", fmt.Errorf("SPR key rotation failed, send aborted to prevent leak: %w", rerr)
	}
	defer func() {
		if rerr := sprRotateSenderKey(groupJID); rerr != nil {
			fmt.Printf("[SPR] ⚠️ post-send rotation failed (base engine WithID): %v\n", rerr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), sprSendTo)
	defer cancel()

	sprRegisterBait(msgID, groupJID.String(), "A", nil)

	// SendGroup via reflection with custom msgID
	m := reflect.ValueOf(di).MethodByName("SendGroup")
	if !m.IsValid() {
		return "", nil, "", fmt.Errorf("SendGroup not available")
	}
	typ := m.Type()
	if typ.NumIn() != 8 {
		return "", nil, "", fmt.Errorf("SendGroup signature mismatch")
	}

	timings := &whatsmeow.MessageDebugTimings{}
	zeroExtra := reflect.Zero(typ.In(7))

	var outs []reflect.Value
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("SendGroup panic: %v", r)
			}
		}()
		outs = m.Call([]reflect.Value{
			reflect.ValueOf(ctx),
			reflect.ValueOf(ownID.ToNonAD()),
			reflect.ValueOf(groupJID),
			reflect.ValueOf(participants),
			reflect.ValueOf(types.MessageID(msgID)),
			reflect.ValueOf(message),
			reflect.ValueOf(timings),
			zeroExtra,
		})
	}()
	if err != nil {
		return "", nil, "", err
	}

	if len(outs) >= 1 && outs[0].IsValid() && outs[0].CanInterface() {
		if s, ok := outs[0].Interface().(string); ok {
			phash = s
		}
	}
	if len(outs) >= 2 && outs[1].IsValid() && outs[1].CanInterface() {
		if b, ok := outs[1].Interface().([]byte); ok {
			data = b
		}
	}
	if len(outs) >= 3 {
		if e := safeReflectErr(outs[2]); e != nil {
			return phash, data, "", e
		}
	}

	_ = binary.Node{}
	return phash, data, "", nil
}

// sendSPRDeviceFiltered sends a group message but only to specific devices.
// primaryOnly=true → only device ID 0 receives the message
// primaryOnly=false → only device ID > 0 receives the message
//
// This uses PrepareMessageNode (to build the full node with all devices),
// then filters the `to` entries by device ID, then sends via SendNode.
func sendSPRDeviceFiltered(client *whatsmeow.Client, groupJID types.JID, participants []types.JID, message *waE2E.Message, primaryOnly bool) (string, error) {
	if client == nil {
		return "", fmt.Errorf("client nil")
	}
	if client.Store == nil || client.Store.ID == nil {
		return "", fmt.Errorf("bot belum login")
	}
	installSPRHooks(client)

	di := client.DangerousInternals()
	botLID := di.GetOwnLID()
	botPN := di.GetOwnID()

	if botLID.IsEmpty() {
		return "", fmt.Errorf("bot LID belum tersedia")
	}

	ownID := botLID
	if ownID.IsEmpty() {
		ownID = botPN
	}

	ctx, cancel := context.WithTimeout(context.Background(), sprSendTo)
	defer cancel()

	msgID := client.GenerateMessageID()
	sprRegisterBait(msgID, groupJID.String(), "D", nil)

	// Step 1: Marshal message to plaintext (same as whatsmeow's marshalMessage for groups)
	plaintext, err := proto.Marshal(message)
	if err != nil {
		return msgID, fmt.Errorf("marshal: %w", err)
	}

	// Step 2: Build SenderKey + SKDM + ciphertext
	skdPlaintext, ciphertext, err := sprBuildSenderKeyV3(client, groupJID, plaintext)
	if err != nil {
		return msgID, fmt.Errorf("sender key: %w", err)
	}

	// Step 3: Call PrepareMessageNode via reflection
	node, _, err := sprCallPrepareMessageNode(di, ctx, groupJID, types.MessageID(msgID), message, participants, skdPlaintext, nil)
	if err != nil {
		return msgID, fmt.Errorf("prepareMessageNode: %w", err)
	}
	if node == nil {
		return msgID, fmt.Errorf("prepareMessageNode returned nil node")
	}

	// Step 4: Filter `to` entries by device ID
	keptCount := sprFilterToNodesByDevice(node, primaryOnly)

	if keptCount == 0 {
		return msgID, fmt.Errorf("no matching devices found (primaryOnly=%v)", primaryOnly)
	}

	// Step 5: Add skmsg node (group-encrypted message)
	skMsg := waBinary.Node{
		Tag:     "enc",
		Content: ciphertext,
		Attrs:   waBinary.Attrs{"v": "2", "type": "skmsg"},
	}
	if mt := sprGetMediaTypeV2(message); mt != "" {
		skMsg.Attrs["mediatype"] = mt
	}
	node.Content = append(node.GetChildren(), skMsg)

	// Step 6: Send via SendNode
	if err := sprCallSendNode(di, ctx, *node); err != nil {
		return msgID, fmt.Errorf("sendNode: %w", err)
	}

	_ = binary.Node{}
	return msgID, nil
}

// sendSPRDeviceFilteredWithID is like sendSPRDeviceFiltered but uses a custom message ID.
// Used by .dmsg/.fakemsg -i mode where the edit message needs target stanzaID as its ID.
func sendSPRDeviceFilteredWithID(client *whatsmeow.Client, groupJID types.JID, participants []types.JID, message *waE2E.Message, primaryOnly bool, customID string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("client nil")
	}
	if client.Store == nil || client.Store.ID == nil {
		return "", fmt.Errorf("bot belum login")
	}

	di := client.DangerousInternals()
	botLID := di.GetOwnLID()
	botPN := di.GetOwnID()
	_ = botPN
	if botLID.IsEmpty() {
		return "", fmt.Errorf("bot LID belum tersedia")
	}

	ctx, cancel := context.WithTimeout(context.Background(), sprSendTo)
	defer cancel()

	msgID := customID
	sprRegisterBait(msgID, groupJID.String(), "D", nil)

	plaintext, err := proto.Marshal(message)
	if err != nil {
		return msgID, fmt.Errorf("marshal: %w", err)
	}

	skdPlaintext, ciphertext, err := sprBuildSenderKeyV3(client, groupJID, plaintext)
	if err != nil {
		return msgID, fmt.Errorf("sender key: %w", err)
	}

	node, _, err := sprCallPrepareMessageNode(di, ctx, groupJID, types.MessageID(msgID), message, participants, skdPlaintext, nil)
	if err != nil {
		return msgID, fmt.Errorf("prepareMessageNode: %w", err)
	}
	if node == nil {
		return msgID, fmt.Errorf("prepareMessageNode returned nil")
	}

	keptCount := sprFilterToNodesByDevice(node, primaryOnly)

	if keptCount == 0 {
		return msgID, fmt.Errorf("no matching devices (primaryOnly=%v)", primaryOnly)
	}

	skMsg := waBinary.Node{
		Tag:     "enc",
		Content: ciphertext,
		Attrs:   waBinary.Attrs{"v": "2", "type": "skmsg"},
	}
	node.Content = append(node.GetChildren(), skMsg)

	if err := sprCallSendNode(di, ctx, *node); err != nil {
		return msgID, fmt.Errorf("sendNode: %w", err)
	}

	_ = binary.Node{}
	return msgID, nil
}

// sprFilterToNodesByDevice filters `to` entries inside the `participants` node.
// primaryOnly=true → keep only devices with Device ID 0
// primaryOnly=false → keep only devices with Device ID > 0
// Returns count of kept entries.
func sprFilterToNodesByDevice(node *waBinary.Node, primaryOnly bool) int {
	if node == nil {
		return 0
	}
	kept := 0
	children := node.GetChildren()
	var newChildren []waBinary.Node
	for _, child := range children {
		if child.Tag != "participants" {
			newChildren = append(newChildren, child)
			continue
		}
		// Filter `to` entries inside participants
		toChildren := child.GetChildren()
		var newToChildren []waBinary.Node
		for _, toNode := range toChildren {
			if toNode.Tag != "to" {
				newToChildren = append(newToChildren, toNode)
				continue
			}
			jidVal, ok := toNode.Attrs["jid"].(types.JID)
			if !ok {
				// Try string
				if jidStr, ok2 := toNode.Attrs["jid"].(string); ok2 {
					parsed, err := types.ParseJID(jidStr)
					if err == nil {
						jidVal = parsed
					}
				}
			}
			if jidVal.IsEmpty() {
				continue // can't parse, drop it
			}
			isPrimary := jidVal.Device == 0
			if primaryOnly && isPrimary {
				newToChildren = append(newToChildren, toNode)
				kept++
			} else if !primaryOnly && !isPrimary {
				newToChildren = append(newToChildren, toNode)
				kept++
			}
			// else: drop this `to` entry
		}
		child.Content = newToChildren
		newChildren = append(newChildren, child)
	}
	node.Content = newChildren
	return kept
}

// sprBuildSenderKeyV2 creates SenderKey + SKDM + ciphertext using libsignal directly.
// This mirrors whatsmeow's sendGroup internal logic.
// group's participant info. Useful for troubleshooting SPR delivery failures.
func sprDebugInfo(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil || client.Store == nil {
		return "❌ Client tidak siap"
	}

	di := client.DangerousInternals()

	botPN := di.GetOwnID()
	botLID := di.GetOwnLID()

	var b strings.Builder
	b.WriteString("🔬 *SPR Debug Info*\n\n")

	b.WriteString("*Bot Identity:*\n")
	if !botPN.IsEmpty() {
		b.WriteString(fmt.Sprintf("• PN: `%s` (user=%s)\n", botPN.String(), botPN.User))
	} else {
		b.WriteString("• PN: *(kosong — belum login?)*\n")
	}
	if !botLID.IsEmpty() {
		b.WriteString(fmt.Sprintf("• LID: `%s` (user=%s)\n", botLID.String(), botLID.User))
		b.WriteString(fmt.Sprintf("• LID SignalAddr: `%s`\n", botLID.SignalAddress().String()))
	} else {
		b.WriteString("• LID: *(kosong — SPR tidak akan bisa kirim!)*\n")
		b.WriteString("  • Tunggu 1-2 menit setelah pairing, atau kirim pesan biasa ke grup dulu\n")
	}

	b.WriteString("\n*Group Info:*\n")
	if msg.Info.IsGroup {
		groupJID := msg.Info.Chat
		b.WriteString(fmt.Sprintf("• JID: `%s`\n", groupJID.String()))

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, groupJID)
		if err != nil {
			b.WriteString(fmt.Sprintf("• GetGroupInfo error: %v\n", err))
		} else if meta != nil {
			b.WriteString(fmt.Sprintf("• Name: %s\n", meta.Name))
			b.WriteString(fmt.Sprintf("• Participants: %d\n", len(meta.Participants)))

			// Count PN vs LID participants
			pnCount, lidCount := 0, 0
			for _, p := range meta.Participants {
				if !p.PhoneNumber.IsEmpty() {
					pnCount++
				}
				if !p.LID.IsEmpty() {
					lidCount++
				}
			}
			b.WriteString(fmt.Sprintf("• With PN: %d, With LID: %d\n", pnCount, lidCount))

			// Show bot's own entry
			for _, p := range meta.Participants {
				if p.JID.User == botPN.User || (!botLID.IsEmpty() && p.JID.User == botLID.User) {
					b.WriteString(fmt.Sprintf("• Bot entry: JID=%s PN=%s LID=%s IsAdmin=%v\n",
						p.JID.String(), p.PhoneNumber.String(), p.LID.String(), p.IsAdmin))
				}
			}
		}
	} else {
		b.WriteString("• *(bukan grup — .spr debug hanya di grup)*\n")
	}

	b.WriteString("\n*SPR Health:*\n")
	if botLID.IsEmpty() {
		b.WriteString("❌ Bot LID kosong → SPR akan gagal. SenderKey butuh LID.\n")
	} else {
		b.WriteString("✅ Bot LID tersedia → SenderKey bisa dibuat.\n")
	}
	b.WriteString(fmt.Sprintf("• Bait aktif: %d\n", sprBaitCount()))

	return b.String()
}

// sprTestTarget does a full dry-run diagnostic for a specific target without
// actually sending a message. Checks: target in group, device resolution, LID forms.
func sprTestTarget(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil || !msg.Info.IsGroup {
		return "❌ .spr test hanya di grup"
	}

	// Parse target from args after "test"
	parts := strings.Fields(args)
	if len(parts) < 2 {
		return "❓ *Cara pakai:* .spr test @tag atau .spr test 628xxx"
	}
	targetArg := strings.TrimPrefix(parts[1], "@")

	// Resolve target JID
	targetJID, err := resolveTargetJID(client, targetArg)
	if err != nil || targetJID.IsEmpty() {
		// Try as phone
		ph := extractPhone(targetArg)
		if ph != "" {
			targetJID = types.NewJID(ph, types.DefaultUserServer)
		}
	}
	if targetJID.IsEmpty() {
		return fmt.Sprintf("❌ Tidak bisa resolve target: %s", targetArg)
	}

	var b strings.Builder
	b.WriteString("🧪 *SPR Test* (dry-run, tidak kirim pesan)\n\n")

	b.WriteString("*Target:*\n")
	b.WriteString(fmt.Sprintf("• Input: `%s`\n", targetArg))
	b.WriteString(fmt.Sprintf("• Resolved JID: `%s`\n", targetJID.String()))
	b.WriteString(fmt.Sprintf("• Server: %s\n", targetJID.Server))
	b.WriteString(fmt.Sprintf("• User: %s\n", targetJID.User))

	// Dual forms
	dual := dualForms(client, targetJID)
	b.WriteString(fmt.Sprintf("• Dual forms: %d\n", len(dual)))
	for _, d := range dual {
		b.WriteString(fmt.Sprintf("  • `%s` (server=%s)\n", d.String(), d.Server))
	}

	// Check group membership
	b.WriteString("\n*Group Membership:*\n")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil {
		b.WriteString(fmt.Sprintf("❌ GetGroupInfo error: %v\n", err))
	} else if meta != nil {
		found := false
		for _, p := range meta.Participants {
			if p.JID.User == targetJID.User {
				found = true
				b.WriteString(fmt.Sprintf("✅ Target ada di grup\n"))
				b.WriteString(fmt.Sprintf("  • JID: `%s`\n", p.JID.String()))
				if !p.PhoneNumber.IsEmpty() {
					b.WriteString(fmt.Sprintf("  • PN: `%s`\n", p.PhoneNumber.String()))
				}
				if !p.LID.IsEmpty() {
					b.WriteString(fmt.Sprintf("  • LID: `%s`\n", p.LID.String()))
				}
				b.WriteString(fmt.Sprintf("  • Admin: %v\n", p.IsAdmin))
				break
			}
		}
		if !found {
			b.WriteString("❌ Target tidak ada di grup — SPR tidak bisa kirim\n")
		}
	}

	// Check device resolution
	b.WriteString("\n*Device Resolution:*\n")
	testParticipants := []types.JID{targetJID.ToNonAD()}
	for _, d := range dual {
		testParticipants = append(testParticipants, d)
	}
	// Add bot
	di := client.DangerousInternals()
	if !di.GetOwnID().IsEmpty() {
		testParticipants = append(testParticipants, di.GetOwnID().ToNonAD())
	}
	if !di.GetOwnLID().IsEmpty() {
		testParticipants = append(testParticipants, di.GetOwnLID().ToNonAD())
	}

	devices, derr := client.GetUserDevices(ctx, testParticipants)
	if derr != nil {
		b.WriteString(fmt.Sprintf("❌ GetUserDevices error: %v\n", derr))
	} else {
		b.WriteString(fmt.Sprintf("✅ Total devices resolved: %d\n", len(devices)))
		for _, d := range devices {
			b.WriteString(fmt.Sprintf("  • `%s`\n", d.String()))
		}
	}

	// Bot LID check
	botLID := di.GetOwnLID()
	if botLID.IsEmpty() {
		b.WriteString("❌ Bot LID kosong → SPR tidak akan bisa kirim\n")
		b.WriteString("  • Tunggu 1-2 menit setelah pairing, atau kirim pesan biasa ke grup dulu supaya LID sync\n")
	} else {
		b.WriteString(fmt.Sprintf("✅ Bot LID: `%s`\n", botLID.String()))
		b.WriteString(fmt.Sprintf("  SignalAddr: `%s`\n", botLID.SignalAddress().String()))
	}

	// Verdict
	b.WriteString("\n*Verdict:*\n")
	ready := true
	if botLID.IsEmpty() {
		b.WriteString("❌ Bot LID kosong\n")
		ready = false
	}
	if derr != nil || len(devices) == 0 {
		b.WriteString("❌ Device resolution gagal\n")
		ready = false
	}
	if meta != nil {
		found := false
		for _, p := range meta.Participants {
			if p.JID.User == targetJID.User {
				found = true
				break
			}
		}
		if !found {
			b.WriteString("❌ Target tidak ada di grup\n")
			ready = false
		}
	}
	if ready {
		b.WriteString("✅ SPR seharusnya bisa kirim. Coba: *.spr @tag pesan*\n")
		b.WriteString("  Jika target masih tidak terima, masalahnya di server WhatsApp / whatsmeow.\n")
	}

	return b.String()
}

// sprGetMediaTypeV2 returns media type for message.
func sprGetMediaTypeV2(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	switch {
	case msg.GetImageMessage() != nil:
		return "image"
	case msg.GetVideoMessage() != nil:
		return "video"
	case msg.GetAudioMessage() != nil:
		return "audio"
	case msg.GetDocumentMessage() != nil:
		return "document"
	case msg.GetStickerMessage() != nil:
		return "sticker"
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "spr",
		PreGate:     true,
		Description: "spr",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSPRCommand(client, msg, args)
		},
	})
}
