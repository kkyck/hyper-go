package main

import (
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
)

// ─── ANTIBOT: samarkan pesan bot sebagai pesan official client ────────────
// Perintah user (2026-10-14): "bypass antibot apapun itu dengan membuat
// seolah-olah pesan bot adalah pesan yang dikirim dari client resmi".
//
// Fingerprint bot yang dihapus:
//  1. MessageContextInfo.botMetadata — TIDAK PERNAH dikirim official client.
//     Bahkan struct kosong (&BotMetadata{}) adalah fingerprint (field present
//     tapi kosong vs nil sama sekali).
//  2. (Dipertahankan) humanizeMessage: messageSecret 32-byte, deviceListMetadata,
//     deviceListMetadataVersion=2 — semua ini official client juga kirim.
//
// Batasan jujur: device ID pengirim (:1 companion) adalah level protokol dan
// tidak bisa disamarkan jadi :0 (primary phone). Yang bisa kita samarkan adalah
// isi pesannya — dan itu yang dicek probe antibot berbasis quote (cRT).
//
// stripBotMarkers dipanggil dari humanizeMessage() sehingga otomatis mencakup
// semua jalur humanSend*. Untuk pesan yang dibangun manual di plugins (richcard,
// notify, dll), lampiran BotMetadata dihapus langsung di sumbernya.
func stripBotMarkers(msg *waE2E.Message) *waE2E.Message {
	if msg == nil {
		return msg
	}
	if mci := msg.GetMessageContextInfo(); mci != nil {
		mci.BotMetadata = nil
	}
	return msg
}
