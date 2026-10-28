package main

// a2ui_nodes.go — Additional node "biz" untuk pesan interactive A2UI.
// ══════════════════════════════════════════════════════════════════
//
// WhatsApp butuh node `<biz>` berisi `<interactive>` + `<quality_control>`
// sebagai AdditionalNodes saat mengirim pesan interactive modus A2UI.
//
// Fungsi ini dipakai jalur resend/reply pada Web UI, di mana node tambahan
// tidak ikut tersimpan di blob/cache dan harus dibangun ulang tiap kali.

import (
	"strconv"
	"strings"
	"time"

	waBinary "github.com/polymorfa/hypermeow/binary"
)

// a2uiBizNode membangun node <biz> native_flow v9 "mixed" + quality_control
// yang wajib dikirim sebagai AdditionalNodes untuk pesan interactive A2UI.
func a2uiBizNode() waBinary.Node {
	return waBinary.Node{
		Tag: "biz",
		Attrs: waBinary.Attrs{
			"actual_actors":   "2",
			"host_storage":    "2",
			"privacy_mode_ts": strconv.FormatInt(time.Now().Unix(), 10),
		},
		Content: []waBinary.Node{
			{
				Tag:   "interactive",
				Attrs: waBinary.Attrs{"type": "native_flow", "v": "1"},
				Content: []waBinary.Node{
					{Tag: "native_flow", Attrs: waBinary.Attrs{"v": "9", "name": "mixed"}},
				},
			},
			{
				Tag:   "quality_control",
				Attrs: waBinary.Attrs{"decision_id": strings.ReplaceAll(generateUUID(), "-", ""), "source_type": "third_party"},
				Content: []waBinary.Node{
					{Tag: "decision_source", Attrs: waBinary.Attrs{"value": "df"}},
				},
			},
		},
	}
}
