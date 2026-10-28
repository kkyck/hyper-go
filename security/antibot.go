package main

// antibot.go — Antibot detection (enhanced, high accuracy)
//
// Deteksi bot/userbot dari berbagai aspek dengan weighted scoring:
//   1. Message ID pattern analysis
//   2. Bot menu/keyword density
//   3. Bot command prefixes (!, #, /, etc)
//   4. PushName analysis
//   5. Invite link detection
//   6. Menu structure (numbered/bullet lists)
//   7. Status indicators (uptime, ping, version)
//   8. Excessive emoji count
//   9. Message structure (consistent formatting)
//  10. Bot JID detection
//  11. Reaction/sticker patterns
//  12. Multi-line command listing
//  13. Bot advertisement patterns
//  14. Timestamp/bot metadata in text
//  15. Sender device analysis
//
// Actions:
//   - Bot admin: kirim peringatan + kick
//   - Bot bukan admin: react emoji 🤖
//
// Commands:
//   .antibot on/off/status
//   .cekbot (reply pesan)
//
// Storage: data/antibot.json

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── State ────────────────────────────────────────────────────────

var (
	antibotMu     sync.RWMutex
	antibotConfig = map[string]bool{}
	antibotLoaded bool
)

func antibotPath() string {
	return filepath.Join("data", "antibot.json")
}

func loadAntibot() {
	antibotMu.Lock()
	defer antibotMu.Unlock()
	if antibotLoaded {
		return
	}
	antibotLoaded = true
	data, err := os.ReadFile(antibotPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &antibotConfig)
}

func saveAntibot() {
	antibotMu.RLock()
	defer antibotMu.RUnlock()
	data, _ := json.MarshalIndent(antibotConfig, "", "  ")
	_ = os.MkdirAll("data", 0o755)
	_ = os.WriteFile(antibotPath(), data, 0o644)
}

func antibotEnabled(chat string) bool {
	antibotMu.RLock()
	defer antibotMu.RUnlock()
	return antibotConfig[chat]
}

func antibotSet(chat string, enabled bool) {
	antibotMu.Lock()
	defer antibotMu.Unlock()
	if enabled {
		antibotConfig[chat] = true
	} else {
		delete(antibotConfig, chat)
	}
	go saveAntibot()
}

// ─── Detection result ─────────────────────────────────────────────

type botDetectionResult struct {
	IsBot      bool
	Reasons    []string
	Confidence int
}

// ─── Detection patterns (curated for high accuracy) ───────────────

// Bot menu keywords — HIGH confidence indicators
var botMenuKeywords = map[string]int{
	// Menu/feature listing
	"menu bot":     40,
	"fitur bot":    40,
	"command list": 35,
	"daftar fitur": 35,
	"daftar menu":  35,
	"all menu":     30,
	"bot menu":     35,
	"main menu":    25,
	"owner menu":   30,
	"admin menu":   25,
	// Bot identity
	"owner bot":   25,
	"creator bot": 25,
	"sewa bot":    35,
	"sewa harga":  30,
	"premium bot": 30,
	"bot online":  20,
	"bot aktif":   20,
	"bot running": 25,
	"bot version": 25,
	"versi bot":   25,
	// Bot infrastructure
	"prefix":         15,
	"runtime":        20,
	"uptime":         25,
	"bot status":     20,
	"session active": 20,
	"sesi aktif":     20,
	// Bot unknown-command templates (dari analisis pesan bot nyata)
	"don't have that command":   35,
	"did you mean this command": 30,
	"tidak ada di dalam daftar": 35,
	"command yang lu maksud":    30,
	"command yang kamu maksud":  30,
	"unknown command":           30,
	"command not found":         30,
	// Template menu bot fancy (dari training 🤖)
	"🖥️ *server information*": 30,
	"┃ ◦":                     20,
	"┏━━":                     20,
	// Bot hosting
	"hosting":    15,
	"deploy":     15,
	"vps":        15,
	"server bot": 20,
	"panel bot":  20,
	// Bot advertisement
	"wa.me/":             30,
	"chat.whatsapp.com/": 25,
	"wa.link/":           25,
	"bit.ly/":            10,
	// Bot payment
	"dana":      15,
	"gopay":     15,
	"ovo":       15,
	"shopeepay": 15,
	"qris":      15,
	"transfer":  5,
	// Bot AI
	"ai bot":       25,
	"chatbot":      25,
	"auto reply":   20,
	"auto respond": 20,
}

// Bot command prefixes (non-dot)
var botPrefixes = map[string]int{
	"!": 20,
	"#": 20,
	"/": 20,
	"+": 15,
	"?": 10,
	">": 10,
	"<": 10,
	"$": 10,
	"%": 10,
	"&": 10,
	"*": 10,
	"-": 5,
}

// Suspicious PushName patterns
var suspiciousPushNamePatterns = []string{
	"bot", "ai ", " assistant", "auto", "machine",
	"robot", "android bot", "ios bot", "md bot",
	"wa bot", "whatsapp bot", "wa-bot", "whatsapp-bot",
	"auto reply", "auto respond", "chatbot",
	"official bot", "verified bot",
}

// Regex hot-path (dulu di-compile per panggilan — sekarang package-level)
var (
	reNumberedLine  = regexp.MustCompile(`^\d+[.)\]]`)
	reBulletLine    = regexp.MustCompile(`^[➤▸•◇◆○●▶▷▸▹►▭▬━─*]`)
	reBotRespStatus = regexp.MustCompile(`(?i)(✅|❌|⏳|🔄|⚙️).{0,20}(done|success|error|failed|loading|processing)`)
	reBotRespWord   = regexp.MustCompile(`(?i)(status|result|output).{0,10}(✅|❌|⏳|🔄)`)
	reJSONBlob      = regexp.MustCompile(`\{[^}]{20,}\}`)
	reHexOnly       = regexp.MustCompile(`^[0-9A-Fa-f]+$`)
	// Sinyal diferensial #16 (dari analisis 274 pesan bot vs 5017 manusia)
	reKVColon  = regexp.MustCompile(`^[A-Za-z][\w ]{1,25}\s+:\s+\S`)
	reBoldMD   = regexp.MustCompile(`\*[^*\n]{2,40}\*`)
	reCapsWord = regexp.MustCompile(`\b[A-Z]{4,}\b`)
	// Template khas bot (dari analisis pesan bot pada beberapa grup)
	reCmdSuggestArrow = regexp.MustCompile(`⇒\s*\*?\.`)
	reFailPrefix      = regexp.MustCompile(`(?i)^(failed|error|gagal)\s*:`)
)

// Bot ID patterns (WhatsApp message IDs from bots tend to be shorter/different)
var botIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^[0-9A-F]{8,12}$`), // hex 8-12 chars
	regexp.MustCompile(`^BOT[A-Z0-9]+$`),   // BOT prefix
	regexp.MustCompile(`^[a-z0-9]{20,}$`),  // random string 20+
}

// Menu structure patterns
var menuStructurePatterns = []*regexp.Regexp{
	// Numbered list: "1. text\n2. text\n3. text"
	regexp.MustCompile(`(?m)^\d+[.)]\s.+\n\d+[.)]\s.+\n\d+[.)]\s.+`),
	// Bullet list with emoji: "➤ text\n➤ text\n➤ text"
	regexp.MustCompile(`(?m)^[➤▸•◇◆○●▶▷▸▹►▭▬━─]\s.+\n[➤▸•◇◆○●▶▷▸▹►▭▬━─]\s.+\n[➤▸•◇◆○●▶▷▸▹►▭▬━─]\s.+`),
	// Category header pattern: "╔═══...═══╗"
	regexp.MustCompile(`╔═{3,}╗.*╚═{3,}╝`),
	// Separator lines: "━━━━━━━━━━"
	regexp.MustCompile(`━{5,}|═{5,}|─{5,}|■{5,}`),
}

// Bot advertisement patterns
var botAdPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(sewa|rent|buy|beli).{0,30}(bot|wa bot|whatsapp bot)`),
	regexp.MustCompile(`(?i)(bot).{0,30}(online|aktif|running|available)`),
	regexp.MustCompile(`(?i)(harga|price|biaya).{0,20}(bot|sewa|premium)`),
	regexp.MustCompile(`(?i)(hubungi|contact|admin|owner).{0,20}(wa\.me|whatsapp|chat)`),
}

// ─── Detection logic ─────────────────────────────────────────────

// checkAntibot runs on every incoming message to detect bots.
func checkAntibot(client *whatsmeow.Client, msg *events.Message) bool {
	if client == nil || msg == nil || !msg.Info.IsGroup {
		return false
	}

	loadAntibot()
	if !antibotEnabled(msg.Info.Chat.String()) {
		return false
	}

	if msg.Info.IsFromMe {
		return false
	}
	if isOwnerMsg(msg) {
		return false
	}
	if isGroupAdmin(client, msg) {
		return false
	}

	text := extractText(msg)
	raw := msg.RawMessage
	if raw == nil {
		raw = msg.Message
	}

	// ─── PARALLEL DETECTION: text + structure + logic + impossible concurrent ───
	textCh := make(chan botDetectionResult, 1)
	structCh := make(chan botDetectionResult, 1)
	logicCh := make(chan botDetectionResult, 1)
	impossibleCh := make(chan botDetectionResult, 1)

	// Text detection goroutine
	go func() {
		if text != "" {
			textCh <- detectBot(msg, text)
		} else {
			textCh <- botDetectionResult{}
		}
	}()

	// Structure detection goroutine
	go func() {
		structCh <- detectBotStructure(msg, raw)
	}()

	// Logic detection goroutine
	go func() {
		logicCh <- detectBotLogic(msg, raw, text)
	}()

	// Impossible-structure detection goroutine (decisive)
	go func() {
		impossibleCh <- detectBotImpossible(msg, raw)
	}()

	// Collect results
	result := <-textCh
	structResult := <-structCh
	logicResult := <-logicCh
	impossibleResult := <-impossibleCh

	// Merge impossible-structure results FIRST — decisive: any hit = bot
	if impossibleResult.Confidence > 0 {
		result.Reasons = append(result.Reasons, impossibleResult.Reasons...)
		result.Confidence += impossibleResult.Confidence
		if result.Confidence > 100 {
			result.Confidence = 100
		}
		if impossibleResult.IsBot {
			result.IsBot = true
		}
	}

	// Merge structure results
	if structResult.Confidence > 0 {
		result.Reasons = append(result.Reasons, structResult.Reasons...)
		result.Confidence += structResult.Confidence
		if result.Confidence > 100 {
			result.Confidence = 100
		}
		if structResult.IsBot {
			result.IsBot = true
		}
	}

	// Merge logic results
	if logicResult.Confidence > 0 {
		result.Reasons = append(result.Reasons, logicResult.Reasons...)
		result.Confidence += logicResult.Confidence
		if result.Confidence > 100 {
			result.Confidence = 100
		}
		if logicResult.IsBot {
			result.IsBot = true
		}
	}

	// Final verdict: if ANY detection category says bot, OR total confidence >= 50
	if !result.IsBot && result.Confidence >= 50 {
		result.IsBot = true
	}

	// Cross-category boost: if logic + structure both suspicious but below threshold
	if !result.IsBot && logicResult.Confidence > 0 && structResult.Confidence > 0 {
		result.IsBot = result.Confidence >= 35
	}

	// TIGHTENED: partial impossible-structure signal + any corroboration = bot.
	// Even a weak impossible signal (below decisive 60) is strong evidence
	// when another category independently flags the message.
	if !result.IsBot && impossibleResult.Confidence > 0 &&
		(result.Confidence-impossibleResult.Confidence) >= 20 {
		result.IsBot = true
		result.Reasons = append(result.Reasons, "Corroborated impossible-structure signal")
	}

	if !result.IsBot {
		return false
	}

	go antibotPunish(client, msg, result)
	return false
}

// detectBot performs comprehensive bot detection with weighted scoring.
// senderLatencyProfile menghitung profil kecepatan respon seorang pengirim:
// latency = jeda antara pesan orang lain dan pesan dia di grup yang sama.
// Return: median detik, fraksi respon ≤3 detik, jumlah sampel.
func senderLatencyProfile(senderPhone string) (medianSec, pctUnder3 float64, n int) {
	if senderPhone == "" {
		return 0, 0, 0
	}
	msgArchiveMu.RLock()
	defer msgArchiveMu.RUnlock()

	// Kelompokkan per chat, urutkan by timestamp
	byChat := make(map[string][]archivedMessage)
	for _, m := range msgArchive {
		byChat[m.GroupJID] = append(byChat[m.GroupJID], m)
	}
	var lats []float64
	for _, msgs := range byChat {
		// insertion sort cukup (arsip sudah mostly terurut)
		for i := 1; i < len(msgs); i++ {
			for j := i; j > 0 && msgs[j].Timestamp < msgs[j-1].Timestamp; j-- {
				msgs[j], msgs[j-1] = msgs[j-1], msgs[j]
			}
		}
		for i := 1; i < len(msgs); i++ {
			prev, cur := msgs[i-1], msgs[i]
			if extractPhone(cur.Sender) != senderPhone {
				continue
			}
			if extractPhone(prev.Sender) == senderPhone {
				continue
			}
			dt := float64(cur.Timestamp - prev.Timestamp)
			if dt > 0 && dt <= 3600 {
				lats = append(lats, dt)
			}
		}
	}
	n = len(lats)
	if n == 0 {
		return 0, 0, 0
	}
	sort.Float64s(lats)
	if n%2 == 1 {
		medianSec = lats[n/2]
	} else {
		medianSec = (lats[n/2-1] + lats[n/2]) / 2
	}
	var under3 int
	for _, v := range lats {
		if v <= 3 {
			under3++
		}
	}
	pctUnder3 = float64(under3) / float64(n)
	return medianSec, pctUnder3, n
}

func detectBot(msg *events.Message, text string) botDetectionResult {
	var reasons []string
	confidence := 0

	sender := msg.Info.Sender

	// ─── 1. Bot JID detection ───
	if sender.Server == "bot" {
		reasons = append(reasons, "Sender is a bot JID (@bot)")
		confidence += 50
	}

	// ─── 2. Message ID pattern analysis ───
	msgID := msg.Info.ID
	for _, pattern := range botIDPatterns {
		if pattern.MatchString(msgID) {
			reasons = append(reasons, fmt.Sprintf("Bot-like message ID: %s", msgID))
			confidence += 15
			break
		}
	}

	// ─── 3. Bot menu/keyword density ───
	lowerText := strings.ToLower(text)
	keywordScore := 0
	matchedKeywords := []string{}
	for keyword, score := range botMenuKeywords {
		if strings.Contains(lowerText, keyword) {
			keywordScore += score
			matchedKeywords = append(matchedKeywords, keyword)
		}
	}
	if keywordScore > 0 {
		// Cap keyword score at 60
		if keywordScore > 60 {
			keywordScore = 60
		}
		confidence += keywordScore
		reasons = append(reasons, fmt.Sprintf("Bot keywords (%d pts): %s", keywordScore, strings.Join(matchedKeywords, ", ")))
	}

	// ─── 4. Bot command prefixes ───
	for prefix, score := range botPrefixes {
		if strings.HasPrefix(text, prefix) {
			rest := strings.TrimPrefix(text, prefix)
			// Check if it looks like a command (short word after prefix)
			fields := strings.Fields(rest)
			if len(fields) > 0 && len(fields[0]) > 0 && len(fields[0]) < 25 {
				// Verify it's not just punctuation
				isWord := false
				for _, c := range fields[0] {
					if unicode.IsLetter(c) {
						isWord = true
						break
					}
				}
				if isWord {
					confidence += score
					reasons = append(reasons, fmt.Sprintf("Bot prefix: %s%s", prefix, fields[0]))
					break
				}
			}
		}
	}

	// ─── 5. PushName analysis ───
	pushName := msg.Info.PushName
	if pushName != "" {
		lowerPush := strings.ToLower(pushName)
		for _, pattern := range suspiciousPushNamePatterns {
			if strings.Contains(lowerPush, pattern) {
				reasons = append(reasons, fmt.Sprintf("Suspicious PushName: %q", pushName))
				confidence += 20
				break
			}
		}
		// Check if PushName is very short (bots often have short names)
		if len(pushName) <= 3 && pushName != "" {
			confidence += 5
		}
	}

	// ─── 6. Menu structure patterns ───
	structureMatched := 0
	for _, pattern := range menuStructurePatterns {
		if pattern.MatchString(text) {
			structureMatched++
		}
	}
	if structureMatched > 0 {
		score := 25 * structureMatched
		if score > 50 {
			score = 50
		}
		confidence += score
		reasons = append(reasons, fmt.Sprintf("Menu structure pattern (%d matches, %d pts)", structureMatched, score))
	}

	// ─── 7. Bot advertisement patterns ───
	adMatched := 0
	for _, pattern := range botAdPatterns {
		if pattern.MatchString(text) {
			adMatched++
		}
	}
	if adMatched > 0 {
		score := 20 * adMatched
		if score > 40 {
			score = 40
		}
		confidence += score
		reasons = append(reasons, fmt.Sprintf("Bot advertisement pattern (%d matches, %d pts)", adMatched, score))
	}

	// ─── 8. Excessive emoji count ───
	emojiCount := countEmojis(text)
	if emojiCount >= 15 {
		reasons = append(reasons, fmt.Sprintf("Excessive emoji: %d", emojiCount))
		confidence += 25
	} else if emojiCount >= 8 {
		confidence += 10
	}

	// ─── 9. Line count + structure ───
	lines := strings.Split(text, "\n")
	if len(lines) >= 15 {
		// Check for consistent formatting (numbered or bulleted)
		numberedLines := 0
		bulletLines := 0
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if reNumberedLine.MatchString(trimmed) {
				numberedLines++
			} else if reBulletLine.MatchString(trimmed) {
				bulletLines++
			}
		}
		if numberedLines >= 5 || bulletLines >= 5 {
			score := 30
			if numberedLines >= 10 || bulletLines >= 10 {
				score = 40
			}
			confidence += score
			reasons = append(reasons, fmt.Sprintf("Structured list: %d numbered, %d bullet (%d lines total)", numberedLines, bulletLines, len(lines)))
		}
	}

	// ─── 10. Separator/decoration patterns ───
	separatorCount := strings.Count(text, "━") + strings.Count(text, "═") +
		strings.Count(text, "─") + strings.Count(text, "■") +
		strings.Count(text, "╗") + strings.Count(text, "╔") +
		strings.Count(text, "╚") + strings.Count(text, "╝")
	if separatorCount >= 4 {
		reasons = append(reasons, fmt.Sprintf("Decoration/separators: %d chars", separatorCount))
		confidence += 20
	}

	// ─── 11. Multiple command prefixes in one message ───
	prefixCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		for prefix := range botPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				// Check if followed by a word
				rest := strings.TrimPrefix(trimmed, prefix)
				fields := strings.Fields(rest)
				if len(fields) > 0 && len(fields[0]) > 0 && len(fields[0]) < 25 {
					prefixCount++
					break
				}
			}
		}
	}
	if prefixCount >= 3 {
		reasons = append(reasons, fmt.Sprintf("Multiple commands: %d lines with bot prefixes", prefixCount))
		confidence += 25
	}

	// ─── 12. Bot metadata in text ───
	botMetaPatterns := []string{
		"message_id", "msg_id", "chat_id", "user_id",
		"session_id", "bot_id", "client_id",
		"timestamp", "unix_time", "epoch",
	}
	for _, meta := range botMetaPatterns {
		if strings.Contains(lowerText, meta) {
			reasons = append(reasons, fmt.Sprintf("Bot metadata: %s", meta))
			confidence += 15
			break
		}
	}

	// ─── 13. Fake message patterns ───
	// Bots sometimes include fake "from" or "sender" in text
	if strings.Contains(lowerText, "from:") && strings.Contains(lowerText, "sender:") {
		reasons = append(reasons, "Fake sender metadata in text")
		confidence += 15
	}

	// ─── 14. Check for typical bot response format ───
	// "✅ Done", "❌ Error", "⏳ Loading" patterns
	botResponsePatterns := []*regexp.Regexp{
		reBotRespStatus,
		reBotRespWord,
	}
	for _, pattern := range botResponsePatterns {
		if pattern.MatchString(text) {
			reasons = append(reasons, "Bot response format pattern")
			confidence += 15
			break
		}
	}

	// ─── 15. Check for JSON/code patterns (bots often output raw data) ───
	jsonPattern := reJSONBlob
	if jsonPattern.MatchString(text) {
		codeCount := strings.Count(text, "{") + strings.Count(text, "}")
		if codeCount >= 4 {
			reasons = append(reasons, "JSON/code output pattern")
			confidence += 15
		}
	}

	// ─── 16. Differential text-style signals (dari analisis 274 pesan bot vs 5017 manusia) ───
	// KV colon "Label : value" → 14.3x lebih sering di bot
	kvColon := 0
	for _, line := range lines {
		if reKVColon.MatchString(strings.TrimSpace(line)) {
			kvColon++
		}
	}
	if kvColon >= 2 {
		confidence += 25
		reasons = append(reasons, fmt.Sprintf("KV colon headers (%d baris, gaya output bot)", kvColon))
	} else if kvColon == 1 {
		confidence += 10
		reasons = append(reasons, "KV colon header (gaya output bot)")
	}
	// Code block ``` → 7.7x lebih sering di bot
	if strings.Contains(text, "```") {
		confidence += 15
		reasons = append(reasons, "Code block (gaya output bot)")
	}
	// Bold markdown *x* → 4.9x lebih sering di bot
	boldCount := len(reBoldMD.FindAllString(text, -1))
	if boldCount >= 3 {
		confidence += 15
		reasons = append(reasons, fmt.Sprintf("Bold markdown berlebihan (%d)", boldCount))
	}
	// ALL CAPS words → 4.5x lebih sering di bot
	capsWords := len(reCapsWord.FindAllString(text, -1))
	if capsWords >= 3 {
		confidence += 10
		reasons = append(reasons, fmt.Sprintf("ALL CAPS words (%d)", capsWords))
	}
	// Pesan sangat panjang → 5.9x lebih sering di bot (manusia jarang >300 char)
	if len([]rune(text)) > 500 {
		confidence += 15
		reasons = append(reasons, "Pesan sangat panjang (>500 char)")
	} else if len([]rune(text)) > 300 {
		confidence += 8
		reasons = append(reasons, "Pesan panjang (>300 char)")
	}
	// Bullet list (tanpa syarat 15 baris) → 13x lebih sering di bot
	bulletLines := 0
	for _, line := range lines {
		if reBulletLine.MatchString(strings.TrimSpace(line)) {
			bulletLines++
		}
	}
	if bulletLines >= 3 {
		confidence += 20
		reasons = append(reasons, fmt.Sprintf("Bullet list (%d baris)", bulletLines))
	}
	// Template saran command khas bot: "⇒ *.xxx*"
	if reCmdSuggestArrow.MatchString(text) {
		confidence += 15
		reasons = append(reasons, "Command suggestion template (⇒ *.cmd)")
	}
	// Awalan error teknis khas bot: "Failed:", "Error:", "Gagal:"
	if reFailPrefix.MatchString(strings.TrimSpace(text)) {
		confidence += 12
		reasons = append(reasons, "Technical error prefix (Failed:/Error:/Gagal:)")
	}
	// ─── 18. Gaya menu bot fancy-unicode (dari training 🤖) ───
	// Box menu ┏┛┃ → 10.6x lebih sering di bot
	boxCount := 0
	for _, r := range text {
		if r == '┏' || r == '┗' || r == '┃' || r == '┛' || r == '┓' ||
			r == '╭' || r == '╰' || r == '╮' || r == '╯' || r == '⊷' {
			boxCount++
		}
	}
	if boxCount >= 3 {
		confidence += 15
		reasons = append(reasons, fmt.Sprintf("Box-drawing menu (%d chars)", boxCount))
	}
	// Bullet menu khas bot: ▨ (5.8x), ❍
	menuBullet := strings.Count(text, "▨") + strings.Count(text, "❍")
	if menuBullet >= 2 {
		confidence += 12
		reasons = append(reasons, fmt.Sprintf("Bullet menu khas bot (%d)", menuBullet))
	}
	// Small-caps unicode (sᴛᴀᴛᴜs, ʙᴏᴛ) → 3.5x; math bold (𝕾𝖆𝖑𝖆𝖒) → 5.9x
	var smallCaps, fancyMath int
	for _, r := range text {
		if r >= 0x1D00 && r <= 0x1D7F {
			smallCaps++
		} else if r >= 0x1D400 && r <= 0x1D7FF {
			fancyMath++
		}
	}
	if smallCaps >= 4 {
		confidence += 10
		reasons = append(reasons, fmt.Sprintf("Small-caps unicode (%d chars)", smallCaps))
	}
	if fancyMath >= 4 {
		confidence += 10
		reasons = append(reasons, fmt.Sprintf("Math-bold unicode (%d chars)", fancyMath))
	}

	// ─── 17. Kecepatan respon terhadap trigger ───
	// Bot merespon kilat & konsisten; manusia bervariasi.
	// (dari analisis: 67% bot punya >70% respon ≤3 dtk vs 20% manusia)
	if med, p3, nlat := senderLatencyProfile(extractPhone(sender.String())); nlat >= 5 {
		if p3 > 0.7 && med <= 3 {
			confidence += 20
			reasons = append(reasons, fmt.Sprintf("Respons kilat & konsisten (%.0f%% ≤3 dtk, median %.0fs, %d sampel)", p3*100, med, nlat))
		} else if p3 > 0.5 && med <= 5 {
			confidence += 12
			reasons = append(reasons, fmt.Sprintf("Respons cepat (%.0f%% ≤3 dtk, median %.0fs)", p3*100, med))
		} else if med <= 10 && nlat >= 10 {
			confidence += 8
			reasons = append(reasons, fmt.Sprintf("Cenderung respons cepat (median %.0fs)", med))
		}
	}

	// Cap confidence at 100

	if confidence > 100 {
		confidence = 100
	}

	// ─── Determine verdict ───
	// High threshold for accuracy: 50+ = bot
	isBot := confidence >= 50

	return botDetectionResult{
		IsBot:      isBot,
		Reasons:    reasons,
		Confidence: confidence,
	}
}

// countEmojis counts emoji characters in text.
func countEmojis(text string) int {
	count := 0
	for _, r := range text {
		if (r >= 0x1F000 && r <= 0x1FFFF) || // emoji
			(r >= 0x2600 && r <= 0x27BF) || // misc symbols
			(r >= 0x2B00 && r <= 0x2BFF) || // arrows
			(r >= 0xFE00 && r <= 0xFE0F) || // variation selectors
			(r >= 0x1F900 && r <= 0x1F9FF) { // supplemental symbols
			count++
		}
	}
	return count
}

// ─── Punish ──────────────────────────────────────────────────────

func antibotPunish(client *whatsmeow.Client, msg *events.Message, result botDetectionResult) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[antibot] panic: %v\n", r)
		}
	}()

	sender := msg.Info.Sender
	senderPhone := extractPhone(sender.String())
	// Cek status admin BOT SENDIRI (bukan pengirim) — kick butuh bot admin
	botIsAdmin := isBotGroupAdmin(client, msg.Info.Chat)

	if botIsAdmin {
		// Bot admin — warning + kick
		warning := fmt.Sprintf("🤖 *Bot Terdeteksi!*\n\n"+
			"User: @%s\n"+
			"Confidence: %d%%\n"+
			"Alasan:\n%s\n\n"+
			"User telah dikeluarkan dari grup.",
			senderPhone, result.Confidence, strings.Join(result.Reasons, "\n"))

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		waMsg := &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: &warning,
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{sender.String()},
				},
			},
		}
		_, _ = client.SendMessage(ctx, msg.Info.Chat, waMsg)

		// Kick
		kickCtx, kickCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer kickCancel()
		forms := dualForms(client, sender)
		if len(forms) == 0 {
			forms = []types.JID{sender}
		}
		for _, f := range forms {
			_, err := client.UpdateGroupParticipants(kickCtx, msg.Info.Chat, []types.JID{f}, whatsmeow.ParticipantChangeRemove)
			if err == nil {
				fmt.Printf("[antibot] ✅ Kicked %s (confidence %d%%)\n", senderPhone, result.Confidence)
				return
			}
		}
		fmt.Printf("[antibot] ⚠️ Failed to kick %s\n", senderPhone)
	} else {
		// Bot not admin — react 🤖
		reactCtx, reactCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer reactCancel()
		reactMsg := &waE2E.Message{
			ReactionMessage: &waE2E.ReactionMessage{
				Key: &waCommon.MessageKey{
					RemoteJID: proto.String(msg.Info.Chat.String()),
					FromMe:    proto.Bool(msg.Info.IsFromMe),
					ID:        proto.String(msg.Info.ID),
				},
				Text: proto.String("🤖"),
			},
		}
		_, _ = client.SendMessage(reactCtx, msg.Info.Chat, reactMsg)
		fmt.Printf("[antibot] 🤖 Reacted to %s (confidence %d%%)\n", senderPhone, result.Confidence)
	}
}

// ─── Command handlers ────────────────────────────────────────────

func handleAntibotCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner/admin grup"
	}

	loadAntibot()
	sub := strings.ToLower(strings.TrimSpace(args))
	chat := msg.Info.Chat.String()

	switch sub {
	case "on", "enable", "aktif":
		antibotSet(chat, true)
		botAdmin := isBotGroupAdmin(client, msg.Info.Chat)
		action := "react 🤖"
		if botAdmin {
			action = "warning + kick"
		}
		return fmt.Sprintf("✅ *Antibot AKTIF*\n\n• Detection: 4-tier (impossible-structure DECISIVE + text/structure/logic), weighted scoring\n• Threshold: 50%% confidence (60%% impossible-structure = instant bot)\n• Action: %s", action)

	case "off", "disable", "mati":
		antibotSet(chat, false)
		return "🔴 Antibot dimatikan di grup ini"

	case "status", "stat":
		enabled := antibotEnabled(chat)
		status := "🔴 OFF"
		if enabled {
			status = "🟢 ON"
		}
		botAdmin := isBotGroupAdmin(client, msg.Info.Chat)
		adminStatus := "❌ Bukan admin (react only)"
		if botAdmin {
			adminStatus = "✅ Admin (kick mode)"
		}
		return fmt.Sprintf("🤖 *Antibot Status*\n\n• Status: *%s*\n• Bot admin: %s\n• Threshold: 50%% (60%% impossible-structure = instant)\n• Detection: 4-tier", status, adminStatus)

	default:
		return `🤖 *Antibot*

Deteksi bot 4 lapis (impossible-structure = vonis langsung):
1. Bot JID detection
2. Message ID pattern
3. Bot keywords (60+ patterns)
4. Command prefixes (!, #, /, etc)
5. PushName analysis
6. Menu structure patterns
7. Bot advertisement
8. Excessive emoji (15+)
9. Structured lists (5+ lines)
10. Decoration/separators
11. Multiple commands in message
12. Bot metadata in text
13. Fake sender patterns
14. Bot response format
15. JSON/code output

Threshold: 50% confidence
Action: admin→kick, non-admin→react

*Commands:*
• .antibot on — aktifkan
• .antibot off — matikan
• .antibot status — cek status
• .cekbot — cek manual (reply pesan)`
	}
}

// handleCekBotCommand — .cekbot (reply pesan)
func handleCekBotCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	if !isOwnerMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner/admin grup"
	}

	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❌ Reply pesan yang ingin di-cek"
	}

	text := extractText(quoted)
	raw := quoted.RawMessage
	if raw == nil {
		raw = quoted.Message
	}

	// Allow cekbot even without text (structure/logic can detect bots)
	if text == "" && raw == nil {
		return "❌ Tidak bisa ambil data pesan"
	}

	result := detectBot(quoted, text)

	// Also check structure and logic
	if raw != nil {
		// Impossible-structure check (decisive — runs first)
		impossibleResult := detectBotImpossible(quoted, raw)
		if impossibleResult.Confidence > 0 {
			result.Reasons = append(result.Reasons, impossibleResult.Reasons...)
			result.Confidence += impossibleResult.Confidence
			if result.Confidence > 100 {
				result.Confidence = 100
			}
			if impossibleResult.IsBot {
				result.IsBot = true
			}
		}
		structResult := detectBotStructure(quoted, raw)
		if structResult.Confidence > 0 {
			result.Reasons = append(result.Reasons, structResult.Reasons...)
			result.Confidence += structResult.Confidence
			if result.Confidence > 100 {
				result.Confidence = 100
			}
			if structResult.IsBot {
				result.IsBot = true
			}
		}
		logicResult := detectBotLogic(quoted, raw, text)
		if logicResult.Confidence > 0 {
			result.Reasons = append(result.Reasons, logicResult.Reasons...)
			result.Confidence += logicResult.Confidence
			if result.Confidence > 100 {
				result.Confidence = 100
			}
			if logicResult.IsBot {
				result.IsBot = true
			}
		}
		// Final verdict: if ANY detection says bot, OR total confidence >= 50
		if !result.IsBot && result.Confidence >= 50 {
			result.IsBot = true
		}
		// Cross-category boost
		if !result.IsBot && logicResult.Confidence > 0 && structResult.Confidence > 0 {
			result.IsBot = result.Confidence >= 35
		}
		// TIGHTENED: partial impossible-structure signal + corroboration = bot
		if !result.IsBot && impossibleResult.Confidence > 0 &&
			(result.Confidence-impossibleResult.Confidence) >= 20 {
			result.IsBot = true
			result.Reasons = append(result.Reasons, "Corroborated impossible-structure signal")
		}
	}

	senderPhone := extractPhone(quoted.Info.Sender.String())

	var b strings.Builder
	b.WriteString("🤖 *Cek Bot Analysis*\n\n")
	b.WriteString(fmt.Sprintf("• User: @%s\n", senderPhone))
	b.WriteString(fmt.Sprintf("• PushName: %s\n", quoted.Info.PushName))
	b.WriteString(fmt.Sprintf("• Message ID: %s\n", quoted.Info.ID))
	b.WriteString(fmt.Sprintf("• Confidence: %d%%\n", result.Confidence))
	verdict := "✅ HUMAN"
	if result.IsBot {
		verdict = "🤖 BOT"
	}
	b.WriteString(fmt.Sprintf("• Verdict: %s\n", verdict))
	if len(result.Reasons) > 0 {
		b.WriteString("\n*Detection details:*\n")
		for _, r := range result.Reasons {
			b.WriteString(fmt.Sprintf("• %s\n", r))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	waMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(b.String()),
			ContextInfo: &waE2E.ContextInfo{
				MentionedJID:  []string{quoted.Info.Sender.String()},
				StanzaID:      proto.String(msg.Info.ID),
				Participant:   proto.String(msg.Info.Sender.String()),
				QuotedMessage: msg.Message,
			},
		},
	}
	_, _ = client.SendMessage(ctx, msg.Info.Chat, waMsg)
	return ""
}

// antibotUnwrap peels ViewOnce/Ephemeral/DeviceSent wrappers to get the inner
// content message for structural analysis. Returns the innermost message and
// flags for which wrappers were present.
func antibotUnwrap(raw *waE2E.Message) (inner *waE2E.Message, wasViewOnce bool, wasEphemeral bool) {
	inner = raw
	for i := 0; i < 5 && inner != nil; i++ {
		if v := inner.GetViewOnceMessage(); v != nil && v.GetMessage() != nil {
			wasViewOnce = true
			inner = v.GetMessage()
			continue
		}
		if v := inner.GetViewOnceMessageV2(); v != nil && v.GetMessage() != nil {
			wasViewOnce = true
			inner = v.GetMessage()
			continue
		}
		if e := inner.GetEphemeralMessage(); e != nil && e.GetMessage() != nil {
			wasEphemeral = true
			inner = e.GetMessage()
			continue
		}
		if d := inner.GetDeviceSentMessage(); d != nil && d.GetMessage() != nil {
			inner = d.GetMessage()
			continue
		}
		break
	}
	return inner, wasViewOnce, wasEphemeral
}

// antibotGetContextInfo extracts ContextInfo from the inner message regardless
// of content type.
func antibotGetContextInfo(inner *waE2E.Message) *waE2E.ContextInfo {
	if inner == nil {
		return nil
	}
	if ext := inner.GetExtendedTextMessage(); ext != nil {
		return ext.GetContextInfo()
	}
	if img := inner.GetImageMessage(); img != nil {
		return img.GetContextInfo()
	}
	if vid := inner.GetVideoMessage(); vid != nil {
		return vid.GetContextInfo()
	}
	if aud := inner.GetAudioMessage(); aud != nil {
		return aud.GetContextInfo()
	}
	if doc := inner.GetDocumentMessage(); doc != nil {
		return doc.GetContextInfo()
	}
	if stk := inner.GetStickerMessage(); stk != nil {
		return nil // stickers don't carry ContextInfo with forward flags
	}
	return nil
}

// detectBotImpossible checks for protobuf structures that official WhatsApp
// clients CANNOT produce. These are decisive: any single hit = bot.
//
// Verified against hypermeow source (send.go, BuildEdit):
//  1. Bare top-level ProtocolMessage — official clients ALWAYS wrap protocol
//     messages as EditedMessage -> FutureProofMessage -> Message.ProtocolMessage
//     (see BuildEdit). A bare ProtocolMessage is only sent by userbots/exploits.
//  2. Multiple conflicting content types — official clients set exactly ONE
//     content field per message (conversation XOR imageMessage XOR ...).
//  3. BotMetadata on incoming message — official human clients never attach
//     bot metadata to their sends (hypermeow only sets it in bot mode).
//  4. Completely empty message — official clients always include content.
func detectBotImpossible(msg *events.Message, raw *waE2E.Message) botDetectionResult {
	var reasons []string
	confidence := 0

	if raw == nil {
		return botDetectionResult{}
	}

	// ─── 1. Bare top-level ProtocolMessage (not wrapped) ───
	// Official: Message.EditedMessage (FutureProofMessage) -> Message.ProtocolMessage
	// Userbot/exploit: Message.ProtocolMessage directly (like dmsg trick)
	if raw.GetProtocolMessage() != nil && raw.GetEditedMessage() == nil {
		reasons = append(reasons, "Bare ProtocolMessage (official clients always wrap in EditedMessage/FutureProofMessage)")
		confidence += 80
	}

	// ─── 2. Multiple conflicting content types ───
	// Count actual content fields (not metadata like MessageContextInfo)
	contentCount := 0
	contentNames := []string{}
	checkContent := func(name string, present bool) {
		if present {
			contentCount++
			contentNames = append(contentNames, name)
		}
	}
	checkContent("conversation", raw.GetConversation() != "")
	checkContent("extendedTextMessage", raw.GetExtendedTextMessage() != nil)
	checkContent("imageMessage", raw.GetImageMessage() != nil)
	checkContent("videoMessage", raw.GetVideoMessage() != nil)
	checkContent("audioMessage", raw.GetAudioMessage() != nil)
	checkContent("stickerMessage", raw.GetStickerMessage() != nil)
	checkContent("documentMessage", raw.GetDocumentMessage() != nil)
	checkContent("contactMessage", raw.GetContactMessage() != nil)
	checkContent("locationMessage", raw.GetLocationMessage() != nil)
	checkContent("liveLocationMessage", raw.GetLiveLocationMessage() != nil)
	checkContent("pollCreationMessage", raw.GetPollCreationMessage() != nil)
	checkContent("pollCreationMessageV2", raw.GetPollCreationMessageV2() != nil)
	checkContent("pollCreationMessageV3", raw.GetPollCreationMessageV3() != nil)
	if contentCount > 1 {
		reasons = append(reasons, fmt.Sprintf("Multiple content types (%d): %s (official clients send exactly one)", contentCount, strings.Join(contentNames, ", ")))
		confidence += 70
	}

	// ─── 3. BotMetadata on incoming message ───
	// Official human clients never set this; only bots/bot-mode sends do.
	if raw.GetMessageContextInfo() != nil && raw.GetMessageContextInfo().GetBotMetadata() != nil {
		reasons = append(reasons, "MessageContextInfo.BotMetadata present (only bots set this)")
		confidence += 70
	}

	// ─── 4. Completely empty message ───
	// No content, no protocol message, no wrappers at all.
	if contentCount == 0 && raw.GetProtocolMessage() == nil &&
		raw.GetEditedMessage() == nil && raw.GetEphemeralMessage() == nil &&
		raw.GetViewOnceMessage() == nil && raw.GetDeviceSentMessage() == nil &&
		raw.GetSenderKeyDistributionMessage() == nil {
		reasons = append(reasons, "Completely empty message (no content fields)")
		confidence += 60
	}

	// ─── 5. View-once + forwarded ───
	// Official clients BLOCK forwarding of view-once media. A view-once
	// message carrying forward flags is impossible via official clients.
	inner, wasViewOnce, _ := antibotUnwrap(raw)
	if wasViewOnce && inner != nil {
		if ci := antibotGetContextInfo(inner); ci != nil {
			if ci.GetIsForwarded() || ci.GetForwardingScore() > 0 {
				reasons = append(reasons, "View-once message with forward flags (official clients block forwarding view-once)")
				confidence += 80
			}
		}
	}

	// ─── 6. Future timestamp ───
	// Official clients stamp messages with current time. A timestamp more
	// than 5 minutes in the future is impossible (modulo clock skew).
	if ts := msg.Info.Timestamp; !ts.IsZero() {
		if time.Until(ts) > 5*time.Minute {
			reasons = append(reasons, fmt.Sprintf("Message timestamp in the future: %s", ts.Format(time.RFC3339)))
			confidence += 70
		}
		// Timestamp before WhatsApp existed (2009) = impossible
		if ts.Year() < 2009 {
			reasons = append(reasons, fmt.Sprintf("Message timestamp before WhatsApp existed: %s", ts.Format(time.RFC3339)))
			confidence += 70
		}
	}

	// ─── 7. Poll option count ───
	// Official clients enforce 2-12 poll options. 0/1 or >12 = impossible.
	checkPoll := func(name string, opts []string) {
		n := len(opts)
		if n > 0 && (n < 2 || n > 12) {
			reasons = append(reasons, fmt.Sprintf("%s with %d options (official: 2-12)", name, n))
			confidence += 70
		}
	}
	if inner != nil {
		if p := inner.GetPollCreationMessage(); p != nil {
			opts := make([]string, 0, len(p.GetOptions()))
			for _, o := range p.GetOptions() {
				opts = append(opts, o.GetOptionName())
			}
			checkPoll("PollCreationMessage", opts)
		}
		if p := inner.GetPollCreationMessageV2(); p != nil {
			opts := make([]string, 0, len(p.GetOptions()))
			for _, o := range p.GetOptions() {
				opts = append(opts, o.GetOptionName())
			}
			checkPoll("PollCreationMessageV2", opts)
		}
		if p := inner.GetPollCreationMessageV3(); p != nil {
			opts := make([]string, 0, len(p.GetOptions()))
			for _, o := range p.GetOptions() {
				opts = append(opts, o.GetOptionName())
			}
			checkPoll("PollCreationMessageV3", opts)
		}
	}

	// ─── 8. LiveLocation invalid coordinates ───
	// Latitude must be [-90, 90], longitude [-180, 180]. Outside = impossible.
	if inner != nil {
		if loc := inner.GetLiveLocationMessage(); loc != nil {
			lat, lng := loc.GetDegreesLatitude(), loc.GetDegreesLongitude()
			if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
				reasons = append(reasons, fmt.Sprintf("LiveLocation invalid coords: lat=%f lng=%f", lat, lng))
				confidence += 70
			}
		}
		if loc := inner.GetLocationMessage(); loc != nil {
			lat, lng := loc.GetDegreesLatitude(), loc.GetDegreesLongitude()
			if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
				reasons = append(reasons, fmt.Sprintf("Location invalid coords: lat=%f lng=%f", lat, lng))
				confidence += 70
			}
		}
	}

	// ─── 9. Empty ContactMessage ───
	// Official clients always include a display name or vCard. Both empty = impossible.
	if inner != nil {
		if c := inner.GetContactMessage(); c != nil {
			if c.GetDisplayName() == "" && c.GetVcard() == "" {
				reasons = append(reasons, "ContactMessage with no display name and no vCard")
				confidence += 60
			}
		}
	}

	if confidence > 100 {
		confidence = 100
	}

	// Decisive: any impossible structure = bot
	isBot := confidence >= 60

	return botDetectionResult{
		IsBot:      isBot,
		Reasons:    reasons,
		Confidence: confidence,
	}
}

// detectBotStructure analyzes the WhatsApp message structure (protobuf fields)
// to detect bot-like patterns that text analysis alone can't catch.
//
// Checks:
//  1. InteractiveMessage (buttons, lists, native flow) — bots often use these
//  2. ButtonsMessage — old-style buttons (bot indicator)
//  3. ListMessage — list menus (bot indicator)
//  4. TemplateMessage — template messages (bot indicator)
//  5. PollCreationMessage — polls (sometimes used by bots)
//  6. BotInvokeMessage — explicit bot invoke (strong indicator)
//  7. SecretEncryptedMessage — secret edits (bot/userbot indicator)
//  8. DeviceSentMessage — sent from linked device (bot indicator)
//  9. MessageContextInfo.BotMetadata — explicit bot metadata (strongest)
//  10. GroupInviteMessage — invite links (bot spam)
//  11. NewsletterAdminInviteMessage — newsletter invites (bot spam)
//  12. Multiple message types in one (unusual for humans)
//  13. HighlyStructuredMessage — HSM (bot indicator)
//  14. StickerMessage from non-admin (potential bot sticker)
//  15. ContextInfo patterns (quoted bot messages, forwarding score)
func detectBotStructure(msg *events.Message, raw *waE2E.Message) botDetectionResult {
	var reasons []string
	confidence := 0

	if raw == nil {
		return botDetectionResult{}
	}

	// ─── 1. BotInvokeMessage — explicit bot invoke ───
	if raw.GetBotInvokeMessage() != nil {
		reasons = append(reasons, "BotInvokeMessage (explicit bot invoke)")
		confidence += 60
	}

	// ─── 2. MessageContextInfo.BotMetadata — explicit bot metadata ───
	// (Also checked decisively in detectBotImpossible; kept here for scoring.)
	if mci := raw.GetMessageContextInfo(); mci != nil {
		if mci.GetBotMetadata() != nil {
			reasons = append(reasons, "MessageContextInfo.BotMetadata (bot metadata present)")
			confidence += 40
		}
	}

	// ─── 3. InteractiveMessage — buttons/lists/native flow ───
	// NOTE: official business accounts CAN send these legitimately,
	// so weight is low (soft signal only).
	if raw.GetInteractiveMessage() != nil {
		interactive := raw.GetInteractiveMessage()
		if interactive.GetNativeFlowMessage() != nil {
			nf := interactive.GetNativeFlowMessage()
			buttonCount := len(nf.GetButtons())
			if buttonCount > 0 {
				reasons = append(reasons, fmt.Sprintf("InteractiveMessage.NativeFlowMessage (%d buttons)", buttonCount))
				confidence += 10
			}
		}
		if interactive.GetCarouselMessage() != nil {
			reasons = append(reasons, "InteractiveMessage.CarouselMessage (carousel)")
			confidence += 10
		}
		if interactive.GetShopStorefrontMessage() != nil {
			reasons = append(reasons, "InteractiveMessage.ShopStorefrontMessage (shop)")
			confidence += 10
		}
		if interactive.GetBloksWidget() != nil {
			reasons = append(reasons, "InteractiveMessage.BloksWidget (A2UI widget)")
			confidence += 15
		}
	}

	// ─── 4. ButtonsMessage — old-style buttons ───
	if raw.GetButtonsMessage() != nil {
		buttons := raw.GetButtonsMessage()
		btnCount := len(buttons.GetButtons())
		if btnCount > 0 {
			reasons = append(reasons, fmt.Sprintf("ButtonsMessage (%d buttons)", btnCount))
			confidence += 35
		}
	}

	// ─── 5. ListMessage — list menus ───
	if raw.GetListMessage() != nil {
		list := raw.GetListMessage()
		sectionCount := len(list.GetSections())
		reasons = append(reasons, fmt.Sprintf("ListMessage (%d sections)", sectionCount))
		confidence += 30
	}

	// ─── 6. TemplateMessage — template messages ───
	if raw.GetTemplateMessage() != nil {
		reasons = append(reasons, "TemplateMessage (template)")
		confidence += 25
	}

	// ─── 7. HighlyStructuredMessage — HSM ───
	if raw.GetHighlyStructuredMessage() != nil {
		reasons = append(reasons, "HighlyStructuredMessage (HSM)")
		confidence += 30
	}

	// ─── 8. SecretEncryptedMessage — secret edits ───
	if raw.GetSecretEncryptedMessage() != nil {
		secret := raw.GetSecretEncryptedMessage()
		encType := secret.GetSecretEncType()
		// MESSAGE_EDIT (2) or EVENT_EDIT (1) from non-admin = suspicious
		if encType == waE2E.SecretEncryptedMessage_MESSAGE_EDIT ||
			encType == waE2E.SecretEncryptedMessage_EVENT_EDIT {
			reasons = append(reasons, fmt.Sprintf("SecretEncryptedMessage (type=%d)", encType))
			confidence += 25
		}
	}

	// ─── 9. DeviceSentMessage ───
	// REMOVED: normal for WhatsApp Web/linked devices. Not a bot signal.

	// ─── 10. GroupInviteMessage ───
	// REMOVED: normal users share group invites. Not a bot signal.

	// ─── 11. NewsletterAdminInviteMessage ───
	// REMOVED: not a reliable bot signal.

	// ─── 12. ContextInfo analysis ───
	// NOTE: forwarding is NORMAL user behavior — removed as bot signal.
	// Only ForwardedAiBotMessageInfo (explicit AI bot) is kept.
	if ext := raw.GetExtendedTextMessage(); ext != nil {
		if ci := ext.GetContextInfo(); ci != nil {
			// ForwardedAiBotMessageInfo — explicit AI bot forward
			if ci.GetForwardedAiBotMessageInfo() != nil {
				reasons = append(reasons, "ForwardedAiBotMessageInfo (AI bot forward)")
				confidence += 40
			}
			// External group reply — unusual for normal users
			if ci.GetGroupMentions() != nil && len(ci.GetGroupMentions()) > 3 {
				reasons = append(reasons, fmt.Sprintf("Multiple group mentions: %d", len(ci.GetGroupMentions())))
				confidence += 15
			}
			// Conversion data (tracking) — bot indicator
			if ci.GetConversionData() != nil {
				reasons = append(reasons, "ConversionData (tracking pixel)")
				confidence += 20
			}
			// StatusAttributions — bot attribution
			if ci.GetStatusAttributions() != nil && len(ci.GetStatusAttributions()) > 0 {
				reasons = append(reasons, "StatusAttributions present")
				confidence += 15
			}
		}
	}

	// ─── 13. AlbumMessage ───
	// REMOVED: albums are a normal official-client feature. Not a bot signal.

	// ─── 14. StickerMessage ───
	// REMOVED: animated stickers are a normal official-client feature.

	// ─── 15. PollCreationMessage ───
	// REMOVED: polls are a normal official-client feature. Not a bot signal.

	// ─── 16. Multiple message types ───
	// NOTE: now handled decisively in detectBotImpossible (>1 content type).
	// Kept here as soft backup for edge cases.

	// ─── 17. StickerPackMessage ───
	// REMOVED: not a reliable bot signal.

	// ─── 18. EventMessage ───
	// REMOVED: events are a normal official-client feature. Not a bot signal.

	// ─── 19. BcallMessage — bot call ───
	if raw.GetBcallMessage() != nil {
		reasons = append(reasons, "BcallMessage (bot call)")
		confidence += 30
	}

	// ─── 20. Ephemeral duration ───
	// REMOVED: EphemeralMessage is a plain FutureProofMessage wrapper;
	// duration is not carried in the message structure.

	// ─── 21. Zero-dimension media ───
	// Official clients always set dimensions on media. Zero dims = suspicious.
	if img := raw.GetImageMessage(); img != nil {
		if img.GetWidth() == 0 && img.GetHeight() == 0 && len(img.GetFileSHA256()) > 0 {
			reasons = append(reasons, "ImageMessage with zero dimensions")
			confidence += 15
		}
	}
	if vid := raw.GetVideoMessage(); vid != nil {
		if vid.GetWidth() == 0 && vid.GetHeight() == 0 && len(vid.GetFileSHA256()) > 0 {
			reasons = append(reasons, "VideoMessage with zero dimensions")
			confidence += 15
		}
	}

	// Determine if bot based on structure alone
	// Lower threshold for structure (35) because structural indicators are more reliable
	isBot := confidence >= 35

	return botDetectionResult{
		IsBot:      isBot,
		Reasons:    reasons,
		Confidence: confidence,
	}
}

// ─── Logic-based detection (fix: text biasa dari bot lolos) ─────

// detectBotLogic checks for logical anomalies and WhatsApp client
// inconsistencies that indicate a bot/userbot, even with plain text.
//
// These checks catch bots that send "normal" text by detecting:
//  1. Message ID format anomalies
//  2. Missing MessageContextInfo (client resmi always sets it)
//  3. Missing/empty MessageSecret
//  4. DeviceListMetadata anomalies
//  5. Bot signature patterns in text
//  6. Unnatural text formatting
//  7. Bot response timing patterns
//  8. Bot-style text entropy
//  9. Consistent formatting anomalies
//  10. WhatsApp client version mismatch
func detectBotLogic(msg *events.Message, raw *waE2E.Message, text string) botDetectionResult {
	var reasons []string
	confidence := 0

	if raw == nil {
		return botDetectionResult{}
	}

	// ─── 1. Message ID format analysis ───
	// WhatsApp client resmi: IDs start with "3EB0" or are 20+ chars
	// Bot/userbot: often short, hex-only, or missing "3EB0" prefix
	msgID := msg.Info.ID
	if len(msgID) > 0 {
		// Official WhatsApp client IDs start with "3EB0"
		if !strings.HasPrefix(msgID, "3EB0") {
			// Check if it's a valid-looking WhatsApp ID
			if len(msgID) < 15 {
				reasons = append(reasons, fmt.Sprintf("Short message ID (%d chars): %s", len(msgID), msgID))
				confidence += 20
			}
			// Check for pure hex (bot pattern)
			isHex := reHexOnly.MatchString(msgID)
			if isHex && len(msgID) >= 8 && len(msgID) <= 16 {
				reasons = append(reasons, fmt.Sprintf("Hex-only message ID: %s", msgID))
				confidence += 15
			}
			// Check for "BOT" prefix
			if strings.HasPrefix(strings.ToUpper(msgID), "BOT") {
				reasons = append(reasons, fmt.Sprintf("BOT-prefixed message ID: %s", msgID))
				confidence += 30
			}
		}
	}

	// ─── 2. MessageContextInfo analysis ───
	// NOTE: REMOVED "missing MessageContextInfo/MessageSecret" checks.
	// Verified against hypermeow source (send.go): MessageSecret is ONLY set
	// in bot mode or when reporting token needed. Normal user messages often
	// have NO MessageContextInfo at all. Flagging its absence = false positive.
	//
	// What IS suspicious: a MessageSecret with invalid length IF present.
	// (Official clients generate 32 bytes when they do set it.)
	mci := raw.GetMessageContextInfo()
	if mci != nil {
		secret := mci.GetMessageSecret()
		if len(secret) > 0 && len(secret) != 32 {
			reasons = append(reasons, fmt.Sprintf("Invalid MessageSecret length: %d (expected 32)", len(secret)))
			confidence += 20
		}
	}

	// ─── 6. Bot signature patterns in text ───
	if text != "" {
		lowerText := strings.ToLower(text)

		// Bot signature patterns
		botSignatures := []struct {
			Pattern string
			Score   int
		}{
			{"©", 15},
			{"v1.", 10}, {"v2.", 10}, {"v3.", 10}, {"v4.", 10},
			{"powered by", 20},
			{"bot by", 20},
			{"created by", 10},
			{"made by", 10},
			{"credits:", 10},
			{"github.com/", 15},
			{"t.me/", 10},
			{"discord.gg/", 15},
			{"youtube.com/", 10},
			{"instagram.com/", 10},
		}
		for _, sig := range botSignatures {
			if strings.Contains(lowerText, sig.Pattern) {
				reasons = append(reasons, fmt.Sprintf("Bot signature: %q", sig.Pattern))
				confidence += sig.Score
				break
			}
		}

		// ─── 7. Unnatural text formatting ───
		// Bots often produce text with unnatural patterns

		// Check for consistent line spacing (bot-generated)
		lines := strings.Split(text, "\n")
		if len(lines) > 3 {
			// Check if all non-empty lines have similar length (bot pattern)
			lineLens := []int{}
			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if len(trimmed) > 0 {
					lineLens = append(lineLens, len(trimmed))
				}
			}
			if len(lineLens) >= 4 {
				// Calculate variance
				avg := 0
				for _, l := range lineLens {
					avg += l
				}
				avg /= len(lineLens)
				variance := 0
				for _, l := range lineLens {
					diff := l - avg
					if diff < 0 {
						diff = -diff
					}
					variance += diff
				}
				variance /= len(lineLens)
				// Very low variance = bot-generated (consistent formatting)
				if variance <= 2 && avg > 10 {
					reasons = append(reasons, fmt.Sprintf("Unnatural consistent line length (variance=%d, avg=%d)", variance, avg))
					confidence += 20
				}
			}
		}

		// ─── 8. Bot response patterns ───
		// Bots often use status patterns like "✅ Done", "❌ Failed"
		botResponsePatterns := []string{
			"✅ done", "✅ success", "✅ berhasil", "✅ sukses",
			"❌ error", "❌ failed", "❌ gagal",
			"⏳ loading", "⏳ processing", "⏳ memproses",
			"🔄 restart", "🔄 restarting",
			"⚙️ setting", "⚙️ config",
			"📊 status:", "📋 report:",
			"🔔 notification", "📢 announcement",
		}
		for _, pattern := range botResponsePatterns {
			if strings.Contains(lowerText, pattern) {
				reasons = append(reasons, fmt.Sprintf("Bot response pattern: %q", pattern))
				confidence += 15
				break
			}
		}

		// ─── 9. Bot auto-reply patterns ───
		// Bots often start with specific greetings
		botGreetings := []string{
			"hai kak", "halo kak", "hai bro", "halo bro",
			"welcome back", "selamat datang kembali",
			"maaf, saya tidak mengerti", "sorry, i don't understand",
			"command not found", "perintah tidak ditemukan",
			"feature not available", "fitur tidak tersedia",
			"please wait", "mohon tunggu",
		}
		for _, greeting := range botGreetings {
			if strings.HasPrefix(lowerText, greeting) {
				reasons = append(reasons, fmt.Sprintf("Bot auto-reply pattern: %q", greeting))
				confidence += 20
				break
			}
		}

		// ─── 10. Excessive whitespace/padding ───
		// Bots often pad text with spaces or newlines
		if strings.Count(text, "\n\n\n") > 0 {
			reasons = append(reasons, "Excessive blank lines (triple newline)")
			confidence += 10
		}
		// Leading/trailing excessive whitespace
		if len(strings.TrimSpace(text)) < len(text)-10 {
			reasons = append(reasons, "Excessive leading/trailing whitespace")
			confidence += 10
		}

		// ─── 11. Bot command echo ───
		// Bots sometimes echo the command back
		if strings.HasPrefix(text, "Command:") || strings.HasPrefix(text, "Perintah:") {
			reasons = append(reasons, "Command echo pattern")
			confidence += 20
		}

		// ─── 12. Zero-width characters ───
		// Bots sometimes embed zero-width characters
		for _, r := range text {
			if r == '\u200B' || r == '\u200C' || r == '\u200D' || r == '\uFEFF' {
				reasons = append(reasons, "Zero-width character detected")
				confidence += 15
				break
			}
		}

		// ─── 13. Monospace/code block patterns ───
		// Bots often output code blocks
		if strings.Count(text, "```") >= 2 {
			reasons = append(reasons, "Code block pattern (```)")
			confidence += 15
		}
	}

	// ─── 14. Sender JID analysis ───
	sender := msg.Info.Sender
	// Bot LID users often have very long user IDs
	if sender.Server == "lid" && len(sender.User) > 20 {
		reasons = append(reasons, fmt.Sprintf("Long LID user ID: %s", sender.User))
		confidence += 10
	}

	// ─── 15. Timestamp analysis ───
	// Bot messages sometimes have timestamp anomalies
	ts := msg.Info.Timestamp
	if !ts.IsZero() {
		// Future timestamp = bot
		if ts.After(time.Now().Add(5 * time.Minute)) {
			reasons = append(reasons, "Future timestamp detected")
			confidence += 25
		}
		// Very old timestamp (more than 1 hour ago) = replayed/bot
		if ts.Before(time.Now().Add(-1 * time.Hour)) {
			reasons = append(reasons, "Stale timestamp (>1h ago)")
			confidence += 15
		}
	}

	// Cap confidence
	if confidence > 100 {
		confidence = 100
	}

	// Lower threshold for logic-based detection (40)
	isBot := confidence >= 40

	return botDetectionResult{
		IsBot:      isBot,
		Reasons:    reasons,
		Confidence: confidence,
	}
}

// autoReactBot — react 🤖 otomatis jika pesan terindikasi bot.
// Dijalankan async dari handleMessage untuk setiap pesan grup.
// Threshold 70 untuk hindari false positive.
func autoReactBot(client *whatsmeow.Client, msg *events.Message) {
	if client == nil || msg == nil {
		return
	}
	// Hanya di grup, skip pesan sendiri
	if !msg.Info.IsGroup || msg.Info.IsFromMe {
		return
	}
	text := extractText(msg)
	// Deteksi butuh teks atau raw message
	raw := msg.Message
	if text == "" && raw == nil {
		return
	}
	result := detectBot(msg, text)
	// Juga cek struktur bila ada raw
	if raw != nil {
		if sr := detectBotStructure(msg, raw); sr.Confidence > 0 {
			result.Confidence += sr.Confidence
			if result.Confidence > 100 {
				result.Confidence = 100
			}
			if sr.IsBot {
				result.IsBot = true
			}
		}
	}
	// React bila terindikasi bot (>= 40) ATAU terindikasi AI
	if result.Confidence >= 40 || isAIMessage(text) {
		aiReactMessage(client, msg, "🤖")
	}
}

// isAIMessage — deteksi pesan gaya AI (bukan command bot).
// Pola: terlalu formal, struktur rapi, disclaimer AI, dll.
func isAIMessage(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	// Disclaimer AI eksplisit
	aiPhrases := []string{
		"sebagai ai", "sebagai artificial", "sebagai model bahasa",
		"saya adalah ai", "aku adalah ai", "i'm an ai", "i am an ai",
		"sebagai asisten ai", "as an ai",
	}
	for _, p := range aiPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	// Struktur sangat rapi: banyak bullet/numbering + panjang
	bullets := strings.Count(text, "\n- ") + strings.Count(text, "\n• ") + strings.Count(text, "\n* ")
	numbered := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 2 && trimmed[0] >= '0' && trimmed[0] <= '9' && trimmed[1] == '.' {
			numbered++
		}
	}
	// Pesan panjang (>300 char) dengan struktur rapi = kemungkinan AI
	if len(text) > 300 && (bullets >= 3 || numbered >= 3) {
		return true
	}
	// Frasa formal khas AI
	formalPhrases := []string{
		"berikut adalah", "tentu saja", "dengan senang hati",
		"semoga membantu", "jangan ragu untuk bertanya",
		"here's", "certainly", "of course",
	}
	formalCount := 0
	for _, p := range formalPhrases {
		if strings.Contains(lower, p) {
			formalCount++
		}
	}
	if formalCount >= 2 && len(text) > 200 {
		return true
	}
	return false
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "antibot",
		Aliases:     []string{"antibotmode"},
		Description: "antibot",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAntibotCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "cekbot",
		Aliases:     []string{"isbot", "checkbot"},
		Description: "cekbot",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCekBotCommand(client, msg, args)
		},
	})
}
