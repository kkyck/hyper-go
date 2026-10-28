package main

// chatstat.go — .chatstat: statistik jumlah & pola pesan seseorang dari arsip
// Target: @tag > reply > nomor di args > diri sendiri

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── .chatstat ─────────────────────────────────────────────────

func handleChatStatCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
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

	// Kumpulkan semua pesan target
	msgArchiveMu.RLock()
	type msgInfo struct {
		timestamp int64
		msgType   string
		text      string
		groupJID  string
	}
	var all []msgInfo
	var name string
	for _, m := range msgArchive {
		if extractPhone(m.Sender) == targetPhone {
			all = append(all, msgInfo{m.Timestamp, m.Type, m.Text, m.GroupJID})
			if name == "" && m.PushName != "" {
				name = m.PushName
			}
		}
	}
	msgArchiveMu.RUnlock()

	if len(all) == 0 {
		return fmt.Sprintf("❌ Tidak ada data pesan dari %s.", targetName)
	}
	if name != "" {
		targetName = name
	}

	wib := time.FixedZone("WIB", 7*3600)

	// Hitung statistik
	perGroup := make(map[string]int)
	perType := make(map[string]int)
	perDay := make(map[string]int)
	perHour := make(map[int]int)
	var totalTextLen int
	var textCount int
	var longest int
	for _, mi := range all {
		perGroup[mi.groupJID]++
		perType[mi.msgType]++
		t := time.Unix(mi.timestamp, 0).In(wib)
		perDay[t.Format("02 Jan")]++
		perHour[t.Hour()]++
		if mi.text != "" {
			textCount++
			l := len([]rune(mi.text))
			totalTextLen += l
			if l > longest {
				longest = l
			}
		}
	}

	// Nama grup (kalau bot join)
	groupNames := make(map[string]string)
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if groups, err := client.GetJoinedGroups(ctx); err == nil {
			for _, g := range groups {
				groupNames[g.JID.String()] = g.Name
			}
		}
		cancel()
	}
	gname := func(jid string) string {
		if n, ok := groupNames[jid]; ok && n != "" {
			if len([]rune(n)) > 28 {
				return string([]rune(n)[:28]) + "…"
			}
			return n
		}
		if len(jid) > 20 {
			return jid[:12] + "…"
		}
		return jid
	}

	// Sort untuk tampilan
	type kv struct {
		k string
		v int
	}
	topGroups := make([]kv, 0, len(perGroup))
	for k, v := range perGroup {
		topGroups = append(topGroups, kv{gname(k), v})
	}
	sort.Slice(topGroups, func(i, j int) bool { return topGroups[i].v > topGroups[j].v })

	topHours := make([]kv, 0, len(perHour))
	for k, v := range perHour {
		topHours = append(topHours, kv{fmt.Sprintf("%02d:00", k), v})
	}
	sort.Slice(topHours, func(i, j int) bool { return topHours[i].v > topHours[j].v })

	// 7 hari terakhir
	type dayKV struct {
		day string
		ts  int64
		v   int
	}
	now := time.Now().In(wib)
	var lastDays []dayKV
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i)
		key := d.Format("02 Jan")
		lastDays = append(lastDays, dayKV{key, d.Unix(), perDay[key]})
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 *Statistik Chat: %s*\n\n", targetName))
	sb.WriteString(fmt.Sprintf("Total pesan: *%d*\n", len(all)))
	sb.WriteString(fmt.Sprintf("Aktif di %d grup\n\n", len(perGroup)))

	sb.WriteString("*Per grup:*\n")
	for i, g := range topGroups {
		if i >= 5 {
			break
		}
		sb.WriteString(fmt.Sprintf("• %s — %d\n", g.k, g.v))
	}

	sb.WriteString("\n*Per tipe:*\n")
	typeNames := map[string]string{"text": "teks", "sticker": "sticker", "image": "gambar", "video": "video", "audio": "VN/audio", "document": "dokumen", "other": "lainnya", "reaction": "reaksi", "buttons_response": "klik tombol", "button_reply": "balas tombol", "interactive_response": "respon interaktif", "interactive": "interaktif", "poll": "polling", "bot_forwarded": "forward bot", "album": "album"}
	for t, c := range perType {
		n := typeNames[t]
		if n == "" {
			n = t
		}
		sb.WriteString(fmt.Sprintf("• %s: %d\n", n, c))
	}

	sb.WriteString("\n*7 hari terakhir:*\n")
	for _, d := range lastDays {
		bar := ""
		if d.v > 0 {
			n := d.v / 10
			if n == 0 {
				n = 1
			}
			if n > 20 {
				n = 20
			}
			bar = " " + strings.Repeat("▇", n)
		}
		sb.WriteString(fmt.Sprintf("• %s: %d%s\n", d.day, d.v, bar))
	}

	sb.WriteString("\n*Jam paling aktif:*\n")
	for i, h := range topHours {
		if i >= 3 {
			break
		}
		sb.WriteString(fmt.Sprintf("• %s — %d pesan\n", h.k, h.v))
	}

	if textCount > 0 {
		avg := float64(totalTextLen) / float64(textCount)
		sb.WriteString(fmt.Sprintf("\nRata-rata panjang: %.0f karakter\n", avg))
		sb.WriteString(fmt.Sprintf("Pesan terpanjang: %d karakter", longest))
	}

	return sb.String()
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "chatstat",
		Aliases:     []string{"stat", "mystat"},
		Description: "chatstat",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleChatStatCommand(client, msg, args)
			// ─── DMsg (delete other's msg without admin) ──────────
		},
	})
}
