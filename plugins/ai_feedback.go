package main

// ai_feedback.go — Feedback loop AI via reaksi 🤖 (owner only).
// ════════════════════════════════════════════════════════════
//
// Cara pakai: owner react 🤖 ke pesan AI bot yang "kedengeran kayak robot".
// Bot menyimpan pasangan (pertanyaan user → jawaban AI) ke PG ai_feedback
// table + kv_store untuk patterns/training.
//
// Semua lokal — tidak pernah dikirim ke grup manapun.
//
// 2026-10-27: migrated from 4 JSON files to PG:
//   data/ai_feedback.json  → ai_feedback table (rows)
//   data/ai_sent.json      → kv_store(ai_sent)  (in-memory cache + write-through)
//   data/bot_training.json → kv_store(bot_training)
//   data/bot_patterns.json → kv_store(bot_patterns)

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

const aiFeedbackEmoji = "🤖"

const aiSentCacheTTL = 24 * time.Hour
const aiSentCacheCap = 500

// kv_store keys for AI feedback related data
const (
	kvKeyAISent      = "ai_sent"
	kvKeyBotTraining = "bot_training"
	kvKeyBotPatterns = "bot_patterns"
)

// aiFeedbackRating for entries flagged via 🤖 reaction.
const aiFeedbackRating = "down"

type aiSentEntry struct {
	Query string
	Reply string
	Chat  string
	TS    time.Time
}

type aiFeedbackEntry struct {
	TS    int64  `json:"ts"`
	Chat  string `json:"chat"`
	MsgID string `json:"msg_id"`
	Query string `json:"query"`
	Reply string `json:"reply"`
}

// botPattern adalah pola baris yang muncul di beberapa contoh training.
type botPattern struct {
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
	Example string `json:"example"`
}

var (
	aiFeedbackMu  sync.Mutex
	aiSentCache   = make(map[string]aiSentEntry)
	aiSentCacheMu sync.Mutex
	aiSentLoaded  bool
)

// rememberAISent dipanggil setiap AI mengirim balasan.
// In-memory cache (TTL 24h, cap 500) + write-through to kv_store.
func rememberAISent(msgID, query, reply, chatJID string) {
	if msgID == "" || reply == "" {
		return
	}
	aiSentCacheMu.Lock()
	aiSentCacheEnsureLoadedLocked()
	now := time.Now()
	// Purge expired entries
	for id, e := range aiSentCache {
		if now.Sub(e.TS) > aiSentCacheTTL {
			delete(aiSentCache, id)
		}
	}
	// Enforce cap (evict oldest)
	for len(aiSentCache) >= aiSentCacheCap {
		oldest := ""
		var oldestTS time.Time
		for id, e := range aiSentCache {
			if oldest == "" || e.TS.Before(oldestTS) {
				oldest, oldestTS = id, e.TS
			}
		}
		delete(aiSentCache, oldest)
	}
	aiSentCache[msgID] = aiSentEntry{Query: query, Reply: reply, Chat: chatJID, TS: now}
	aiSentCacheMu.Unlock()
	go aiSentCacheSave()
}

// aiSentCacheEnsureLoadedLocked loads the cache from PG kv_store on first
// access. Filters out expired entries at load time. Caller must hold lock.
func aiSentCacheEnsureLoadedLocked() {
	if aiSentLoaded {
		return
	}
	aiSentLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stored map[string]aiSentEntry
	if kvStoreGet(ctx, kvKeyAISent, &stored) {
		now := time.Now()
		for id, e := range stored {
			if now.Sub(e.TS) <= aiSentCacheTTL {
				aiSentCache[id] = e
			}
		}
	}
}

// aiSentCacheSave persists the in-memory cache to PG kv_store (async).
func aiSentCacheSave() {
	aiSentCacheMu.Lock()
	cp := make(map[string]aiSentEntry, len(aiSentCache))
	for k, v := range aiSentCache {
		cp[k] = v
	}
	aiSentCacheMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = kvStoreSet(ctx, kvKeyAISent, &cp)
}

// handleAIFeedbackReaction menangani reaksi 🤖 dari owner ke pesan AI.
// Dipasang di handleMessage SEBELUM filter IsFromMe (seperti dmsgva).
// Return true jika dikonsumsi.
func handleAIFeedbackReaction(client *whatsmeow.Client, msg *events.Message) bool {
	if msg == nil || msg.Message == nil {
		return false
	}
	reaction := msg.Message.GetReactionMessage()
	if reaction == nil {
		return false
	}
	rtext := reaction.GetText()
	// Normalisasi: buang variation selector (FE0F) & ZWJ biar 🤖️ tetap cocok
	rtextNorm := strings.Map(func(r rune) rune {
		if r == 0xFE0F || r == 0x200D {
			return -1
		}
		return r
	}, rtext)
	isOwner := isOwnerMsg(msg) || msg.Info.IsFromMe
	fmt.Printf("[aifeedback-debug] reaction=%q norm=%q target=%s from=%s isOwner=%v\n",
		rtext, rtextNorm, reaction.GetKey().GetID(), msg.Info.Sender.String(), isOwner)
	if !strings.Contains(rtextNorm, aiFeedbackEmoji) {
		return false
	}
	// Hanya owner (atau dari akun bot sendiri via companion)
	if !isOwner {
		fmt.Printf("[aifeedback-debug] bukan owner, skip\n")
		return false
	}
	targetID := reaction.GetKey().GetID()
	if targetID == "" {
		return false
	}

	aiSentCacheMu.Lock()
	aiSentCacheEnsureLoadedLocked()
	entry, ok := aiSentCache[targetID]
	aiSentCacheMu.Unlock()

	isOurAI := ok || strings.HasPrefix(targetID, "aiagent-") || isAIResponse(targetID)

	// Kasus 1: pesan AI kita → feedback gaya bahasa (perbagus Ruby)
	if isOurAI {
		fb := aiFeedbackEntry{
			TS:    time.Now().Unix(),
			Chat:  msg.Info.Chat.String(),
			MsgID: targetID,
			Query: entry.Query,
			Reply: entry.Reply,
		}
		if err := aiFeedbackAppend(fb); err != nil {
			fmt.Printf("[aifeedback] gagal simpan: %v\n", err)
			return true
		}
		fmt.Printf("[aifeedback] 🤖 tercatat untuk %s (query %d char, reply %d char)\n",
			targetID, len(entry.Query), len(entry.Reply))
		return true
	}

	// Kasus 2: pesan bot LAIN → simpan sebagai contoh training untuk .isbot
	// Ambil teks dari arsip berdasarkan ID
	trainingText := ""
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	db, _ := pgGet()
	if db != nil {
		_ = pgQueryRow(ctx, db,
			`SELECT text FROM message_archive WHERE msg_id = ? ORDER BY id DESC LIMIT 1`,
			targetID).Scan(&trainingText)
	}
	cancel()
	if trainingText == "" {
		fmt.Printf("[aifeedback] 🤖 target %s bukan AI kita & tidak ada teks di arsip — skip\n", targetID)
		return true
	}
	if err := botTrainingAppend(targetID, trainingText); err != nil {
		fmt.Printf("[aifeedback] gagal simpan training: %v\n", err)
		return true
	}
	fmt.Printf("[aifeedback] 🤖 training isbot tercatat: %s (%d char)\n", targetID, len(trainingText))
	return true
}

// botTrainingAppend menyimpan contoh pesan bot (dari reaksi 🤖 owner ke pesan bot lain)
// sebagai data training untuk memperkuat .isbot. Setiap contoh baru otomatis
// ditambang polanya (mineBotPatterns) untuk pemantauan kontinu.
//
// Storage: kv_store(bot_training) — list of {ts, msg_id, text}, cap 2000.
func botTrainingAppend(msgID, text string) error {
	aiFeedbackMu.Lock()
	defer aiFeedbackMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var list []map[string]any
	kvStoreGet(ctx, kvKeyBotTraining, &list)
	if list == nil {
		list = []map[string]any{}
	}
	// hindari duplikat ID
	for _, e := range list {
		if e["msg_id"] == msgID {
			return nil
		}
	}
	list = append(list, map[string]any{
		"ts":     time.Now().Unix(),
		"msg_id": msgID,
		"text":   text,
	})
	if len(list) > 2000 {
		list = list[len(list)-2000:]
	}
	if err := kvStoreSet(ctx, kvKeyBotTraining, &list); err != nil {
		return err
	}
	// Tambang pola dari contoh baru (pemantauan kontinu)
	mineBotPatternsLocked(list)
	return nil
}

// mineBotPatternsLocked menambang baris-baris template yang berulang di contoh training.
// Baris yang muncul di ≥2 contoh dianggap kandidat template bot.
// Harus dipanggil dengan aiFeedbackMu terkunci.
//
// Storage: kv_store(bot_patterns) — list of {pattern, count, example}, cap 100.
func mineBotPatternsLocked(list []map[string]any) {
	lineCount := make(map[string]int)
	lineExample := make(map[string]string)
	for _, e := range list {
		text, _ := e["text"].(string)
		seen := make(map[string]bool)
		for _, line := range strings.Split(text, "\n") {
			l := strings.TrimSpace(line)
			if len([]rune(l)) < 8 || len([]rune(l)) > 120 {
				continue
			}
			// normalisasi: angka/IP/versi jadi placeholder biar template ketangkap
			norm := botPatternNormalize(l)
			if seen[norm] {
				continue
			}
			seen[norm] = true
			lineCount[norm]++
			if _, ok := lineExample[norm]; !ok {
				lineExample[norm] = l
			}
		}
	}
	var patterns []botPattern
	for norm, c := range lineCount {
		if c >= 2 {
			patterns = append(patterns, botPattern{Pattern: norm, Count: c, Example: lineExample[norm]})
		}
	}
	// urut by count desc
	for i := 0; i < len(patterns); i++ {
		for j := i + 1; j < len(patterns); j++ {
			if patterns[j].Count > patterns[i].Count {
				patterns[i], patterns[j] = patterns[j], patterns[i]
			}
		}
	}
	if len(patterns) > 100 {
		patterns = patterns[:100]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = kvStoreSet(ctx, kvKeyBotPatterns, &patterns)
	if len(patterns) > 0 {
		fmt.Printf("[aifeedback] pola bot ditambang: %d kandidat (top: %q x%d)\n",
			len(patterns), patterns[0].Example, patterns[0].Count)
	}
}

// botPatternNormalize mengganti angka/IP/versi dengan placeholder.
func botPatternNormalize(s string) string {
	s = rePatternIP.ReplaceAllString(s, "<IP>")
	s = rePatternNum.ReplaceAllString(s, "<N>")
	s = rePatternVer.ReplaceAllString(s, "<V>")
	return strings.ToLower(s)
}

var (
	rePatternIP  = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
	rePatternNum = regexp.MustCompile(`\b\d+(\.\d+)?\s*(MB|GB|KB|ms|MHz|%|x)?\b`)
	rePatternVer = regexp.MustCompile(`\bv?\d+\.\d+(\.\d+)?\b`)
)

// handleBotTrainCommand — .bottrain [apply]
func handleBotTrainCommand(msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	args = strings.TrimSpace(strings.ToLower(args))

	// Muat training + patterns from PG
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var tlist []map[string]any
	kvStoreGet(ctx, kvKeyBotTraining, &tlist)
	if tlist == nil {
		tlist = []map[string]any{}
	}
	var patterns []botPattern
	kvStoreGet(ctx, kvKeyBotPatterns, &patterns)
	if patterns == nil {
		patterns = []botPattern{}
	}

	if args == "apply" {
		// Terapkan pola dengan count>=3 sebagai keyword deteksi (skor 25)
		applied := 0
		for _, p := range patterns {
			if p.Count >= 3 && len(p.Pattern) >= 10 {
				key := strings.ToLower(p.Pattern)
				if _, exists := botMenuKeywords[key]; !exists {
					botMenuKeywords[key] = 25
					applied++
				}
			}
		}
		return fmt.Sprintf("✅ %d pola diterapkan ke deteksi .isbot", applied)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🤖 *Bot Training* — %d contoh, %d pola kandidat\n\n", len(tlist), len(patterns)))
	if len(patterns) == 0 {
		sb.WriteString("Belum ada pola berulang. Terus tandai pesan bot dengan 🤖.")
		return sb.String()
	}
	sb.WriteString("*Pola teratas:*\n")
	for i, p := range patterns {
		if i >= 8 {
			break
		}
		ex := p.Example
		if len([]rune(ex)) > 60 {
			ex = string([]rune(ex)[:60]) + "…"
		}
		sb.WriteString(fmt.Sprintf("• (%dx) %s\n", p.Count, ex))
	}
	sb.WriteString("\n`.bottrain apply` untuk terapkan pola (≥3x) ke .isbot.")
	return sb.String()
}

// aiFeedbackAppend persists one feedback entry to PG ai_feedback table.
// Cap at 2000 rows (evict oldest by timestamp).
func aiFeedbackAppend(fb aiFeedbackEntry) error {
	aiFeedbackMu.Lock()
	defer aiFeedbackMu.Unlock()

	db, err := pgGet()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Insert new row
	_, err = pgExec(ctx, db,
		`INSERT INTO ai_feedback (msg_id, chat, sender, rating, ai_text, user_feedback, timestamp)
                 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fb.MsgID, fb.Chat, "", aiFeedbackRating, fb.Reply, fb.Query, fb.TS)
	if err != nil {
		return err
	}

	// Enforce cap by evicting oldest rows beyond 2000
	_, _ = pgExec(ctx, db,
		`DELETE FROM ai_feedback
                 WHERE id NOT IN (
                   SELECT id FROM ai_feedback ORDER BY timestamp DESC LIMIT 2000
                 )`)
	return nil
}

// aiFeedbackLoad fetches the most recent 1000 feedback entries.
func aiFeedbackLoad() []aiFeedbackEntry {
	aiFeedbackMu.Lock()
	defer aiFeedbackMu.Unlock()

	db, err := pgGet()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := pgQuery(ctx, db,
		`SELECT timestamp, chat, msg_id, user_feedback, ai_text
                 FROM ai_feedback
                 ORDER BY timestamp DESC
                 LIMIT 1000`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []aiFeedbackEntry
	for rows.Next() {
		var e aiFeedbackEntry
		if err := rows.Scan(&e.TS, &e.Chat, &e.MsgID, &e.Query, &e.Reply); err != nil {
			continue
		}
		list = append(list, e)
	}
	return list
}

// handleAIFeedbackCommand — .aifeedback [clear]
func handleAIFeedbackCommand(msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	args = strings.TrimSpace(strings.ToLower(args))
	if args == "clear" {
		aiFeedbackMu.Lock()
		db, err := pgGet()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = pgExec(ctx, db, `TRUNCATE TABLE ai_feedback RESTART IDENTITY`)
		}
		aiFeedbackMu.Unlock()
		return "🗑️ Feedback AI dikosongkan"
	}
	list := aiFeedbackLoad()
	if len(list) == 0 {
		return "🤖 *AI Feedback*\n\nBelum ada. React 🤖 ke pesan AI yang kedengeran kayak robot,\nbot akan mencatatnya untuk perbaikan."
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🤖 *AI Feedback* — %d tercatat\n\n", len(list)))
	// Analisis pola cepat: panjang rata-rata reply yang di-flag
	var totalLen int
	longCount := 0
	for _, e := range list {
		totalLen += len([]rune(e.Reply))
		if len([]rune(e.Reply)) > 150 {
			longCount++
		}
	}
	avgLen := totalLen / len(list)
	sb.WriteString(fmt.Sprintf("• Rata-rata panjang reply di-flag: %d char\n", avgLen))
	sb.WriteString(fmt.Sprintf("• Reply panjang (>150 char): %d\n\n", longCount))
	sb.WriteString("*Terbaru:*\n")
	start := 5
	if start > len(list) {
		start = len(list)
	}
	for i := 0; i < start; i++ {
		e := list[i]
		q := e.Query
		if len([]rune(q)) > 40 {
			q = string([]rune(q)[:40]) + "…"
		}
		r := e.Reply
		if len([]rune(r)) > 60 {
			r = string([]rune(r)[:60]) + "…"
		}
		ts := time.Unix(e.TS, 0).Format("02/01 15:04")
		sb.WriteString(fmt.Sprintf("• [%s] Q: %s\n  A: %s\n", ts, q, r))
	}
	return sb.String()
}

// keep filepath referenced (used elsewhere in original code; safe to noop now)
var _ = filepath.Join

func init() {
	RegisterPlugin(Plugin{
		Command:     "aifeedback",
		Aliases:     []string{"aifb"},
		Description: "aifeedback",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAIFeedbackCommand(msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "bottrain",
		Aliases:     []string{"btrain"},
		Description: "bottrain",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleBotTrainCommand(msg, args)
		},
	})
}
