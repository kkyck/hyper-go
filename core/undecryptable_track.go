package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Passive tracker: pesan grup yang TIDAK bisa didekripsi device ini ──
// Scope: grup NIXCODE (NNN + 2K26). Sebagian pengirim di sana memakai
// fanout sender-key terbatas (pola SPR): ciphertext hanya didistribusikan
// ke device target, device secondary (bot) menerima stanza kosong
// (IsUnavailable) sehingga isi pesan tidak bisa dibaca dari device ini.
// Tracker ini HANYA mencatat metadata kejadian — bukan tindakan balasan.

var undecryptableTrackChats = map[string]bool{
	"120363410933636228@g.us": true, // NIXCODE || NEW NORMAL (NNN)
	"120363412018002178@g.us": true, // NIXCODE 2K26
}

const undecryptableLogFile = "data/undecryptable_log.jsonl"
const undecryptableLogMaxSize = 1 << 20 // 1MB, rotate: simpan 500 baris terakhir

var undecryptableLogMu sync.Mutex

type undecryptableEntry struct {
	Timestamp       time.Time `json:"ts"`
	Chat            string    `json:"chat"`
	Sender          string    `json:"sender"`
	PushName        string    `json:"push_name,omitempty"`
	MsgID           string    `json:"msg_id"`
	IsUnavailable   bool      `json:"is_unavailable"`
	DecryptFailMode string    `json:"decrypt_fail_mode"`
}

func trackUndecryptable(um *events.UndecryptableMessage) {
	if um == nil || !um.Info.IsGroup || um.Info.IsFromMe {
		return
	}
	chat := um.Info.Chat.String()
	if !undecryptableTrackChats[chat] {
		return
	}
	entry := undecryptableEntry{
		Timestamp:       time.Now(),
		Chat:            chat,
		Sender:          um.Info.Sender.String(),
		PushName:        um.Info.PushName,
		MsgID:           string(um.Info.ID),
		IsUnavailable:   um.IsUnavailable,
		DecryptFailMode: fmt.Sprint(um.DecryptFailMode),
	}

	undecryptableLogMu.Lock()
	defer undecryptableLogMu.Unlock()
	_ = os.MkdirAll("data", 0o755)
	if st, err := os.Stat(undecryptableLogFile); err == nil && st.Size() > undecryptableLogMaxSize {
		if data, err := os.ReadFile(undecryptableLogFile); err == nil {
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(lines) > 500 {
				lines = lines[len(lines)-500:]
			}
			_ = os.WriteFile(undecryptableLogFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		}
	}
	line, _ := json.Marshal(entry)
	f, err := os.OpenFile(undecryptableLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()

	fmt.Printf("[undecryptable-track] %s di %s dari %s (unavailable=%v, failMode=%s)\n",
		entry.MsgID, chat, entry.Sender, entry.IsUnavailable, entry.DecryptFailMode)
}
