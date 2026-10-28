package main

// sbpr.go — SBPR (Send Batch Private Relay) — Queue mode dengan SPR
//
// Beda dengan .spr yang kirim ke SEMUA target sekaligus dalam 1 pesan grup,
// .sbpr kirim ke setiap target SATU PER SATU via SPR (sistem anteran).
//
// WAJIB pakai SPR (SendGroup) — bukan private chat.
// Target mendapat SKDM → bisa baca pesan.
// Non-target: "Menunggu pesan ini…" (default WhatsApp, unavoidable di grup).
//
// Usage:
//   .sbpr @tag1 @tag2 teks rahasia  — kirim ke target satu per satu via SPR
//   .sbpr 628xxx teks               — kirim ke nomor
//   .sbpr sapa                       — sapa semua member
//   .sbpr status                     — cek progress
//   .sbpr help                       — bantuan
//
// Owner only. Group only. Silent (no confirmation reply).

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	sbprDelayPerTarget = 2 * time.Second
	sbprMaxTargets     = 20
	sbprMaxText        = 4096
)

type sbprEntry struct {
	Target       types.JID
	Text         string
	PPURL        string
	MentionedJID string
	Status       string
	SentAt       time.Time
	Error        string
}

var (
	sbprQueueMu sync.Mutex
	sbprQueue   = []*sbprEntry{}
	sbprRunning bool
)

func handleSBPRCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	sub := strings.Fields(strings.ToLower(args))
	if len(sub) == 0 || sub[0] == "help" || sub[0] == "bantuan" {
		return sbprHelp()
	}
	if sub[0] == "status" || sub[0] == "stat" {
		return sbprStatus()
	}
	if sub[0] == "sapa" {
		return handleSBPRSapa(client, msg)
	}
	if sub[0] == "getpp" {
		return handleSBPRGetPP(client, msg)
	}
	if sub[0] == "batch" {
		return handleSBPRBatch(client, msg, args)
	}
	// sBPR asli: sendBatchPrivateRelay langsung (bukan queue)
	// Parse targets + text
	targets, text, err := resolveSPRTargets(client, msg, args)
	if err != nil {
		return "❓ " + err.Error()
	}
	if len(targets) > sbprMaxTargets {
		return fmt.Sprintf("❌ Maksimal %d target", sbprMaxTargets)
	}
	if len(text) > sbprMaxText {
		text = text[:sbprMaxText]
	}

	// Kirim langsung via batch (key ID sama, isi berbeda, relay berulang)
	go sbprBatchSend(client, msg.Info.Chat, targets, text)

	// Silent — no confirmation reply
	return ""
}

// handleSBPRBatch — sBPR v2: sendBatchPrivateRelay
// Fetch grup sekali, kirim dengan key ID sama tapi isi berbeda, relay berulang.
// Usage: .sbpr batch @tag1 @tag2 teks (bisa pakai {nama} untuk personalisasi)
func handleSBPRBatch(client *whatsmeow.Client, msg *events.Message, args string) string {
	// Hilangkan kata "batch" dari args
	rest := strings.TrimSpace(args[len("batch"):])
	targets, text, err := resolveSPRTargets(client, msg, rest)
	if err != nil {
		return "❓ " + err.Error()
	}
	if len(targets) > sbprMaxTargets {
		return fmt.Sprintf("❌ Maksimal %d target", sbprMaxTargets)
	}
	if len(text) > sbprMaxText {
		text = text[:sbprMaxText]
	}
	go sbprBatchSend(client, msg.Info.Chat, targets, text)
	return "" // silent
}

// sbprWorker processes the queue one by one via SPR (SendGroup).
//
// PENTING: flag sbprRunning sudah di-claim atomik oleh spawner (pola shouldSpawn
// di dalam sbprQueueMu). Worker TIDAK boleh cek "if sbprRunning { return }"
// di sini — karena flag sudah true saat worker di-spawn, cek itu bikin worker
// langsung return tanpa memproses antrean (bug: SBPR mati total).
// Single-worker dijamin oleh claim atomik di spawner.
func sbprWorker(client *whatsmeow.Client, groupJID types.JID) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[SBPR] PANIC di worker: %v\n", r)
		}
		sbprQueueMu.Lock()
		sbprRunning = false
		sbprQueueMu.Unlock()
	}()

	for {
		sbprQueueMu.Lock()
		if len(sbprQueue) == 0 {
			sbprQueueMu.Unlock()
			break
		}
		entry := sbprQueue[0]
		sbprQueue = sbprQueue[1:]
		entry.Status = "sending"
		sbprQueueMu.Unlock()

		// Build message
		var waMsg *waE2E.Message
		if entry.PPURL != "" {
			waMsg = sbprBuildPPMessage(client, entry.PPURL, entry.Text)
		}
		if waMsg == nil {
			extText := &waE2E.ExtendedTextMessage{Text: proto.String(entry.Text)}
			waMsg = &waE2E.Message{ExtendedTextMessage: extText}
		}

		// Build participants (just this one target)
		participants, err := buildSPRParticipants(client, groupJID, []types.JID{entry.Target})
		if err != nil {
			entry.Status = "failed"
			entry.Error = err.Error()
			time.Sleep(sbprDelayPerTarget)
			continue
		}

		// Rotate SenderKey before send.
		// Dibungkus sprBaseMu agar tidak interleave dengan SPR lain yang
		// sedang dalam rangkaian rotation→send.
		sprBaseMu.Lock()
		if rerr := sprRotateSenderKey(groupJID); rerr != nil {
			fmt.Printf("[SBPR] ⚠️ pre-send rotation gagal: %v\n", rerr)
		}
		sprBaseMu.Unlock()

		// Send via SPR with decrypt-fail:hide
		id, hideErr := sendSPREngineAHide(client, groupJID, participants, waMsg)
		if hideErr != nil {
			id2, _, _, _, err2 := sendSPREngineA(client, groupJID, participants, waMsg)
			if err2 != nil {
				entry.Status = "failed"
				entry.Error = err2.Error()
			} else {
				entry.Status = "done"
				entry.SentAt = time.Now()
				sprRegisterBait(id2, groupJID.String(), "B", nil)
			}
		} else {
			entry.Status = "done"
			entry.SentAt = time.Now()
			sprRegisterBait(id, groupJID.String(), "B", nil)
		}

		// Rotate SenderKey after send
		go func() {
			time.Sleep(2 * time.Second)
			sprBaseMu.Lock()
			if rerr := sprRotateSenderKey(groupJID); rerr != nil {
				fmt.Printf("[SBPR] ⚠️ post-send rotation gagal: %v\n", rerr)
			}
			sprBaseMu.Unlock()
		}()

		// Delay before next target
		time.Sleep(sbprDelayPerTarget)
	}

	// Silent — no notification message
}

// handleSBPRSapa — greet all group members via SPR queue
func handleSBPRSapa(client *whatsmeow.Client, msg *events.Message) string {
	if msg == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || meta == nil {
		return fmt.Sprintf("❌ Gagal ambil member: %v", err)
	}

	var targets []types.JID
	for _, p := range meta.Participants {
		if p.JID.IsEmpty() {
			continue
		}
		targets = append(targets, p.JID.ToNonAD())
	}

	if len(targets) == 0 {
		return "❌ Tidak ada member"
	}

	// sBPR asli: key ID sama untuk semua target
	go sbprBatchSend(client, msg.Info.Chat, targets, "Halo {nomor} 👋")

	// Silent
	return ""
}

func sbprStatus() string {
	sbprQueueMu.Lock()
	defer sbprQueueMu.Unlock()

	total := len(sbprQueue)
	if total == 0 && !sbprRunning {
		return "📋 *SBPR Status*\n\n• State: idle"
	}

	var queued, sending, done, failed int
	for _, e := range sbprQueue {
		switch e.Status {
		case "queued":
			queued++
		case "sending":
			sending++
		case "done":
			done++
		case "failed":
			failed++
		}
	}

	status := "idle"
	if sbprRunning {
		status = "running"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("📋 *SBPR Status*\n\n• State: %s\n• Sisa antrean: %d", status, total))
	if done > 0 || failed > 0 {
		b.WriteString(fmt.Sprintf("\n• Selesai: %d, gagal: %d", done, failed))
	}
	if total > 0 {
		b.WriteString("\n")
		for i, e := range sbprQueue {
			if i >= 10 {
				b.WriteString(fmt.Sprintf("\n• … +%d lainnya", total-10))
				break
			}
			ph := phoneOf(botClient, e.Target)
			if ph == "" {
				ph = e.Target.User
			}
			icon := "⏳"
			switch e.Status {
			case "sending":
				icon = "📤"
			case "done":
				icon = "✅"
			case "failed":
				icon = "❌"
			}
			b.WriteString(fmt.Sprintf("\n• %s %s", icon, ph))
		}
	}
	return b.String()
}

func sbprHelp() string {
	return `📨 *SBPR — Send Batch Private Relay*

Fetch grup sekali, kirim dengan KEY ID SAMA tapi ISI BERBEDA per target.
Relay berulang 3x dengan rotasi sender key.
Target dapat baca, lainnya: "Menunggu pesan ini…"

• .sbpr @tag teks
• .sbpr @a @b Halo {nama}!
• .sbpr 628xxx teks
• .sbpr sapa — sapa semua member
• .sbpr status — cek progress

{nama} = nama target, {nomor} = nomor target`
}

// handleSBPRGetPP — ambil PP + pushname semua member, kirim via SBPR
//
// ⚡ OPTIMIZED dengan goroutines:
//   - Fetch PP + pushname untuk SEMUA member secara PARALLEL (max 20 concurrent)
//   - Setiap member: 2 HTTP requests (PP + userinfo) → ~200ms
//   - 100 member: ~5 detik (vs 33 menit sequential)
//
// Flow:
//  1. GetGroupInfo → list semua member
//  2. Spawn goroutine per member (max 20 concurrent via semaphore)
//  3. Setiap goroutine: fetch PP URL + pushName, kirim ke channel
//  4. Main goroutine collect dari channel → entries
//  5. Push entries ke sbprQueue → sbprWorker kirim via SPR
func handleSBPRGetPP(client *whatsmeow.Client, msg *events.Message) string {
	if msg == nil {
		return ""
	}
	if client == nil {
		return "❌ Client tidak siap"
	}
	if client.Store == nil || client.Store.ID == nil {
		return "❌ Bot belum login"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || meta == nil {
		return fmt.Sprintf("❌ Gagal ambil member: %v", err)
	}

	// Filter valid members (skip bot)
	var members []types.JID
	for _, p := range meta.Participants {
		if p.JID.IsEmpty() || p.JID.String() == client.Store.ID.String() {
			continue
		}
		members = append(members, p.JID.ToNonAD())
	}
	if len(members) == 0 {
		return "❌ Tidak ada member"
	}

	// Channel untuk collect entries dari goroutines
	type ppResult struct {
		entry *sbprEntry
	}
	resultCh := make(chan ppResult, len(members))

	// Semaphore untuk limit concurrency (max 20 parallel)
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup

	// Spawn goroutine per member
	for _, memberJID := range members {
		wg.Add(1)
		go func(m types.JID) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("[SBPR-getpp] panic for %s: %v\n", m.String(), r)
				}
			}()

			sem <- struct{}{}        // acquire (block if 20 in flight)
			defer func() { <-sem }() // release

			// Resolve target JID (prefer PN form)
			targetJID := m
			if ph := phoneOf(client, m); ph != "" {
				targetJID = types.NewJID(ph, types.DefaultUserServer)
			}

			// Fetch PP URL (with fallback to original JID if PN form fails)
			ppURL := ""
			ppCtx, ppCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer ppCancel()
			if ppInfo, err := client.GetProfilePictureInfo(ppCtx, targetJID, &whatsmeow.GetProfilePictureParams{}); err == nil && ppInfo != nil && ppInfo.URL != "" {
				ppURL = ppInfo.URL
			}
			if ppURL == "" && m != targetJID {
				if ppInfo, err := client.GetProfilePictureInfo(ppCtx, m, &whatsmeow.GetProfilePictureParams{}); err == nil && ppInfo != nil && ppInfo.URL != "" {
					ppURL = ppInfo.URL
				}
			}
			ppCancel()

			// Fetch pushName
			pushName := targetJID.User
			uCtx, uCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if users, err := client.GetUserInfo(uCtx, []types.JID{targetJID}); err == nil {
				if u, ok := users[targetJID]; ok && u.VerifiedName != nil && u.VerifiedName.Details != nil {
					if vn := u.VerifiedName.Details.GetVerifiedName(); vn != "" {
						pushName = vn
					}
				}
			}
			uCancel()

			// No profile picture available — leave the URL empty.
			// (Hardcoding a CDN URL would point at somebody else's avatar.)

			resultCh <- ppResult{entry: &sbprEntry{
				Target: targetJID,
				Text:   pushName,
				PPURL:  ppURL,
				Status: "queued",
			}}
		}(memberJID)
	}

	// Wait semua goroutine selesai, lalu close channel
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Collect entries dari channel
	var entries []*sbprEntry
	for r := range resultCh {
		entries = append(entries, r.entry)
	}

	fmt.Printf("[SBPR-getpp] fetched %d/%d members (parallel)\n", len(entries), len(members))

	// sBPR asli: kirim PP dengan key ID sama
	go sbprBatchSendPP(client, msg.Info.Chat, entries)
	return ""
}

// handleSBPRTag — .tag <text> — kirim tag ke semua member via SBPR
func handleSBPRTag(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner/admin grup"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}
	text := strings.TrimSpace(args)
	if text == "" {
		return "❓ *Cara pakai:* .tag <teks>"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || meta == nil {
		return fmt.Sprintf("❌ Gagal ambil member: %v", err)
	}
	var entries []*sbprEntry
	for _, p := range meta.Participants {
		if p.JID.IsEmpty() || p.JID.String() == client.Store.ID.String() {
			continue
		}
		targetJID := p.JID.ToNonAD()
		if ph := phoneOf(client, p.JID); ph != "" {
			targetJID = types.NewJID(ph, types.DefaultUserServer)
		}
		entries = append(entries, &sbprEntry{Target: targetJID, Status: "queued"})
	}
	if len(entries) == 0 {
		return "❌ Tidak ada member"
	}
	var targets []types.JID
	for _, e := range entries {
		targets = append(targets, e.Target)
	}
	// sBPR asli: key ID sama, setiap target di-tag
	go sbprBatchSend(client, msg.Info.Chat, targets, "{tag} "+text)
	return ""
}

// handleJoinCommand — .join <link grup>
func handleJoinCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	link := strings.TrimSpace(args)
	if link == "" {
		return "❓ *Cara pakai:* .join <link grup>\nContoh: .join https://chat.whatsapp.com/xxxxx"
	}
	code := ""
	if idx := strings.Index(link, "chat.whatsapp.com/"); idx >= 0 {
		code = link[idx+len("chat.whatsapp.com/"):]
		if q := strings.Index(code, "?"); q >= 0 {
			code = code[:q]
		}
	} else if !strings.Contains(link, "/") && !strings.Contains(link, "http") {
		code = link
	} else {
		return "❓ *Cara pakai:* .join https://chat.whatsapp.com/xxxxx"
	}
	if code == "" {
		return "❌ Kode invite kosong"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	groupJID, err := client.JoinGroupWithLink(ctx, code)
	if err != nil {
		return fmt.Sprintf("❌ Gagal join grup: %v", err)
	}
	return fmt.Sprintf("✅ *Berhasil Join Grup*\n\n• Group JID: %s\n• Bot sekarang member grup ini", groupJID.String())
}

// sbprBuildPPMessage fetches PP image, uploads, builds imageMessage with caption
func sbprBuildPPMessage(client *whatsmeow.Client, ppURL, pushName string) *waE2E.Message {
	if client == nil || ppURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ppURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	// Batasi 32MB untuk cegah OOM dari response raksasa.
	imgData, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil || len(imgData) < 1024 {
		return nil
	}
	upCtx, upCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer upCancel()
	up, err := client.Upload(upCtx, imgData, whatsmeow.MediaImage)
	if err != nil {
		return nil
	}
	caption := "sesuai dengan foto profilmu " + pushName + "\n> _hanya kamu yang dapat melihat pesan ini._"
	return &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String("image/jpeg"),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(imgData))),
			Caption:       proto.String(caption),
		},
	}
}

// ==== sBPR v2: sendBatchPrivateRelay ====
// Konsep dari user:
//   "kirim langsung nge fetch satu grup dan pesan nya cuma sekali
//    dengan key id sama tapi isi pesan berbeda,
//    di relay berulang dan dikirim berulang"
//
// Implementasi:
//   1. Fetch daftar member grup SEKALI (bukan per-target)
//   2. Generate SATU base message ID (key ID sama untuk semua)
//   3. Untuk setiap target: kirim dengan ID yang sama tapi konten berbeda
//      (personalisasi via mention/nama)
//   4. Relay berulang (3x) untuk reliabilitas

const (
	sbprBatchRelayCount = 3
	sbprBatchDelay      = 500 * time.Millisecond
)

// sbprBatchSend — kirim batch ke multiple target dengan key ID sama.
// VERSI GOROUTINE: kirim paralel dengan semaphore (max 5 concurrent).
func sbprBatchSend(client *whatsmeow.Client, groupJID types.JID, targets []types.JID, textTpl string) (int, int) {
	if len(targets) == 0 || client == nil {
		return 0, 0
	}
	pushNames := make(map[string]string)
	baseID := "SBPR" + fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	var sent, failed int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, target := range targets {
		wg.Add(1)
		go func(t types.JID) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			phone := phoneOf(client, t)
			if phone == "" {
				phone = t.User
			}
			name, ok := pushNames[phone]
			if !ok || name == "" {
				name = phone
			}
			text := strings.ReplaceAll(textTpl, "{nama}", name)
			text = strings.ReplaceAll(text, "{nomor}", phone)
			text = strings.ReplaceAll(text, "{tag}", "@"+phone)
			// Typing indicator biar natural
			go func() {
				tctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				_ = client.SendChatPresence(tctx, groupJID, types.ChatPresenceComposing, types.ChatPresenceMediaText)
			}()
			// Build mention list: if {tag} was in template, add target JID
			// so WhatsApp renders @phone as a real mention (blue clickable),
			// not just plain text. MentionedJID is in ContextInfo (not directly
			// on ExtendedTextMessage).
			var mentionedJIDs []string
			if strings.Contains(textTpl, "{tag}") {
				mentionedJIDs = []string{t.String()}
			}
			extMsg := &waE2E.ExtendedTextMessage{
				Text: proto.String(text),
			}
			if len(mentionedJIDs) > 0 {
				extMsg.ContextInfo = &waE2E.ContextInfo{
					MentionedJID: mentionedJIDs,
				}
			}
			waMsg := &waE2E.Message{
				ExtendedTextMessage: extMsg,
			}
			participants, err := buildSPRParticipants(client, groupJID, []types.JID{t})
			if err != nil {
				atomic.AddInt32(&failed, 1)
				return
			}
			var lastErr error
			for r := 0; r < sbprBatchRelayCount; r++ {
				sprBaseMu.Lock()
				if rerr := sprRotateSenderKey(groupJID); rerr != nil {
					fmt.Printf("[SBPRv2] rotasi gagal: %v\n", rerr)
				}
				sprBaseMu.Unlock()
				if id, err := sendSPRWithID(client, groupJID, participants, waMsg, baseID); err == nil {
					sprRegisterBait(id, groupJID.String(), "Bv2", nil)
					lastErr = nil
					break
				} else {
					lastErr = err
				}
				time.Sleep(sbprBatchDelay + time.Duration(rand.Intn(400))*time.Millisecond)
			}
			if lastErr != nil {
				atomic.AddInt32(&failed, 1)
			} else {
				atomic.AddInt32(&sent, 1)
			}
		}(target)
	}
	wg.Wait()
	return int(sent), int(failed)
}

// sbprBatchSendPP — kirim foto profil ke multiple target dengan key ID sama.
// VERSI GOROUTINE: paralel dengan semaphore (max 5 concurrent).
func sbprBatchSendPP(client *whatsmeow.Client, groupJID types.JID, entries []*sbprEntry) (int, int) {
	if len(entries) == 0 || client == nil {
		return 0, 0
	}
	baseID := "SBPR" + fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	var sent, failed int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, e := range entries {
		wg.Add(1)
		go func(entry *sbprEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			waMsg := sbprBuildPPMessage(client, entry.PPURL, entry.Text)
			if waMsg == nil {
				atomic.AddInt32(&failed, 1)
				return
			}
			participants, err := buildSPRParticipants(client, groupJID, []types.JID{entry.Target})
			if err != nil {
				atomic.AddInt32(&failed, 1)
				return
			}
			var lastErr error
			for r := 0; r < sbprBatchRelayCount; r++ {
				sprBaseMu.Lock()
				if rerr := sprRotateSenderKey(groupJID); rerr != nil {
					fmt.Printf("[SBPRv2-PP] rotasi gagal: %v\n", rerr)
				}
				sprBaseMu.Unlock()
				if id, err := sendSPRWithID(client, groupJID, participants, waMsg, baseID); err == nil {
					sprRegisterBait(id, groupJID.String(), "Bv2PP", nil)
					lastErr = nil
					break
				} else {
					lastErr = err
				}
				time.Sleep(sbprBatchDelay + time.Duration(rand.Intn(400))*time.Millisecond)
			}
			if lastErr != nil {
				atomic.AddInt32(&failed, 1)
			} else {
				atomic.AddInt32(&sent, 1)
			}
		}(e)
	}
	wg.Wait()
	return int(sent), int(failed)
}

func sendSPRWithID(client *whatsmeow.Client, groupJID types.JID, participants []types.JID, waMsg *waE2E.Message, msgID string) (string, error) {
	// Gunakan engine A hide dengan ID override
	if err := sendSPREngineAHideWithID(client, groupJID, participants, waMsg, msgID); err == nil {
		return msgID, nil
	}
	// Fallback ke engine A biasa dengan ID override
	if _, _, _, err := sendSPREngineAWithID(client, groupJID, participants, waMsg, msgID); err == nil {
		return msgID, nil
	} else {
		return "", err
	}
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "tag",
		Description: "tag",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSBPRTag(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "join",
		Description: "join",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleJoinCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "sbpr",
		Description: "sbpr",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSBPRCommand(client, msg, args)
			// ─── SPR2 (Fast SPR — react ⚡ + background SPR) ──────
		},
	})
}
