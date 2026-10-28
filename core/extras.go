package main

import (
	"fmt"
	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
	"math/rand"
	"os"
)

/**
 * ✨ extras.go — Quotes, fakta, helpers
 * ════════════════════════════════════════════════════════════
 * Commands (prefix .):
 *   .quote / .quotes → Quote random
 *   .fakta / .fact   → Fakta random
 *   .groupinfo / .ginfo → handled in main.go (groupinfo cmd)
 *   .hidetag / .ht   → handled in main.go
 */

var quotes = []string{
	"Jangan takut gagal. Takutlah tidak mencoba.",
	"Konsistensi mengalahkan motivasi yang datang sesaat.",
	"Kerja keras tidak mengkhianati hasil — tapi arah yang salah tetap sia-sia.",
	"Istirahat itu bagian dari produktivitas, bukan kemalasan.",
	"Kode yang bersih hari ini = hutang teknis lebih sedikit besok.",
	"Belajar dari error lebih cepat daripada takut compile.",
	"Jangan bandingkan chapter 1-mu dengan chapter 20 orang lain.",
	"Progress kecil setiap hari > heroik seminggu sekali lalu hilang.",
	"Kalau bot bisa online 24 jam, kamu juga bisa istirahat teratur 😄",
	"Satu commit bermakna lebih baik daripada seribu TODO kosong.",
	"Kesuksesan: gagal berkali-kali tanpa kehilangan semangat.",
	"Fokus ke proses; hasil akan mengikuti.",
}

var faktaList = []string{
	"Otak manusia menghasilkan listrik sekitar 12–25 watt — cukup nyalain LED kecil.",
	"Go diluncurkan Google tahun 2009; maskotnya si Gopher.",
	"WhatsApp memakai protokol Signal untuk E2E encryption.",
	"Madu tidak pernah basi jika disimpan benar — arkeolog pernah temukan madu kuno yang masih bisa dimakan.",
	"Octopus punya 3 jantung.",
	"Bahasa Indonesia termasuk rumpun Austronesia.",
	"GC di Go berjalan concurrent; biasanya pause-nya singkat.",
	"Bumi berputar ~1670 km/jam di ekuator.",
	"Password '123456' masih sering masuk top leaked password dunia.",
	"SQLite adalah database paling banyak dipakai di dunia (ada di hampir semua HP).",
	"Satu grup WhatsApp bisa punya hingga ribuan member (tergantung tipe).",
	"QR login WhatsApp multi-device diganti berkala demi keamanan.",
}

func handleQuoteCommand() string {
	q := quotes[rand.Intn(len(quotes))]
	return fmt.Sprintf("💬 *Quote*\n\n_%s_", q)
}

func handleFaktaCommand() string {
	f := faktaList[rand.Intn(len(faktaList))]
	return fmt.Sprintf("🧠 *Fakta random*\n\n%s", f)
}

// getenvDefault reads an env var with fallback.
func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "quote",
		Aliases:     []string{"quotes"},
		Description: "quote",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleQuoteCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "fakta",
		Aliases:     []string{"fact"},
		Description: "fakta",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFaktaCommand()
		},
	})
}
