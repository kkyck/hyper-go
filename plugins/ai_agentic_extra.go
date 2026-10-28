package main

// ai_agentic_extra.go — Additional tools for the agentic AI (Ruby Hoshino)
// ════════════════════════════════════════════════════════════
// This file holds the implementations of new tools added in v2.33.
// All dispatch happens in aiExecuteTool() in ai_agentic.go.
//
// Categories of new tools:
//   - GROUP INFO EXTRA: get_admin_list, get_group_settings
//   - MESSAGE TOOLS EXTRA: react_message, set_typing, mark_read, mention_user
//   - GROUP ADMIN EXTRA: unlock_group_send (alias for lock_group_send off)
//   - UTILITY: random_choice, roll_dice, flip_coin, random_number,
//              uuid_gen, hash_text, url_encode, url_decode,
//              time_convert, countdown_to, get_random_quote
//
// All functions are stateless and side-effect free except for
// WhatsApp messaging side effects (reaction, typing, mark_read).
// ════════════════════════════════════════════════════════════

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── GROUP INFO EXTRA ────────────────────────────────────────────

// aiGetAdminList returns all admins of the current group with phone/JID.
func aiGetAdminList(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "Error: client nil"
	}
	if !msg.Info.IsGroup {
		return "Error: hanya bisa di grup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil {
		return fmt.Sprintf("Error: gagal ambil info grup: %v", err)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👑 *Admin Grup*\n\n"))
	count := 0
	for _, p := range meta.Participants {
		if p.IsAdmin || p.IsSuperAdmin {
			count++
			role := "admin"
			if p.IsSuperAdmin {
				role = "super admin"
			}
			sb.WriteString(fmt.Sprintf("• +%s (%s)\n", p.JID.User, role))
		}
	}
	if count == 0 {
		return "Grup ini tidak punya admin"
	}
	sb.WriteString(fmt.Sprintf("\nTotal: %d admin", count))
	return sb.String()
}

// aiGetGroupSettings returns the current group settings in detail.
// Includes Announce (lock send) and Locked (lock info) status — used by
// the AI to understand the current lock state before toggling.
func aiGetGroupSettings(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "Error: client nil"
	}
	if !msg.Info.IsGroup {
		return "Error: hanya bisa di grup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil {
		return fmt.Sprintf("Error: gagal ambil info grup: %v", err)
	}
	var sb strings.Builder
	sb.WriteString("⚙️ *Group Settings*\n\n")
	sb.WriteString(fmt.Sprintf("• Nama: %s\n", meta.Name))
	if strings.TrimSpace(meta.Topic) != "" {
		desc := strings.TrimSpace(meta.Topic)
		if len(desc) > 200 {
			desc = desc[:200] + "..."
		}
		sb.WriteString(fmt.Sprintf("• Deskripsi: %s\n", desc))
	}
	sb.WriteString(fmt.Sprintf("• Total Member: %d\n", len(meta.Participants)))
	if !meta.GroupCreated.IsZero() {
		sb.WriteString(fmt.Sprintf("• Dibuat: %s\n", meta.GroupCreated.Format("02 Jan 2006")))
	}
	if !meta.OwnerJID.IsEmpty() {
		sb.WriteString(fmt.Sprintf("• Owner: +%s\n", meta.OwnerJID.User))
	}
	if meta.IsAnnounce {
		sb.WriteString("• 🔒 Lock Send (Announce): ON — HANYA ADMIN yang bisa kirim pesan, member TIDAK BISA kirim\n")
	} else {
		sb.WriteString("• 🔓 Lock Send (Announce): OFF — semua member bisa kirim pesan\n")
	}
	if meta.IsLocked {
		sb.WriteString("• 🔒 Lock Info (Locked): ON — hanya admin yang bisa edit nama/deskripsi/foto grup\n")
	} else {
		sb.WriteString("• 🔓 Lock Info (Locked): OFF — semua member bisa edit info grup\n")
	}
	adminCount := 0
	for _, p := range meta.Participants {
		if p.IsAdmin || p.IsSuperAdmin {
			adminCount++
		}
	}
	sb.WriteString(fmt.Sprintf("• Admin: %d\n", adminCount))
	sb.WriteString(fmt.Sprintf("• Member biasa: %d\n", len(meta.Participants)-adminCount))
	return sb.String()
}

// ─── MESSAGE TOOLS EXTRA ────────────────────────────────────────

// aiReactMessage reacts to the quoted message (or current message)
// with the specified emoji. Defaults to ⭐ if emoji is empty.
func aiReactMessage(client *whatsmeow.Client, msg *events.Message, emoji string) string {
	if client == nil {
		return "Error: client nil"
	}
	emoji = strings.TrimSpace(emoji)
	if emoji == "" {
		emoji = "⭐" // Ruby's signature star
	}
	// Default target = current message
	var targetChat types.JID = msg.Info.Chat
	var targetID string = msg.Info.ID
	var targetFromMe bool = msg.Info.IsFromMe
	// Prefer quoted message if available
	if q := getQuotedMessage(msg); q != nil && q.Info.ID != "" {
		targetChat = q.Info.Chat
		targetID = q.Info.ID
		targetFromMe = q.Info.IsFromMe
	}
	reactMsg := &waE2E.Message{
		ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(targetChat.String()),
				FromMe:    proto.Bool(targetFromMe),
				ID:        proto.String(targetID),
			},
			Text: proto.String(emoji),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := client.SendMessage(ctx, targetChat, reactMsg)
	if err != nil {
		return fmt.Sprintf("Error: gagal react: %v", err)
	}
	return fmt.Sprintf("✅ Reacted with %s", emoji)
}

// aiSetTyping shows the typing indicator in the current chat.
func aiSetTyping(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "Error: client nil"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.SendChatPresence(ctx, msg.Info.Chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	if err != nil {
		return fmt.Sprintf("Error: gagal set typing: %v", err)
	}
	return "✅ Typing indicator shown"
}

// aiMarkRead marks the current message as read.
func aiMarkRead(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "Error: client nil"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.MarkRead(ctx, []types.MessageID{msg.Info.ID}, time.Now(), msg.Info.Chat, msg.Info.Sender)
	if err != nil {
		return fmt.Sprintf("Error: gagal mark read: %v", err)
	}
	return "✅ Message marked as read"
}

// aiMentionUser resolves a target (phone or @tag) into a mention
// the AI can include in the next reply.
func aiMentionUser(client *whatsmeow.Client, msg *events.Message, target string) string {
	jid, label, err := aiResolveTarget(client, msg, target)
	if err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("✅ Mention resolved: @%s (JID: %s). Sertakan @%s di pesan selanjutnya untuk mention user.", label, jid.String(), label)
}

// ─── GROUP ADMIN EXTRA ───────────────────────────────────────────

// aiUnlockGroupSend is the explicit alias for lock_group_send off.
// Used to unlock a group that was previously locked (announce mode off).
func aiUnlockGroupSend(client *whatsmeow.Client, msg *events.Message) string {
	return aiSetGroupAnnounce(client, msg, "off")
}

// ─── UTILITY TOOLS ───────────────────────────────────────────────

// aiRandomChoice picks a random item from a comma-separated list.
func aiRandomChoice(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "Error: list tidak boleh kosong. Contoh: merah,hijau,biru"
	}
	items := strings.Split(args, ",")
	var valid []string
	for _, s := range items {
		s = strings.TrimSpace(s)
		if s != "" {
			valid = append(valid, s)
		}
	}
	if len(valid) == 0 {
		return "Error: semua item kosong"
	}
	idx, err := aiRandInt(len(valid))
	if err != nil {
		return "Error: RNG failed"
	}
	return fmt.Sprintf("🎲 Aku pilih: *%s* ⭐", valid[idx])
}

// aiRollDice rolls a dice. Default 1d6, or NdM format.
func aiRollDice(args string) string {
	args = strings.TrimSpace(strings.ToLower(args))
	n, sides := 1, 6
	if args != "" {
		parts := strings.SplitN(args, "d", 2)
		if len(parts) != 2 {
			return "Error: format salah. Pakai: NdM (contoh: 2d6, 1d20) atau kosong untuk 1d6"
		}
		if _, err := fmt.Sscanf(parts[0], "%d", &n); err != nil || n < 1 || n > 100 {
			return "Error: jumlah dadu harus 1-100"
		}
		if _, err := fmt.Sscanf(parts[1], "%d", &sides); err != nil || sides < 2 || sides > 1000 {
			return "Error: jumlah sisi harus 2-1000"
		}
	}
	var rolls []int
	total := 0
	for i := 0; i < n; i++ {
		r, err := aiRandInt(sides)
		if err != nil {
			return "Error: RNG failed"
		}
		rolls = append(rolls, r+1) // randInt returns 0..n-1, we want 1..sides
		total += r + 1
	}
	return fmt.Sprintf("🎲 Roll %dd%d: %v = *%d* ⭐", n, sides, rolls, total)
}

// aiFlipCoin flips a coin.
func aiFlipCoin() string {
	r, err := aiRandInt(2)
	if err != nil {
		return "Error: RNG failed"
	}
	if r == 0 {
		return "🪙 Coin flip: *Heads* (kepala) ⭐"
	}
	return "🪙 Coin flip: *Tails* (ekor) ⭐"
}

// aiRandomNumber returns a random number between N and M (inclusive).
func aiRandomNumber(args string) string {
	args = strings.TrimSpace(args)
	var lo, hi int
	if args == "" {
		lo, hi = 1, 100
	} else {
		n, err := fmt.Sscanf(args, "%d %d", &lo, &hi)
		if err != nil || n != 2 {
			return "Error: format salah. Pakai: N M (contoh: 1 100)"
		}
		if lo > hi {
			lo, hi = hi, lo
		}
	}
	span := hi - lo + 1
	if span <= 0 {
		return "Error: range invalid"
	}
	r, err := aiRandInt(span)
	if err != nil {
		return "Error: RNG failed"
	}
	return fmt.Sprintf("🎲 Random number [%d..%d]: *%d* ⭐", lo, hi, lo+r)
}

// aiUUIDGen generates a random UUID v4.
func aiUUIDGen() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "Error: RNG failed"
	}
	// Set version (4) and variant (10xx)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("🔑 UUID v4: %08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// aiHashText hashes a text with md5 or sha256.
func aiHashText(args string) string {
	args = strings.TrimSpace(args)
	parts := strings.SplitN(args, "|", 2)
	if len(parts) < 2 {
		return "Error: format salah. Pakai: md5|sha256 | text"
	}
	algo := strings.ToLower(strings.TrimSpace(parts[0]))
	text := strings.TrimSpace(parts[1])
	if text == "" {
		return "Error: text tidak boleh kosong"
	}
	switch algo {
	case "md5":
		h := md5.Sum([]byte(text))
		return fmt.Sprintf("🔑 MD5(%q) = %s", text, hex.EncodeToString(h[:]))
	case "sha256":
		h := sha256.Sum256([]byte(text))
		return fmt.Sprintf("🔑 SHA256(%q) = %s", text, hex.EncodeToString(h[:]))
	default:
		return "Error: algo tidak dikenal. Pakai: md5 atau sha256"
	}
}

// aiURLEncode URL-encodes a text.
func aiURLEncode(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "Error: text tidak boleh kosong"
	}
	return fmt.Sprintf("🔗 Encoded: %s", url.QueryEscape(args))
}

// aiURLDecode URL-decodes a text.
func aiURLDecode(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "Error: text tidak boleh kosong"
	}
	decoded, err := url.QueryUnescape(args)
	if err != nil {
		return fmt.Sprintf("Error: gagal decode: %v", err)
	}
	return fmt.Sprintf("🔗 Decoded: %s", decoded)
}

// aiTimeConvert converts time across WIB/UTC/JST.
// Format: "FROM TO HH:MM" (e.g. "WIB JST 12:00").
func aiTimeConvert(args string) string {
	args = strings.TrimSpace(strings.ToUpper(args))
	if args == "" {
		return "Error: format salah. Pakai: FROM TO HH:MM (contoh: WIB JST 12:00)"
	}
	parts := strings.Fields(args)
	if len(parts) != 3 {
		return "Error: format salah. Pakai: FROM TO HH:MM (contoh: WIB JST 12:00)"
	}
	from, to, timeStr := parts[0], parts[1], parts[2]
	var fromOffset, toOffset int
	switch from {
	case "WIB":
		fromOffset = 7
	case "UTC":
		fromOffset = 0
	case "JST":
		fromOffset = 9
	default:
		return "Error: FROM harus WIB/UTC/JST"
	}
	switch to {
	case "WIB":
		toOffset = 7
	case "UTC":
		toOffset = 0
	case "JST":
		toOffset = 9
	default:
		return "Error: TO harus WIB/UTC/JST"
	}
	var hh, mm int
	if n, err := fmt.Sscanf(timeStr, "%d:%d", &hh, &mm); err != nil || n != 2 || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return "Error: format jam salah. Pakai: HH:MM (24 jam)"
	}
	inputMinutes := hh*60 + mm
	utcMinutes := inputMinutes - fromOffset*60
	utcMinutes = ((utcMinutes % 1440) + 1440) % 1440
	outMinutes := (utcMinutes + toOffset*60) % 1440
	outMinutes = ((outMinutes % 1440) + 1440) % 1440
	outHH := outMinutes / 60
	outMM := outMinutes % 60
	return fmt.Sprintf("🕐 %s %s = %s %s ⭐", timeStr, from, fmt.Sprintf("%02d:%02d", outHH, outMM), to)
}

// aiCountdownTo returns days until a target date (YYYY-MM-DD).
func aiCountdownTo(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return "Error: format salah. Pakai: YYYY-MM-DD"
	}
	target, err := time.Parse("2006-01-02", args)
	if err != nil {
		return "Error: format tanggal salah. Pakai: YYYY-MM-DD"
	}
	now := time.Now().UTC().Add(7 * time.Hour) // WIB
	nowDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	diff := target.Sub(nowDate).Hours() / 24
	days := int(diff)
	if days < 0 {
		return fmt.Sprintf("📅 %s sudah lewat %d hari yang lalu", args, -days)
	}
	if days == 0 {
		return fmt.Sprintf("📅 %s adalah HARI INI! ⭐", args)
	}
	return fmt.Sprintf("📅 Countdown ke %s: *%d hari* lagi ⭐", args, days)
}

// aiGetRandomQuote returns a random Ruby Hoshino style quote.
func aiGetRandomQuote() string {
	quotes := []string{
		"Senyum adalah senjata terkuat! ⭐ — Ruby Hoshino",
		"Setiap orang berhak bahagia. Termasuk kamu. Termasuk aku. ⭐ — Ruby",
		"Kesepian itu... sangat menyedihkan. Makanya aku ingin semua orang bisa tersenyum. — Ruby",
		"Hari ini juga aku akan memberikan yang terbaik! ⭐",
		"Kalau kamu sedih, ingat: ada Ruby yang sayang sama kamu. Hehe~ ⭐",
		"Yang paling tulus itu yang paling terasa. — Ruby",
		"Kata orang aku terlalu ceria. Tapi itu kan aku~ Hehe~ ⭐",
		"Rumah itu bukan tempat, tapi orang-orangnya. ⭐ — Ruby",
	}
	idx, err := aiRandInt(len(quotes))
	if err != nil {
		return "💬 " + quotes[0]
	}
	return "💬 " + quotes[idx]
}

// ─── Helpers ────────────────────────────────────────────────────

// randInt returns a cryptographically secure random int in [0, n).
func aiRandInt(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("n must be > 0")
	}
	bigN, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(bigN.Int64()), nil
}

// All imports are kept referenced by function signatures above:
//   - whatsmeow.Client is used as func parameter type
//   - waCommon.MessageKey is used inside ReactionMessage construction
//   - waE2E.Message is used to construct ReactionMessage payload
//   - types.MessageID is used in MarkRead call
//   - events.Message is used as func parameter type
//   - proto.String / proto.Bool are used to wrap strings/bools in proto pointers

// ─── Pollinations text AI (2nd fallback) ─────────────────────────
//
// Uses the free Pollinations text API (no key required).
// Two strategies:
//  1. POST $POLLINATIONS_BASE/openai (OpenAI-compatible, supports long prompts)
//  2. GET $POLLINATIONS_BASE/<prompt> (fallback for simple prompts)
//
// This is the 2nd AI fallback (after geminiFreeAsk, before kwAIChat).
func pollinationsTextAsk(prompt string) (string, error) {
	if base := pollinationsBaseURL(); base == "" {
		return "", &envMissingError{Name: "POLLINATIONS_BASE"}
	}
	// Strategy 1: OpenAI-compatible POST endpoint
	systemPrompt := ""
	userPrompt := prompt
	if idx := strings.Index(prompt, "assistant: "); idx > 0 {
		systemPrompt = strings.TrimSpace(prompt[:idx])
		userPrompt = strings.TrimSpace(prompt[idx+len("assistant: "):])
		if userPrompt == "" {
			userPrompt = systemPrompt
			systemPrompt = ""
		}
	}

	reqBody := map[string]interface{}{
		"model": "openai",
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": systemPrompt + "\n\n" + userPrompt,
			},
		},
		"temperature": 0.7,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("pollinations marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST",
		pollinationsBaseURL()+"/openai", bytes.NewReader(jsonBody))
	if err != nil {
		return pollinationsTextAskGET(prompt)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hyper-go/1.0")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return pollinationsTextAskGET(prompt)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return pollinationsTextAskGET(prompt)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return pollinationsTextAskGET(prompt)
	}

	if result.Error != "" {
		return "", fmt.Errorf("pollinations API error: %s", result.Error)
	}

	if len(result.Choices) == 0 || result.Choices[0].Message.Content == "" {
		return pollinationsTextAskGET(prompt)
	}

	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

// pollinationsTextAskGET is the GET fallback (simpler, but limited prompt length).
func pollinationsTextAskGET(prompt string) (string, error) {
	if base := pollinationsBaseURL(); base == "" {
		return "", &envMissingError{Name: "POLLINATIONS_BASE"}
	}
	encoded := url.QueryEscape(prompt)
	if len(encoded) > 3000 {
		encoded = url.QueryEscape(prompt[:2000])
	}

	urlStr := pollinationsBaseURL() + "/" + encoded

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hyper-go/1.0")
	req.Header.Set("Accept", "text/plain")

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pollinations GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("pollinations GET HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 100*1024))
	if err != nil {
		return "", fmt.Errorf("pollinations GET read: %w", err)
	}

	text := strings.TrimSpace(string(body))
	if text == "" {
		return "", fmt.Errorf("pollinations GET: empty response")
	}

	return text, nil
}
