package main

// gender.go — .tebakgender: tebak jenis kelamin dari gaya ketikan (for fun)
// Menganalisis arsip pesan user: interjeksi, emoticon, panjang pesan, pola ngambek, dll.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── .tebakgender ──────────────────────────────────────────────

// hasElongated cek huruf berulang 3x+ ("iyaaa", "kamuuu") — pengganti regex backreference yg tidak didukung RE2.
func hasElongated(s string) bool {
	var prev rune
	count := 0
	for _, r := range s {
		if r == prev {
			count++
			if count >= 3 {
				return true
			}
		} else {
			prev = r
			count = 1
		}
	}
	return false
}

func handleTebakGenderCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	loadMsgArchive()

	// Tentukan target: @tag > reply > phone di args > pengirim
	var targetJID string
	var targetName string
	if mentions := extractMentionedJIDsDual(msg); len(mentions) > 0 {
		targetJID = mentions[0]
		targetName = extractPhone(targetJID)
	} else if q := getQuotedMessage(msg); q != nil {
		targetJID = q.Info.Sender.String()
		targetName = q.Info.PushName
	} else if a := strings.TrimSpace(args); a != "" {
		phone := extractPhone(a)
		if phone == "" {
			return "❌ Nomor/tag tidak valid."
		}
		targetJID = phone
		targetName = phone
	} else {
		targetJID = msg.Info.Sender.String()
		targetName = msg.Info.PushName
	}
	targetPhone := extractPhone(targetJID)

	// Cari nama target dari arsip biar error/output nunjukin nama, bukan angka mentah
	if targetName == targetPhone || targetName == "" {
		msgArchiveMu.RLock()
		for _, m := range msgArchive {
			if extractPhone(m.Sender) == targetPhone && m.PushName != "" {
				targetName = m.PushName
				break
			}
		}
		msgArchiveMu.RUnlock()
	}

	// Kumpulkan pesan target dari SEMUA grup + catat grupnya + tipe pesan
	msgArchiveMu.RLock()
	var texts []string
	var name string
	groupSet := make(map[string]bool)
	typeCount := make(map[string]int)
	totalMsgCount := 0
	for _, m := range msgArchive {
		if extractPhone(m.Sender) == targetPhone {
			totalMsgCount++
			typeCount[m.Type]++
			groupSet[m.GroupJID] = true
			if m.Text != "" {
				texts = append(texts, m.Text)
			}
			if name == "" && m.PushName != "" {
				name = m.PushName
			}
		}
	}
	msgArchiveMu.RUnlock()

	if len(texts) < 5 {
		return fmt.Sprintf("❌ Data kurang (cuma %d pesan). Butuh minimal 5 pesan teks dari %s.", len(texts), targetName)
	}
	if name != "" {
		targetName = name
	}

	// Cek grup bot vs grup target
	var groupNames []string
	var sharedCount int
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		groups, err := client.GetJoinedGroups(ctx)
		cancel()
		if err == nil {
			botGroups := make(map[string]string)
			for _, g := range groups {
				botGroups[g.JID.String()] = g.Name
			}
			for gjid := range groupSet {
				if gname, ok := botGroups[gjid]; ok {
					sharedCount++
					if len(groupNames) < 5 {
						groupNames = append(groupNames, gname)
					}
				}
			}
		}
	}

	// ─── Feature extraction ───
	var (
		scoreF, scoreM float64
		totalLen       int
		emojiCount     int
		shortCount     int // pesan <= 3 kata
		exclaimCount   int
		questionCount  int
		wkwkCount      int
		reasons        []string
	)
	addF := func(pts float64, why string) { scoreF += pts; reasons = append(reasons, "♀ "+why) }
	addM := func(pts float64, why string) { scoreM += pts; reasons = append(reasons, "♂ "+why) }

	femaleInterjections := []string{"cih", "cuih", "hmph", "humph", "hmm", "ih ", " ih", "apasih", "apasi", "manyun", "huh", "huhuhu", "hiks", "ihh", "ya ampun", "astaga", "duhhh", "hadehhh"}
	maleInterjections := []string{"woy", "woi", "anjir", "anjay", "jir", "goblok", "tolol", "bangsat", "kampret", "ngentot", "kontol", "memek"}
	poutEmotes := []string{">:(", ":(", ":'(", "T_T", ">.<", ":3", "(>_<)", "(T_T)"}
	maleEmotes := []string{":v", ":p", "8)", "B)"}
	dramaticWords := []string{"muak", "kesal", "sebel", "benci", "putus", "ngambek", "cuek", "ingat lu", "inget lu", "terserah", "drama"}
	// Pronoun & sapaan
	femalePronouns := []string{" aku ", "aku ", " saya ", " kamu ", "kamu ", " kak ", "kak "}
	malePronouns := []string{" gue ", "gue ", " gua ", "gua ", " gw ", "gw", " lu ", " lo ", " elu ", "cuy", "bro ", "gan ", "bre "}
	femaleParticles := []string{" dong", "dong ", " deh", " sih"}
	kaomojiMarks := []string{"(｡", "(╯", "♥", "♡", "(´", "(；", "(￣"}
	// ─── Sinyal tambahan ───
	cuteWords := []string{"gemes", "gemoy", "gemash", "aww", "awww", "cieee", "ciyee", "lucu banget", "imut"}
	baperWords := []string{"baper", "salting", "kepo", "mager", "kzl", "gabut"}
	endearWords := []string{"sayang", " beb ", "ayang", "bestie", "besti"}
	demiWords := []string{"demi apa", "sumpah ya", "sumpah demi"}
	apologizeWords := []string{"maaf", "sorry", "sori", "ampun"}
	gasWords := []string{"gaskeun", "gaspol", " gaskeun", "gas kan", "gaskan"}
	chillWords := []string{"santuy", "mantap", "jos", "gacor", "sabi"}
	gamingWords := []string{"mabar", "push rank", " gg", " otw", "ranked", "party"}
	techWords := []string{"bug", "error", "server", "deploy", "coding", "script", "compile", "database"}
	dismissWords := []string{"bodo amat", "bodoh amat", " skip", "skip ", "yaudah sih"}
	heartEmojis := []string{"❤️", "💕", "💖", "🥰", "😍", "💗"}
	sparkleEmojis := []string{"🌸", "✨", "💅", "🦋"}

	var elongatedCount, tildeCount, kaomojiCount int
	var longLaughCount, exclaimSpamCount, ellipsisCount, capsCount int
	var heartEmojiCount int

	laughRe := regexp.MustCompile(`(wkwk){2,}|(haha){2,}|(hihi){2,}`)

	for _, t := range texts {
		low := strings.ToLower(t)
		totalLen += len([]rune(t))
		if len(strings.Fields(t)) <= 3 {
			shortCount++
		}
		exclaimCount += strings.Count(t, "!")
		questionCount += strings.Count(t, "?")
		if strings.Contains(low, "wkwk") || strings.Contains(low, "haha") || strings.Contains(low, "hehe") {
			wkwkCount++
		}
		if hasElongated(low) {
			elongatedCount++
		}
		if strings.HasSuffix(strings.TrimSpace(t), "~") {
			tildeCount++
		}
		if laughRe.MatchString(low) {
			longLaughCount++
		}
		if strings.Contains(t, "!!!") || strings.Contains(t, "???") {
			exclaimSpamCount++
		}
		if strings.Count(t, "...") >= 1 || strings.Count(t, "…") >= 1 {
			ellipsisCount++
		}
		trimmed := strings.TrimSpace(t)
		if len(trimmed) > 4 && trimmed == strings.ToUpper(trimmed) && regexp.MustCompile(`[A-Z]`).MatchString(trimmed) {
			capsCount++
		}
		for _, h := range heartEmojis {
			if strings.Contains(t, h) {
				heartEmojiCount++
				break
			}
		}
		for _, h := range sparkleEmojis {
			if strings.Contains(t, h) {
				heartEmojiCount++
				break
			}
		}
		for _, k := range kaomojiMarks {
			if strings.Contains(t, k) {
				kaomojiCount++
				break
			}
		}
		for _, r := range t {
			if r > 0x1F600 && r < 0x1FAFF || (r >= 0x2600 && r <= 0x27BF) || r == 0x2764 || r == 0x1F970 {
				emojiCount++
				break
			}
		}
	}

	joined := strings.ToLower(strings.Join(texts, "\n"))
	countHas := func(words []string) int {
		c := 0
		for _, w := range words {
			if strings.Contains(joined, w) {
				c++
			}
		}
		return c
	}
	// Hitung kemunculan (bukan cuma ada/tidak) untuk pronoun
	countOcc := func(words []string) int {
		c := 0
		for _, w := range words {
			c += strings.Count(joined, w)
		}
		return c
	}

	if n := countHas(femaleInterjections); n > 0 {
		addF(float64(n)*8, fmt.Sprintf("interjeksi manja/ngambek (%d)", n))
	}
	if n := countHas(poutEmotes); n > 0 {
		addF(float64(n)*6, fmt.Sprintf("emoticon manyun (%d)", n))
	}
	if n := countHas(maleEmotes); n > 0 {
		addM(float64(n)*5, fmt.Sprintf("emoticon :v/:p (%d)", n))
	}
	if n := countHas(dramaticWords); n > 0 {
		addF(float64(n)*7, fmt.Sprintf("kata dramatis/emosional (%d)", n))
	}
	if n := countHas(maleInterjections); n > 0 {
		addM(float64(n)*6, fmt.Sprintf("umpatan kasar (%d)", n))
	}
	// Pronoun: sinyal kuat dalam bahasa Indonesia
	if n := countOcc(femalePronouns); n >= 3 {
		addF(float64(n)*1.5, fmt.Sprintf("pakai aku/kamu/kak (%dx)", n))
	}
	if n := countOcc(malePronouns); n >= 3 {
		addM(float64(n)*1.5, fmt.Sprintf("pakai gue/lu/bro (%dx)", n))
	}
	if n := countOcc(femaleParticles); n >= 3 {
		addF(float64(n)*1.2, fmt.Sprintf("partikel dong/deh/sih (%dx)", n))
	}
	// Kaomoji, kata memanjang, tilde
	if kaomojiCount > 0 {
		addF(float64(kaomojiCount)*5, fmt.Sprintf("kaomoji (%d)", kaomojiCount))
	}
	if elongatedCount > len(texts)/10 {
		addF(6, fmt.Sprintf("kata memanjang: iyaaa/kamuuu (%d)", elongatedCount))
	}
	if tildeCount > 0 {
		addF(float64(tildeCount)*4, fmt.Sprintf("akhiran tilde ~ (%d)", tildeCount))
	}
	// ─── Sinyal tambahan ───
	if n := countHas(cuteWords); n > 0 {
		addF(float64(n)*6, fmt.Sprintf("kata cute: gemes/awww/cieee (%d)", n))
	}
	if n := countHas(baperWords); n > 0 {
		addF(float64(n)*5, fmt.Sprintf("kata baper/salting/kepo (%d)", n))
	}
	if n := countHas(endearWords); n > 0 {
		addF(float64(n)*6, fmt.Sprintf("panggilan sayang/beb (%d)", n))
	}
	if n := countHas(demiWords); n > 0 {
		addF(float64(n)*5, fmt.Sprintf("\"demi apa\"/sumpah (%d)", n))
	}
	if n := countHas(apologizeWords); n > 2 {
		addF(5, fmt.Sprintf("sering minta maaf (%d)", n))
	}
	if n := countHas(gasWords); n > 0 {
		addM(float64(n)*5, fmt.Sprintf("kata gas/gaskeun (%d)", n))
	}
	if n := countHas(chillWords); n > 0 {
		addM(float64(n)*4, fmt.Sprintf("kata santuy/mantap/gacor (%d)", n))
	}
	if n := countHas(gamingWords); n > 0 {
		addM(float64(n)*5, fmt.Sprintf("istilah gaming: mabar/gg/otw (%d)", n))
	}
	if n := countHas(techWords); n >= 3 {
		addM(float64(n)*2, fmt.Sprintf("bahas teknis: bug/server (%d)", n))
	}
	if n := countHas(dismissWords); n > 0 {
		addM(float64(n)*5, fmt.Sprintf("dismissive: bodo amat/skip (%d)", n))
	}
	if longLaughCount > len(texts)/8 {
		addF(5, fmt.Sprintf("tawa panjang wkwkwkwk (%d)", longLaughCount))
	}
	if exclaimSpamCount > len(texts)/10 {
		addF(5, fmt.Sprintf("spam !!!/??? (%d)", exclaimSpamCount))
	}
	if ellipsisCount > len(texts)/5 {
		addF(4, fmt.Sprintf("sering pakai ... (%d)", ellipsisCount))
	}
	if heartEmojiCount > 0 {
		addF(float64(heartEmojiCount)*4, fmt.Sprintf("emoji hati/bunga (%d)", heartEmojiCount))
	}
	// Rasio sticker dari SEMUA pesan (termasuk non-teks)
	if totalMsgCount > 10 {
		stickerRatio := float64(typeCount["sticker"]) / float64(totalMsgCount)
		if stickerRatio > 0.15 {
			addF(8, fmt.Sprintf("tukang kirim sticker (%.0f%%)", stickerRatio*100))
		} else if stickerRatio < 0.02 && totalMsgCount > 50 {
			addM(4, "jarang kirim sticker")
		}
		vnRatio := float64(typeCount["audio"]) / float64(totalMsgCount)
		if vnRatio > 0.1 {
			addF(5, fmt.Sprintf("sering VN (%.0f%%)", vnRatio*100))
		}
	}

	// Rata-rata panjang pesan
	avgLen := float64(totalLen) / float64(len(texts))
	if avgLen < 12 {
		addF(5, "pesan rata-rata sangat pendek (ketus)")
	} else if avgLen > 40 {
		addM(4, "pesan rata-rata panjang (cerewet teknis)")
	}

	// Rasio pesan pendek
	shortRatio := float64(shortCount) / float64(len(texts))
	if shortRatio > 0.5 {
		addF(6, fmt.Sprintf("jawaban pendek dominan (%.0f%%)", shortRatio*100))
	}

	// Emoji
	emojiRatio := float64(emojiCount) / float64(len(texts))
	if emojiRatio > 0.3 {
		addF(7, fmt.Sprintf("sering pakai emoji (%.0f%% pesan)", emojiRatio*100))
	} else if emojiRatio < 0.05 {
		addM(5, "jarang pakai emoji")
	}

	// Tanda seru / tanya
	if exclaimCount > len(texts)/3 {
		addF(4, "banyak tanda seru (ekspresif)")
	}
	if questionCount > len(texts)/2 {
		addF(3, "banyak bertanya (kepo)")
	}

	// Wkwk
	if wkwkCount > len(texts)/4 {
		addM(3, "sering wkwk/haha (gaya santai)")
	}

	// ─── Verdict ───
	total := scoreF + scoreM
	if total == 0 {
		return fmt.Sprintf("🤷 Analisis %s (%d pesan):\nTidak cukup sinyal khas. Netral.", targetName, len(texts))
	}
	pctF := scoreF / total * 100
	pctM := scoreM / total * 100

	var verdict, emoji string
	if pctF > 65 {
		verdict, emoji = "PEREMPUAN", "👩"
	} else if pctM > 65 {
		verdict, emoji = "LAKI-LAKI", "👨"
	} else {
		verdict, emoji = "CAMPURAN / TIDAK JELAS", "🤷"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s *Tebakan Gender: %s*\n", emoji, targetName))
	sb.WriteString(fmt.Sprintf("Berdasarkan %d pesan teks dari %d grup.\n", len(texts), len(groupSet)))
	if sharedCount > 0 {
		sb.WriteString(fmt.Sprintf("✅ Bot satu grup dengan target di %d grup", sharedCount))
		if len(groupNames) > 0 {
			sb.WriteString(": " + strings.Join(groupNames, ", "))
		}
		sb.WriteString(".\n")
	}
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("♀ Perempuan: %.0f%%\n♂ Laki-laki: %.0f%%\n\n", pctF, pctM))
	sb.WriteString(fmt.Sprintf("Kesimpulan: *%s*\n\n", verdict))
	sb.WriteString("Sinyal yang terdeteksi:\n")
	shown := 0
	for _, r := range reasons {
		if shown >= 6 {
			break
		}
		sb.WriteString("• " + r + "\n")
		shown++
	}
	sb.WriteString("\n_For fun aja, bukan kepastian ya._")
	return sb.String()
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "tebakgender",
		Aliases:     []string{"gender"},
		Description: "tebakgender",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTebakGenderCommand(client, msg, args)
		},
	})
}
