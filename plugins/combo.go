package main

// combo.go — Unique combined features (multi-feature mashups)
// ════════════════════════════════════════════════════════════
//
// These features combine multiple existing capabilities:
//
//   .ship      → Love match 2 random group members + ship name
//   .battle    → Battle 2 users (HP, ATK, DEF from name hash)
//   .hotrank   → Rank top 5 group members by "hotness" (deterministic)
//   .vibecheck → AI reads group mood from recent messages
//   .matchmake → Find best couple in group (love calc everyone)
//   .quickpoll → Quick yes/no poll with auto reactions
//   .confess   → Anonymous confession (sent as SPR to group)
//   .story     → Collaborative story (AI continues from group context)
//   .factcheck → AI fact-checks a quoted message
//   .roast     → AI roasts a @tag or random member
//   .wheel     → Spin wheel of group members (picks random winner)
//   .ai8ball   → AI-powered magic 8-ball (uses AI, not random)

import (
	"context"
	"fmt"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── .ship — Ship 2 random group members ───────────────────────

// resolvePhone resolves a JID (LID or PN) to phone number string.
// Handles LID→PN conversion via dualForms.
func resolvePhone(client *whatsmeow.Client, jid types.JID) string {
	if jid.IsEmpty() {
		return "?"
	}
	// If already PN form, just return user
	if jid.Server == "s.whatsapp.net" {
		return jid.User
	}
	// Try phoneOf (checks store for LID→PN mapping)
	if client != nil {
		if ph := phoneOf(client, jid); ph != "" {
			return ph
		}
		// Try dual forms
		for _, form := range dualForms(client, jid) {
			if form.Server == "s.whatsapp.net" && form.User != "" {
				return form.User
			}
			if ph := phoneOf(client, form); ph != "" {
				return ph
			}
		}
	}
	// Fallback: use the raw user (might be LID but at least something)
	return jid.User
}

func handleShipCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || len(meta.Participants) < 2 {
		return "❌ Grup terlalu sepi"
	}

	// Pick 2 random members
	n1, _ := aiRandInt(len(meta.Participants))
	n2, _ := aiRandInt(len(meta.Participants))
	for n2 == n1 && len(meta.Participants) > 1 {
		n2, _ = aiRandInt(len(meta.Participants))
	}

	p1 := meta.Participants[n1].JID
	p2 := meta.Participants[n2].JID
	name1 := resolvePhone(client, p1)
	name2 := resolvePhone(client, p2)

	// Ship name (combine first 3 chars of each)
	shipName := name1
	if len(name1) > 3 {
		shipName = name1[:3]
	}
	if len(name2) > 3 {
		shipName += name2[:3]
	} else {
		shipName += name2
	}

	// Love percentage
	combined := strings.ToLower(name1 + name2)
	hash := 0
	for _, c := range combined {
		hash = hash*31 + int(c)
	}
	percent := (hash % 100)
	if percent < 15 {
		percent += 15
	}

	var comment string
	switch {
	case percent >= 90:
		comment = "💕 SHIP PERFECT! Wedding soon!"
	case percent >= 75:
		comment = "💗 Cocok banget! Date dong!"
	case percent >= 60:
		comment = "💜 Lumayan, bisa dicoba~"
	case percent >= 40:
		comment = "💙 Hmm, mungkin temenan aja"
	default:
		comment = "💔 Mending nggak deh 😂"
	}

	return fmt.Sprintf("🚢 *Ship*\n\n• +%s ❤️ +%s\n• Ship name: *%s*\n• Match: *%d%%*\n\n%s",
		name1, name2, shipName, percent, comment)
}

// ─── .battle — Battle 2 users ──────────────────────────────────

func handleBattleCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || len(meta.Participants) < 2 {
		return "❌ Grup terlalu sepi"
	}

	var p1, p2 types.JID
	mentions := extractMentionedJIDsDual(msg)
	if len(mentions) >= 2 {
		j1, _ := parseAnyJID(mentions[0])
		j2, _ := parseAnyJID(mentions[1])
		if !j1.IsEmpty() && !j2.IsEmpty() {
			p1, p2 = j1, j2
		}
	}
	if p1.IsEmpty() || p2.IsEmpty() {
		n1, _ := aiRandInt(len(meta.Participants))
		n2, _ := aiRandInt(len(meta.Participants))
		for n2 == n1 && len(meta.Participants) > 1 {
			n2, _ = aiRandInt(len(meta.Participants))
		}
		p1 = meta.Participants[n1].JID
		p2 = meta.Participants[n2].JID
		_ = p1
		_ = p2
	}

	name1 := resolvePhone(client, p1)
	name2 := resolvePhone(client, p2)

	// Stats from name hash
	stat := func(name string, seed int) (int, int, int) {
		h := 0
		for _, c := range strings.ToLower(name) {
			h = h*seed + int(c)
		}
		hp := (h % 50) + 50  // 50-100
		atk := (h % 30) + 20 // 20-49
		def := (h % 25) + 15 // 15-39
		return hp, atk, def
	}

	hp1, atk1, def1 := stat(name1, 31)
	hp2, atk2, def2 := stat(name2, 37)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("⚔️ *Battle*\n\n• +%s — HP %d, ATK %d, DEF %d\n• +%s — HP %d, ATK %d, DEF %d\n\n",
		name1, hp1, atk1, def1, name2, hp2, atk2, def2))

	// Simulate battle
	turn := 0
	for hp1 > 0 && hp2 > 0 && turn < 10 {
		// P1 attacks P2
		dmg1 := atk1 - def2/2
		if dmg1 < 1 {
			dmg1 = 1
		}
		hp2 -= dmg1
		sb.WriteString(fmt.Sprintf("• Turn %d: +%s hit +%s for %d → HP %d\n", turn+1, name1, name2, dmg1, hp2))
		if hp2 <= 0 {
			break
		}

		// P2 attacks P1
		dmg2 := atk2 - def1/2
		if dmg2 < 1 {
			dmg2 = 1
		}
		hp1 -= dmg2
		sb.WriteString(fmt.Sprintf("• Turn %d: +%s hit +%s for %d → HP %d\n", turn+1, name2, name1, dmg2, hp1))
		turn++
	}

	var winner string
	if hp1 <= 0 && hp2 <= 0 {
		winner = "DRAW! Keduanya KO 🤝"
	} else if hp1 <= 0 {
		winner = fmt.Sprintf("🏆 +%s MENANG!", name2)
	} else {
		winner = fmt.Sprintf("🏆 +%s MENANG!", name1)
	}
	sb.WriteString(fmt.Sprintf("\n%s", winner))

	return sb.String()
}

// ─── .hotrank — Rank group members by hotness ──────────────────

func handleHotRankCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || len(meta.Participants) == 0 {
		return "❌ Grup kosong"
	}

	type member struct {
		Phone string
		Score int
	}
	var members []member
	for _, p := range meta.Participants {
		phone := resolvePhone(client, p.JID)
		h := 0
		for _, c := range strings.ToLower(phone) {
			h = h*41 + int(c)
		}
		score := (h % 60) + 40 // 40-99
		members = append(members, member{Phone: phone, Score: score})
	}
	// Sort by score desc
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			if members[j].Score > members[i].Score {
				members[i], members[j] = members[j], members[i]
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("🔥 *Hot Rank Grup*\n\n")
	max := 5
	if len(members) < max {
		max = len(members)
	}
	emojis := []string{"🥇", "🥈", "🥉", "4️⃣", "5️⃣"}
	for i := 0; i < max; i++ {
		fire := ""
		for f := 0; f < members[i].Score/10; f++ {
			fire += "🔥"
			if len(fire) >= 10 {
				break
			}
		}
		sb.WriteString(fmt.Sprintf("%s +%s — *%d%%* %s\n", emojis[i], members[i].Phone, members[i].Score, fire))
	}
	return sb.String()
}

// ─── .vibecheck — AI reads group mood ───────────────────────────

func handleVibeCheckCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	// Get recent messages from archive
	chat := msg.Info.Chat.String()
	recent := aiConvMemoryGet(chat)
	if len(recent) == 0 {
		return "📭 Belum ada cukup pesan untuk vibe check"
	}

	// Summarize recent messages
	var sb strings.Builder
	sb.WriteString("Pesan terbaru di grup ini:\n")
	for i, t := range recent {
		if i >= 8 {
			break
		}
		preview := t.Content
		if len(preview) > 100 {
			preview = preview[:97] + "..."
		}
		sb.WriteString(fmt.Sprintf("%s: %s\n", t.Role, preview))
	}

	prompt := "Berdasarkan pesan-pesan ini, deskripsikan mood/vibe grup dalam 2 kalimat casual (Bahasa Indonesia, gaya Ruby Hoshino):\n\n" + sb.String()

	result, err := pollinationsTextAsk(prompt)
	if err != nil || result == "" {
		// Fallback: simple heuristic
		return "🔍 *Vibe Check*\n\nVibe grup ini: *rame dan seru* (fallback)"
	}

	return "🔍 *Vibe Check*\n\n" + result
}

// ─── .matchmake — Find best couple in group ────────────────────

func handleMatchmakeCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || len(meta.Participants) < 2 {
		return "❌ Grup terlalu sepi"
	}

	bestPct := 0
	var best1, best2 string

	for i := 0; i < len(meta.Participants); i++ {
		for j := i + 1; j < len(meta.Participants); j++ {
			n1 := resolvePhone(client, meta.Participants[i].JID)
			n2 := resolvePhone(client, meta.Participants[j].JID)
			combined := strings.ToLower(n1 + n2)
			h := 0
			for _, c := range combined {
				h = h*31 + int(c)
			}
			pct := (h % 100)
			if pct > bestPct {
				bestPct = pct
				best1 = resolvePhone(client, meta.Participants[i].JID)
				best2 = resolvePhone(client, meta.Participants[j].JID)
			}
		}
	}

	emoji := "💔"
	if bestPct >= 80 {
		emoji = "💕"
	} else if bestPct >= 60 {
		emoji = "💗"
	} else if bestPct >= 40 {
		emoji = "💙"
	}

	return fmt.Sprintf("💘 *Best Match Grup*\n\n• +%s %s +%s\n• Match: *%d%%*\n\nMungkin cocok nih~ 👀", best1, emoji, best2, bestPct)
}

// ─── .quickpoll — Quick yes/no poll ────────────────────────────

func handleQuickPollCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if strings.TrimSpace(args) == "" {
		return "❓ *Cara pakai:* .quickpoll <pertanyaan>\nContoh: `.quickpoll Mie ayam enak nggak?`"
	}

	// Send the poll question
	question := strings.TrimSpace(args)
	pollMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(fmt.Sprintf("📊 *Quick Poll*\n\n%s\n\n• 👍 Ya\n• 👎 Nggak", question)),
		},
	}
	pollMsg = humanizeMessage(pollMsg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := client.SendMessage(ctx, msg.Info.Chat, pollMsg)
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat poll: %v", err)
	}

	// React with 👍 and 👎 on the poll message
	time.Sleep(500 * time.Millisecond)
	reactions := []string{"👍", "👎"}
	for _, emoji := range reactions {
		reactMsg := &waE2E.Message{
			ReactionMessage: &waE2E.ReactionMessage{
				Key: &waCommon.MessageKey{
					RemoteJID: proto.String(msg.Info.Chat.String()),
					FromMe:    proto.Bool(true),
					ID:        proto.String(resp.ID),
				},
				Text: proto.String(emoji),
			},
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		client.SendMessage(ctx2, msg.Info.Chat, reactMsg)
		cancel2()
		time.Sleep(300 * time.Millisecond)
	}

	return "" // silent — poll message is the response
}

// ─── .confess — Anonymous confession ───────────────────────────

func handleConfessCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if strings.TrimSpace(args) == "" {
		return "❓ *Cara pakai:* .confess <pesan>\nPesan dikirim anonim ke grup"
	}
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	// Send as plain text (no sender info, no reply context)
	confession := fmt.Sprintf("💌 *Confession Anonim*\n\n%s", strings.TrimSpace(args))
	confessMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(confession),
		},
	}
	confessMsg = humanizeMessage(confessMsg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := client.SendMessage(ctx, msg.Info.Chat, confessMsg)
	if err != nil {
		return "❌ Gagal kirim confess"
	}

	// Reply to sender privately (confirmation)
	return "✅ Confess terkirim anonim ke grup"
}

// ─── .story — Collaborative story (AI continues) ──────────────

func handleStoryCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	chat := msg.Info.Chat.String()

	// Get recent conversation for context
	recent := aiConvMemoryGet(chat)
	var storyContext string
	if len(recent) > 0 {
		for i, t := range recent {
			if i >= 4 {
				break
			}
			storyContext += t.Role + ": " + t.Content + "\n"
		}
	}

	userInput := strings.TrimSpace(args)
	if userInput == "" {
		input := "Mulai cerita pendek random (2-3 kalimat, Bahasa Indonesia, gaya santai)"
		if storyContext != "" {
			input = "Ini kelanjutan cerita grup. Lanjutkan 2-3 kalimat:\n\n" + storyContext
		}
		result, err := pollinationsTextAsk(input)
		if err != nil || result == "" {
			return "❌ Lagi nggak mood nulis cerita, coba lagi"
		}
		return "📖 *Story Time*\n\n" + result
	}

	// User gives a word/sentence to continue
	prompt := "Lanjutkan cerita ini dengan kata/ide: '" + userInput + "' (2-3 kalimat, Bahasa Indonesia, santai):\n\n" + storyContext
	result, err := pollinationsTextAsk(prompt)
	if err != nil || result == "" {
		return "❌ Aku lagi bingung nulisnya, coba lagi"
	}

	// Save to conversation memory
	aiConvMemoryAdd(chat, ChatTurn{Role: "user", Content: "Story: " + userInput})
	aiConvMemoryAdd(chat, ChatTurn{Role: "assistant", Content: result})

	return "📖 *Story Time*\n\n" + result
}

// ─── .factcheck — AI fact-checks quoted message ─────────────────

func handleFactCheckCommand(client *whatsmeow.Client, msg *events.Message) string {
	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❓ *Cara pakai:* reply pesan lalu ketik .factcheck"
	}
	quotedText := extractTextFromMessage(quoted)
	if quotedText == "" {
		return "❌ Pesan yang di-reply kosong (bukan teks)"
	}

	prompt := "Fact-check pernyataan ini. Jawab singkat: BENAR/MYBATI/PARSIAL + penjelasan 1 kalimat (Bahasa Indonesia):\n\n\"" + quotedText + "\""
	result, err := pollinationsTextAsk(prompt)
	if err != nil || result == "" {
		return "❌ Nggak bisa fact-check sekarang, coba lagi"
	}

	return "🔍 *Fact Check*\n\n• Pesan: \"" + truncateStr(quotedText, 200) + "\"\n\n" + result
}

// ─── .roast — AI roasts someone ────────────────────────────────

func handleRoastCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}

	var target string
	mentions := extractMentionedJIDsDual(msg)
	if len(mentions) > 0 {
		if j, err := parseAnyJID(mentions[0]); err == nil {
			target = resolvePhone(client, j)
		}
	}
	if target == "" {
		// Pick random member
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
		if err != nil || len(meta.Participants) == 0 {
			return "❌ Gagal ambil member"
		}
		n, _ := aiRandInt(len(meta.Participants))
		target = meta.Participants[n].JID.User
	}

	prompt := "Roast orang dengan nomor +" + target + " dalam 2-3 kalimat. Bikin lucu, nggak nyakitin beneran, gaya santai Bahasa Indonesia. Jangan kasar/NSFW."
	result, err := pollinationsTextAsk(prompt)
	if err != nil || result == "" {
		return "❌ Lagi nggak ada ide roast, coba lagi"
	}

	return fmt.Sprintf("🔥 *Roast +%s*\n\n%s", target, result)
}

// ─── .wheel — Spin wheel of group members ──────────────────────

func handleWheelCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !msg.Info.IsGroup {
		return "❌ Hanya di grup"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meta, err := client.GetGroupInfo(ctx, msg.Info.Chat)
	if err != nil || len(meta.Participants) == 0 {
		return "❌ Grup kosong"
	}

	// Pick random winner
	n, _ := aiRandInt(len(meta.Participants))
	winner := resolvePhone(client, meta.Participants[n].JID)

	return fmt.Sprintf("🎡 *Spin Wheel*\n\n• Pemenang: *+%s* 🎉\n\nSelamat! 🎊", winner)
}

// ─── .ai8ball — AI-powered magic 8-ball ────────────────────────

func handleAI8BallCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if strings.TrimSpace(args) == "" {
		return "❓ *Cara pakai:* .ai8ball <pertanyaan>"
	}

	prompt := "Jawab pertanyaan ini kayak magic 8-ball tapi gaya Ruby Hoshino (casual, 1-2 kalimat, Bahasa Indonesia):\n\n" + args
	result, err := pollinationsTextAsk(prompt)
	if err != nil || result == "" {
		// Fallback to regular 8ball
		return handle8BallCommand(args)
	}

	return "🎱 " + result
}

// keep imports
var _ = whatsmeow.MediaAudio
var _ = waE2E.Message{}

func init() {
	RegisterPlugin(Plugin{
		Command:     "ship",
		Description: "ship",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleShipCommand(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "battle",
		Aliases:     []string{"fight"},
		Description: "battle",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleBattleCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "hotrank",
		Aliases:     []string{"hot"},
		Description: "hotrank",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleHotRankCommand(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "vibecheck",
		Aliases:     []string{"vibe"},
		Description: "vibecheck",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleVibeCheckCommand(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "matchmake",
		Aliases:     []string{"match"},
		Description: "matchmake",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMatchmakeCommand(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "quickpoll",
		Aliases:     []string{"poll"},
		Description: "quickpoll",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleQuickPollCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "confess",
		Description: "confess",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleConfessCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "story",
		Description: "story",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleStoryCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "factcheck",
		Aliases:     []string{"fc"},
		Description: "factcheck",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFactCheckCommand(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "roast",
		Description: "roast",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleRoastCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "wheel",
		Description: "wheel",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleWheelCommand(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "ai8ball",
		Aliases:     []string{"ai8"},
		Description: "ai8ball",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAI8BallCommand(client, msg, args)
		},
	})
}
