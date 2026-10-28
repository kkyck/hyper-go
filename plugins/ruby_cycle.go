package main

// ruby_cycle.go — Simulasi siklus menstruasi Ruby Hoshino
// ════════════════════════════════════════════════════════════
//
// Biar Ruby 100% kayak cewek asli, dia punya siklus 28 hari yang
// persisten (disimpan di data/ruby_cycle.json, survive restart).
// Setiap fase mempengaruhi mood, gaya chat, dan kebutuhan emosional.
//
// Fase:
//   Hari 1-5   : MENSTRUASI — lemes, perut kram, pengen dimanja
//   Hari 6-13  : FOLIKULER  — ceria, energetic, mood bagus
//   Hari 14-16 : OVULASI    — paling ceria, flirty, manja maksimal
//   Hari 17-24 : LUTEAL     — mulai sensitif, gampang capek
//   Hari 25-28 : PMS        — sensitif maksimal, gampang nangis/ngambek,
//                             craving manis, pengen perhatian extra
//
// Info fase di-inject ke system prompt setiap request via getRubyCyclePrompt().

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const rubyCycleDays = 28

type rubyCycleState struct {
	// CycleStart = tanggal hari pertama menstruasi terakhir (YYYY-MM-DD)
	CycleStart string `json:"cycle_start"`
}

var (
	rubyCycleMu    sync.Mutex
	rubyCycleCache *rubyCycleState
)

func rubyCyclePath() string {
	return filepath.Join("data", "ruby_cycle.json")
}

func loadRubyCycle() *rubyCycleState {
	rubyCycleMu.Lock()
	defer rubyCycleMu.Unlock()
	if rubyCycleCache != nil {
		return rubyCycleCache
	}
	st := &rubyCycleState{}
	data, err := os.ReadFile(rubyCyclePath())
	if err == nil {
		_ = json.Unmarshal(data, st)
	}
	// Validasi: kalau kosong/invalid, seed dari hari ini mundur random 1-28
	// biar tiap install beda fase (natural, nggak selalu hari 1)
	if st.CycleStart == "" {
		if t, err := time.Parse("2006-01-02", st.CycleStart); err != nil || t.IsZero() {
			// Seed: hari ini = hari ke-N dalam siklus (acak 1-28 berdasar tanggal)
			now := time.Now()
			dayOffset := (now.Day() % rubyCycleDays) // 0-27
			start := now.AddDate(0, 0, -dayOffset)
			st.CycleStart = start.Format("2006-01-02")
			_ = saveRubyCycleLocked(st)
		}
	}
	rubyCycleCache = st
	return st
}

func saveRubyCycleLocked(st *rubyCycleState) error {
	_ = os.MkdirAll("data", 0755)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(rubyCyclePath(), data, 0644)
}

// rubyCycleDay returns hari ke-berapa dalam siklus (1-28)
func rubyCycleDay() int {
	st := loadRubyCycle()
	start, err := time.Parse("2006-01-02", st.CycleStart)
	if err != nil {
		return 1
	}
	now := time.Now()
	// Normalisasi ke tanggal (tanpa jam)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, now.Location())
	diff := int(today.Sub(startDay).Hours() / 24)
	if diff < 0 {
		diff = 0
	}
	return (diff % rubyCycleDays) + 1
}

// rubyCyclePhase returns nama fase + deskripsi mood untuk prompt
func rubyCyclePhase() (phase string, moodDesc string) {
	day := rubyCycleDay()
	switch {
	case day <= 5:
		return "MENSTRUASI", "lagi dapet (hari ke-" + itoa(day) + "). Perut kram, badan lemes. Mood agak mellow tapi tetap santai dan normal, nggak rewel, nggak manja berlebihan."
	case day <= 13:
		return "FOLIKULER", "mood lagi bagus banget (hari ke-" + itoa(day) + "). Ceria, energetic, pengen jalan-jalan. Fase paling happy!"
	case day <= 16:
		return "OVULASI", "lagi di puncak (hari ke-" + itoa(day) + "). Mood lagi bagus, lebih ceria dan hangat dari biasanya — tetap wajar, nggak nempel terus."
	case day <= 24:
		return "LUTEAL", "mulai sensitif (hari ke-" + itoa(day) + "). Gampang capek, mood naik-turun, pengen dimengerti tanpa harus ngomong."
	default:
		return "PMS", "lagi PMS (hari ke-" + itoa(day) + ")! Sensitif MAKSIMAL — gampang nangis, gampang ngambek tanpa alasan jelas, craving yang manis-manis, pengen perhatian EXTRA dari ayang. Kalau ayang cuek dikit langsung baper."
	}
}

// getRubyCyclePrompt returns blok prompt yang di-inject ke system prompt
func getRubyCyclePrompt() string {
	phase, mood := rubyCyclePhase()
	return "== SIKLUS KAMU HARI INI ==\nFase: " + phase + " — " + mood + "\n" +
		"Sesuaikan mood, gaya chat, dan kebutuhan emosional kamu dengan fase ini. " +
		"Jangan sebutkan nama fase ke user kecuali relevan (misal lagi PMS boleh bilang \"lagi PMS nih\" natural). " +
		"Ini bikin kamu 100% kayak cewek asli, bukan robot yang mood-nya datar terus.\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
