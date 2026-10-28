package main

// fun.go — Fun games & interactive features
// ════════════════════════════════════════════════════════════
//
// Commands:
//   .tebak     → Tebak angka 1-100
//   .dadu      → Roll dice (1-6)
//   .coin      → Flip coin
//   .slot      → Slot machine
//   .8ball     → Magic 8-ball
//   .tod       → Truth or Dare
//   .wyr       → Would You Rather
//   .trivia    → Random trivia question
//   .fortune   → Daily fortune cookie
//   .love      → Love calculator
//   .roast     → AI roast (random target or @tag)
//   .gaytest   → Gay meter
//   .rate      → Rate anything (1-10)
//   .pick      → Pick between two options

import (
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Tebak Angka ────────────────────────────────────────────────

var (
	tebakMu       sync.Mutex
	tebakSessions = map[string]*tebakSession{}
)

type tebakSession struct {
	Number   int
	Tries    int
	MaxTries int
	Started  time.Time
}

// funPosMod mengembalikan hash % n yang selalu non-negatif.
// (Operator % Go bisa negatif untuk hash negatif akibat int overflow.)
func funPosMod(hash, n int) int {
	r := hash % n
	if r < 0 {
		r += n
	}
	return r
}

func handleTebakCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	chat := msg.Info.Chat.String()
	sub := strings.ToLower(strings.TrimSpace(args))

	// Satu lock untuk seluruh pemrosesan tebakan — mencegah race saat
	// dua pesan .tebak dari chat yang sama diproses bersamaan
	// (session.Tries++ sebelumnya dimutasi di luar lock).
	tebakMu.Lock()
	defer tebakMu.Unlock()

	if sub == "stop" || sub == "batal" {
		delete(tebakSessions, chat)
		return "🛑 Game tebak dihentikan"
	}

	session, exists := tebakSessions[chat]

	if !exists {
		// Start new game
		n, _ := aiRandInt(100)
		n++ // 1-100
		tebakSessions[chat] = &tebakSession{
			Number:   n,
			Tries:    0,
			MaxTries: 7,
			Started:  time.Now(),
		}
		return "🎲 *Tebak Angka*\n\nAku pikirin angka 1-100. Kamu punya 7 kesempatan.\n• Ketik: `.tebak <angka>`"
	}

	// Check if args is a number (valid range 1-100, tanpa menghabiskan kesempatan)
	var guess int
	if _, err := fmt.Sscanf(sub, "%d", &guess); err != nil || guess < 1 || guess > 100 {
		return fmt.Sprintf("❓ Ketik angka 1-100. Sisa %d kesempatan", session.MaxTries-session.Tries)
	}

	session.Tries++
	remaining := session.MaxTries - session.Tries

	if guess == session.Number {
		tries := session.Tries
		delete(tebakSessions, chat)
		return fmt.Sprintf("🎉 Tebak benar! Angkanya *%d*. Kamu tebak dalam %d kali!", session.Number, tries)
	}

	if session.Tries >= session.MaxTries {
		ans := session.Number
		delete(tebakSessions, chat)
		return fmt.Sprintf("💀 Kesempatan habis! Angkanya *%d*. Coba lagi dengan .tebak", ans)
	}

	if guess < session.Number {
		return fmt.Sprintf("⬆️ Terlalu kecil! Sisa %d kesempatan.", remaining)
	}
	return fmt.Sprintf("⬇️ Terlalu besar! Sisa %d kesempatan.", remaining)
}

// ─── Dadu ──────────────────────────────────────────────────────

func handleDaduCommand(args string) string {
	n, _ := aiRandInt(6)
	n++
	faces := []string{"⚀", "⚁", "⚂", "⚃", "⚄", "⚅"}
	emoji := "🎲"
	if n >= 1 && n <= 6 {
		emoji = faces[n-1]
	}
	return fmt.Sprintf("%s *%d*", emoji, n)
}

// ─── Coin Flip ─────────────────────────────────────────────────

func handleCoinCommand() string {
	n, _ := aiRandInt(2)
	if n == 0 {
		return "🪙 *Heads* (kepala)"
	}
	return "🪙 *Tails* (ekor)"
}

// ─── Slot Machine ──────────────────────────────────────────────

func handleSlotCommand() string {
	emojis := []string{"🍒", "🍋", "🍊", "🍇", "🔔", "⭐", "💎", "7️⃣"}
	r1, _ := aiRandInt(len(emojis))
	r2, _ := aiRandInt(len(emojis))
	r3, _ := aiRandInt(len(emojis))

	result := fmt.Sprintf("🎰 %s | %s | %s", emojis[r1], emojis[r2], emojis[r3])

	if r1 == r2 && r2 == r3 {
		if emojis[r1] == "💎" {
			return result + "\n\n💎💎💎 JACKPOT!!! 💎💎💎"
		}
		if emojis[r1] == "7️⃣" {
			return result + "\n\n7️⃣7️⃣7️⃣ BIG WIN!!! 🎉"
		}
		return result + "\n\n🎉 MENANG! 3 sama!"
	}
	if r1 == r2 || r2 == r3 || r1 == r3 {
		return result + "\n\n🤏 Hampir! 2 sama."
	}
	return result + "\n\n❌ Zonk. Coba lagi!"
}

// ─── Magic 8-Ball ──────────────────────────────────────────────

func handle8BallCommand(args string) string {
	if strings.TrimSpace(args) == "" {
		return "❓ *Cara pakai:* .8ball <pertanyaan>\nContoh: `.8ball aku ganteng nggak?`"
	}
	answers := []string{
		"Ya, pasti! ✅",
		"Jelas iya! 💯",
		"Tanpa ragu! ⭐",
		"Definitely! 🎉",
		"Kayaknya iya~ 🤔",
		"Coba tanya lagi nanti 😏",
		"Nggak bisa jawab sekarang 🤐",
		"Fokus dulu, bertanya lagi nanti 😴",
		"Hmm, jangan berharap 😬",
		"Nggak 👎",
		"Sangat nggak! ❌",
		"Ragu-ragu... 😶",
		"Coba pikir sendiri 🧠",
		"Nomor undian: mungkin 🎲",
		"Google aja 😂",
		"Peluang kecil tapi ada 💫",
		"100% iya! 🔥",
		"Titik! 📌",
		"Sumber ngatakan iya 📰",
		"Konfirmasi! ✅",
	}
	n, _ := aiRandInt(len(answers))
	return fmt.Sprintf("🎱 %s", answers[n])
}

// ─── Truth or Dare ─────────────────────────────────────────────

func handleTODCommand(args string) string {
	sub := strings.ToLower(strings.TrimSpace(args))
	if sub == "" {
		return "❓ *Cara pakai:* .tod truth atau .tod dare"
	}

	truths := []string{
		"Siapa orang yang paling kamu suka di grup ini?",
		"Apa rahasia yang belum pernah kamu ceritakan ke siapapun?",
		"Kalau bisa hapus satu chat, chat mana yang kamu hapus?",
		"Siapa ex yang masih kamu pikirin?",
		"Pernah bohong ke orang tua? Tentang apa?",
		"Apa hal paling memalukan yang pernah kamu lakuin?",
		"Kalau bisa jadi invisible sehari, mau ngapain?",
		"Siapa di grup ini yang paling kamu tidak suka? (jujur!)",
		"Pernah nge-stalk mantan? Berapa lama?",
		"Apa ketakutan terbesar kamu?",
	}

	dares := []string{
		"Kirim voice note nyanyi lagu anak-anak!",
		"Ganti nama jadi 'Si Manis' selama 1 jam!",
		"Kirim foto selfie dengan ekspresi paling jelek!",
		"Kasih compliment ke 3 orang di grup ini!",
		"Cerita pengalaman paling memalukanmu dalam 3 kalimat!",
		"Kirim stiker paling random yang kamu punya!",
		"Panggil teman kamu dengan sebutan 'sayang' selama 5 menit!",
		"Kirim pesan ke orang terakhir yang chat kamu: 'aku kangen'",
		"Bikin pantun jelek tentang topik yang grup pilih!",
		"Voice note dengan suara bebek!",
	}

	if sub == "truth" || sub == "t" {
		n, _ := aiRandInt(len(truths))
		return fmt.Sprintf("🤔 *Truth*\n\n%s", truths[n])
	}
	if sub == "dare" || sub == "d" {
		n, _ := aiRandInt(len(dares))
		return fmt.Sprintf("😈 *Dare*\n\n%s", dares[n])
	}
	return "❓ *Cara pakai:* .tod truth atau .tod dare"
}

// ─── Would You Rather ──────────────────────────────────────────

func handleWYRCommand() string {
	questions := []string{
		"Mampu terbang ATAU bisa menghilang?",
		"Makan nasi pakai kecap ATAU nasi pakai susu?",
		"Tidak pernah bisa pakai HP lagi ATAU tidak pernah bisa keluar rumah lagi?",
		"Suka sama orang yang tidak suka kamu ATAU suka sama orang yang suka kamu tapi kamu tidak suka?",
		"Kaya tapi sendirian ATAU miskin tapi dikelilingi teman?",
		"Tahu kapan kamu meninggal ATAU tahu bagaimana kamu meninggal?",
		"Punya kekuatan super tapi semua orang tahu ATAU tidak punya kekuatan tapi hidup tenang?",
		"Selalu telat 5 menit ATAU selalu datang 5 menit lebih awal?",
		"Bisa bicara sama hewan ATAU bisa baca pikiran orang?",
		"Hilang semua foto ATAU hilang semua musik?",
		"Kerja 4 hari seminggu 10 jam ATAU 6 hari 6 jam?",
		"Tidak bisa berbohong ATAU orang tidak percaya kata-katamu?",
		"Punya WiFi gratis seumur hidup ATAU makan gratis seumur hidup?",
		"Jadi terkenal tapi dibenci ATAU tidak dikenal tapi disayang?",
		"Rewind waktu ATAU pause waktu?",
	}
	n, _ := aiRandInt(len(questions))
	return fmt.Sprintf("🤔 *Would You Rather*\n\n%s", questions[n])
}

// ─── Trivia ─────────────────────────────────────────────────────

func handleTriviaCommand() string {
	trivias := []string{
		"Berapa jumlah planet di tata surya? (Jawaban: 8)",
		"Ibukota Australia? (Jawaban: Canberra)",
		"Siapa penemu telepon? (Jawaban: Alexander Graham Bell)",
		"Berapa kecepatan cahaya? (Jawaban: ~300.000 km/detik)",
		"Apa simbol kimia untuk emas? (Jawaban: Au)",
		"Berapa jumlah tulang di tubuh manusia dewasa? (Jawaban: 206)",
		"Planet terdekat dengan matahari? (Jawaban: Merkurius)",
		"Siapa presiden pertama Indonesia? (Jawaban: Soekarno)",
		"Apa bahasa yang paling banyak dipakai di dunia? (Jawaban: Mandarin)",
		"Berapa jumlah benua di dunia? (Jawaban: 7)",
		"Apa guna tertinggi di dunia? (Jawaban: Everest, 8849m)",
		"Tahun kapan Indonesia merdeka? (Jawaban: 1945)",
		"Apa lautan terbesar di dunia? (Jawaban: Pasifik)",
		"Siapa penulis novel 'Bumi Manusia'? (Jawaban: Pramoedya)",
		"Apa simbol kimia untuk air? (Jawaban: H2O)",
	}
	n, _ := aiRandInt(len(trivias))
	return fmt.Sprintf("🧠 *Trivia*\n\n%s", trivias[n])
}

// ─── Fortune Cookie ────────────────────────────────────────────

func handleFortuneCommand(msg *events.Message) string {
	fortunes := []string{
		"Hari ini keberuntungan ada di pihakmu. Lakukan sesuatu yang berani!",
		"Seseorang sedang memikirkanmu. Siapa ya?",
		"Kesempatan emas akan datang minggu ini. Jangan lewatkan!",
		"Hari ini bukan harimu. Tapi besok bisa jadi ya.",
		"Uang akan datang dari arah yang tidak disangka.",
		"Jangan terlalu keras kepala. Dengarkan orang lain hari ini.",
		"Percaya sama instingmu. Dia nggak pernah salah.",
		"Seseorang di sekitarmu menyimpan perasaan. Coba perhatikan.",
		"Kesuksesan kamu bukan soal uang. Tapi soal ketenangan batin.",
		"Berhenti overthinking. Yang takut terjadi biasanya nggak terjadi.",
		"Hari ini cocok buat mulai hal baru.",
		"Peluk orang yang kamu sayang hari ini.",
		"Rezeki nggak cuma soal uang. Kadang soal ketenangan.",
		"Jangan bandingin diri kamu sama orang lain. Kamu unik.",
		"Maafin orang yang pernah nyakitin kamu. Bukan buat dia, buat kamu.",
	}
	n, _ := aiRandInt(len(fortunes))
	sender := msg.Info.PushName
	if sender == "" {
		sender = "kamu"
	}
	return fmt.Sprintf("🥠 *Fortune Cookie untuk %s*\n\n%s", sender, fortunes[n])
}

// ─── Love Calculator ───────────────────────────────────────────

func handleLoveCommand(msg *events.Message, args string) string {
	if strings.TrimSpace(args) == "" {
		return "❓ *Cara pakai:* .love @tag atau .love <nama>"
	}

	// Extract name from args or mention
	name := strings.TrimSpace(args)
	if strings.HasPrefix(name, "@") {
		name = strings.TrimPrefix(name, "@")
	}
	sender := msg.Info.PushName
	if sender == "" {
		sender = "kamu"
	}

	// Calculate love percentage (deterministic based on names)
	combined := strings.ToLower(sender + name)
	hash := 0
	for _, c := range combined {
		hash = hash*31 + int(c)
	}
	percent := funPosMod(hash, 100)
	if percent < 20 {
		percent += 30 // minimum 20%
	}

	var comment string
	switch {
	case percent >= 90:
		comment = "Soulmate! 💞"
	case percent >= 75:
		comment = "Cocok banget! 💕"
	case percent >= 60:
		comment = "Lumayan cocok~ 💗"
	case percent >= 40:
		comment = "Bisa dicoba... 🤔"
	case percent >= 25:
		comment = "Hmm, agak susah 😬"
	default:
		comment = "Mending temenan aja 😂"
	}

	return fmt.Sprintf("💕 *Love Calculator*\n\n• %s ❤️ %s\n• Match: *%d%%*\n\n%s", sender, name, percent, comment)
}

// ─── Gay Meter ─────────────────────────────────────────────────

func handleGayTestCommand(msg *events.Message, args string) string {
	name := strings.TrimSpace(args)
	if strings.HasPrefix(name, "@") {
		name = strings.TrimPrefix(name, "@")
	}
	if name == "" {
		name = msg.Info.PushName
		if name == "" {
			name = "kamu"
		}
	}

	combined := strings.ToLower(name)
	hash := 0
	for _, c := range combined {
		hash = hash*37 + int(c)
	}
	percent := funPosMod(hash, 100)

	var comment string
	switch {
	case percent >= 90:
		comment = "Fabulous! 🌈✨"
	case percent >= 70:
		comment = "Hmmm rainbow detected 🌈"
	case percent >= 50:
		comment = "Agak rainbow 🤔"
	case percent >= 30:
		comment = "Masih aman... mungkin 😏"
	default:
		comment = "Sangat straight (katanya) 😎"
	}

	return fmt.Sprintf("🌈 *Gay Meter*\n\n• %s: *%d%%* gay\n\n%s", name, percent, comment)
}

// ─── Rate ──────────────────────────────────────────────────────

func handleRateCommand(args string) string {
	target := strings.TrimSpace(args)
	if target == "" {
		return "❓ *Cara pakai:* .rate <apa saja>\nContoh: `.rate makan mie ayam`"
	}

	combined := strings.ToLower(target)
	hash := 0
	for _, c := range combined {
		hash = hash*29 + int(c)
	}
	rating := funPosMod(hash, 10) + 1 // 1-10

	var stars string
	for i := 0; i < rating; i++ {
		stars += "⭐"
	}
	for i := rating; i < 10; i++ {
		stars += "🖤"
	}

	return fmt.Sprintf("📊 *Rating: %s*\n\n%s\n• Skor: *%d/10*", target, stars, rating)
}

// ─── Pick ──────────────────────────────────────────────────────

func handlePickCommand(args string) string {
	// Split by " atau " or " or " or "|"
	parts := strings.FieldsFunc(args, func(r rune) bool {
		return r == '|' || r == ','
	})
	if len(parts) < 2 {
		// Try splitting by " atau "
		parts = strings.Split(args, " atau ")
	}
	if len(parts) < 2 {
		parts = strings.Split(args, " or ")
	}
	if len(parts) < 2 {
		return "❓ *Cara pakai:* .pick <opsi1> atau <opsi2>\nContoh: `.pick makan atau tidur`"
	}

	opt1 := strings.TrimSpace(parts[0])
	opt2 := strings.TrimSpace(parts[1])

	n, _ := aiRandInt(2)
	var chosen string
	if n == 0 {
		chosen = opt1
	} else {
		chosen = opt2
	}

	return fmt.Sprintf("🤔 *Pick*\n\n• *%s* vs *%s*\n• Pilih: *%s*", opt1, opt2, chosen)
}

// keep imports
var _ = whatsmeow.MediaAudio
var _ = events.Message{}

func init() {
	RegisterPlugin(Plugin{
		Command:     "tebak",
		Aliases:     []string{"guess"},
		Description: "tebak",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTebakCommand(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "dadu",
		Aliases:     []string{"dice"},
		Description: "dadu",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleDaduCommand(args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "coin",
		Aliases:     []string{"flip"},
		Description: "coin",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCoinCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "slot",
		Aliases:     []string{"slots"},
		Description: "slot",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSlotCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "8ball",
		Aliases:     []string{"magic8"},
		Description: "8ball",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handle8BallCommand(args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "tod",
		Description: "tod",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTODCommand(args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "wyr",
		Aliases:     []string{"wouldyourather"},
		Description: "wyr",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleWYRCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "trivia",
		Aliases:     []string{"quiz"},
		Description: "trivia",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleTriviaCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "fortune",
		Aliases:     []string{"fortune2"},
		Description: "fortune",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFortuneCommand(msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "love",
		Aliases:     []string{"lovecalc"},
		Description: "love",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleLoveCommand(msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "gaytest",
		Aliases:     []string{"gaymeter"},
		Description: "gaytest",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGayTestCommand(msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "rate",
		Aliases:     []string{"rating"},
		Description: "rate",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleRateCommand(args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "pick",
		Aliases:     []string{"choose"},
		Description: "pick",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePickCommand(args)
			// ─── Combined Features ────────────────────────────────
		},
	})
}
