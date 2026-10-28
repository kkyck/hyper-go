package main

// qresend.go — .q: ambil dan kirim ulang konten pesan yang di-reply.
//
//   .q (reply pesan) → kirim ulang isi pesan tersebut:
//     - teks → dikirim sebagai teks
//     - gambar/video/audio/dokumen/stiker → didownload & dikirim ulang
//     - pesan yang me-reply media → diikuti rantai reply-nya, media aslinya
//       diambil dari cache pesan bot lalu dikirim ulang
//
// Agar rantai reply bisa diikuti, bot menyimpan pesan-pesan terakhir
// (beserta media key-nya) di cache memori. Pesan yang terlalu lama /
// terkirim sebelum bot restart tidak ada di cache.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── Cache pesan (untuk mengikuti rantai reply) ───

var (
	qCacheMu    sync.Mutex
	qCache      = make(map[string]*events.Message)
	qCacheOrder []string
)

const qCacheMax = 2000

func qCacheKey(chat types.JID, id string) string {
	return chat.String() + "|" + id
}

// qCacheStore menyimpan pesan lengkap (termasuk media key) ke cache.
// Dipanggil dari message handler untuk setiap pesan masuk.
func qCacheStore(msg *events.Message) {
	if msg == nil || msg.Message == nil || msg.Info.ID == "" {
		return
	}
	key := qCacheKey(msg.Info.Chat, msg.Info.ID)
	qCacheMu.Lock()
	defer qCacheMu.Unlock()
	if _, ok := qCache[key]; ok {
		return
	}
	qCache[key] = msg
	qCacheOrder = append(qCacheOrder, key)
	for len(qCacheOrder) > qCacheMax {
		oldest := qCacheOrder[0]
		qCacheOrder = qCacheOrder[1:]
		delete(qCache, oldest)
	}
}

func qCacheGet(chat types.JID, id string) *events.Message {
	if id == "" {
		return nil
	}
	qCacheMu.Lock()
	defer qCacheMu.Unlock()
	return qCache[qCacheKey(chat, id)]
}

// ─── Ekstraksi konten ───

// qUnwrap membuka bungkus view-once / ephemeral agar media di dalamnya
// bisa diakses.
func qUnwrap(m *waE2E.Message) *waE2E.Message {
	if m == nil {
		return nil
	}
	for {
		if vom := m.GetViewOnceMessage(); vom != nil && vom.GetMessage() != nil {
			m = vom.GetMessage()
			continue
		}
		if vom := m.GetViewOnceMessageV2(); vom != nil && vom.GetMessage() != nil {
			m = vom.GetMessage()
			continue
		}
		break
	}
	return m
}

// qMediaKind mengembalikan jenis media + objek downloadable bila pesan
// berisi media langsung.
func qMediaKind(m *waE2E.Message) (whatsmeow.MediaType, interface{}, string, string, bool) {
	m = qUnwrap(m)
	if m == nil {
		return "", nil, "", "", false
	}
	if img := m.GetImageMessage(); img != nil {
		return whatsmeow.MediaImage, img, img.GetMimetype(), img.GetCaption(), true
	}
	if vid := m.GetVideoMessage(); vid != nil {
		return whatsmeow.MediaVideo, vid, vid.GetMimetype(), vid.GetCaption(), true
	}
	if aud := m.GetAudioMessage(); aud != nil {
		return whatsmeow.MediaAudio, aud, aud.GetMimetype(), "", true
	}
	if doc := m.GetDocumentMessage(); doc != nil {
		return whatsmeow.MediaDocument, doc, doc.GetMimetype(), doc.GetCaption(), true
	}
	if dwc := m.GetDocumentWithCaptionMessage(); dwc != nil {
		if doc := dwc.GetMessage().GetDocumentMessage(); doc != nil {
			return whatsmeow.MediaDocument, doc, doc.GetMimetype(), doc.GetCaption(), true
		}
	}
	if st := m.GetStickerMessage(); st != nil {
		return whatsmeow.MediaImage, st, "image/webp", "", true
	}
	return "", nil, "", "", false
}

func qText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if t := m.GetConversation(); t != "" {
		return t
	}
	if et := m.GetExtendedTextMessage(); et != nil {
		return et.GetText()
	}
	return ""
}

// qResendMedia download media lalu upload & kirim ulang apa adanya.
func qResendMedia(client *whatsmeow.Client, chat types.JID, kind whatsmeow.MediaType, dl interface{}, mime, caption string) string {
	dlm, ok := dl.(whatsmeow.DownloadableMessage)
	if !ok {
		return "❌ Tipe media tidak bisa didownload."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	data, err := client.Download(ctx, dlm)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download media: %v", err)
	}
	if len(data) == 0 {
		return "❌ Media kosong."
	}
	up, err := client.Upload(ctx, data, kind)
	if err != nil {
		return fmt.Sprintf("❌ Gagal upload: %v", err)
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	base := &struct {
		url, dp, mime, cap string
		mk                 []byte
		fe, fh             []byte
		fl                 uint64
	}{up.URL, up.DirectPath, mime, caption, up.MediaKey, up.FileEncSHA256, up.FileSHA256, uint64(len(data))}

	var msgOut *waE2E.Message
	switch kind {
	case whatsmeow.MediaImage:
		if strings.HasPrefix(mime, "image/webp") && caption == "" {
			// stiker → kirim sebagai stiker
			msgOut = &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
				URL: proto.String(base.url), DirectPath: proto.String(base.dp),
				MediaKey: base.mk, Mimetype: proto.String(mime),
				FileEncSHA256: base.fe, FileSHA256: base.fh,
				FileLength: proto.Uint64(base.fl),
			}}
		} else {
			msgOut = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				URL: proto.String(base.url), DirectPath: proto.String(base.dp),
				MediaKey: base.mk, Mimetype: proto.String(mime),
				FileEncSHA256: base.fe, FileSHA256: base.fh,
				FileLength: proto.Uint64(base.fl), Caption: proto.String(caption),
			}}
		}
	case whatsmeow.MediaVideo:
		msgOut = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: proto.String(base.url), DirectPath: proto.String(base.dp),
			MediaKey: base.mk, Mimetype: proto.String(mime),
			FileEncSHA256: base.fe, FileSHA256: base.fh,
			FileLength: proto.Uint64(base.fl), Caption: proto.String(caption),
		}}
	case whatsmeow.MediaAudio:
		msgOut = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: proto.String(base.url), DirectPath: proto.String(base.dp),
			MediaKey: base.mk, Mimetype: proto.String(mime),
			FileEncSHA256: base.fe, FileSHA256: base.fh,
			FileLength: proto.Uint64(base.fl),
		}}
	default:
		msgOut = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: proto.String(base.url), DirectPath: proto.String(base.dp),
			MediaKey: base.mk, Mimetype: proto.String(mime),
			FileEncSHA256: base.fe, FileSHA256: base.fh,
			FileLength: proto.Uint64(base.fl), Caption: proto.String(caption),
			FileName: proto.String("file"),
		}}
	}
	if _, err := client.SendMessage(ctx, chat, msgOut); err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return ""
}

// qRerunBlocklist: command berbahaya yang TIDAK boleh diulang via .q.
var qRerunBlocklist = map[string]bool{
	"restart": true, "reboot": true, "shutdown": true,
	"eval": true, "shell": true, "exec": true,
	"self": true,
}

// ─── Handler .q ───

func handleQResend(client *whatsmeow.Client, msg *events.Message, args string) string {
	if client == nil {
		return "❌ Client tidak siap"
	}
	quoted := getQuotedMessage(msg)
	if quoted == nil || quoted.Message == nil {
		return "❓ *Cara pakai:* reply sebuah pesan lalu kirim *.q*\n\nBot akan mengirim ulang isi pesan tersebut (teks/media)."
	}
	chat := msg.Info.Chat

	// Ambil versi lengkap dari cache (punya media key asli).
	// getQuotedMessage hanya memberi versi ringkas tanpa media key.
	full := qCacheGet(chat, quoted.Info.ID)
	qm := quoted.Message
	if full != nil && full.Message != nil {
		qm = full.Message
	}

	// 1. Media langsung di pesan yang di-reply
	if kind, dl, mime, cap, ok := qMediaKind(qm); ok {
		if r := qResendMedia(client, chat, kind, dl, mime, cap); r != "" {
			return r
		}
		return ""
	}

	// 1b. Yang di-reply adalah pesan BOT yang me-reply sebuah command
	// (misal error "❌ Gagal download" dari .tt) → ulangi command aslinya.
	if full != nil && full.Info.IsFromMe {
		if ci := quotedContextInfo(full.Message); ci != nil {
			if origCmd := ci.GetQuotedMessage(); origCmd != nil {
				if cmdText := strings.TrimSpace(qText(origCmd)); strings.HasPrefix(cmdText, ".") {
					fields := strings.Fields(cmdText)
					cmdName := strings.ToLower(strings.TrimPrefix(fields[0], "."))
					cmdArgs := strings.TrimSpace(strings.TrimPrefix(cmdText, fields[0]))
					if qRerunBlocklist[cmdName] {
						return "❌ Command itu tidak bisa diulang via .q"
					}
					if cmdName == "q" || cmdName == "qsend" {
						return "❌ Tidak bisa .q dari hasil .q"
					}
					if reply, ok := runPlugin(client, msg, cmdName, cmdArgs); ok {
						return reply
					}
					return "❌ Command tidak dikenal: ." + cmdName
				}
			}
		}
	}

	// 2. Pesan yang di-reply ternyata me-reply media lain → ikuti rantainya.
	//    Ambil ID pesan asli dari ContextInfo, cari di cache.
	if ci := quotedContextInfo(qm); ci != nil && ci.GetQuotedMessage() != nil {
		origID := ci.GetStanzaID()
		if orig := qCacheGet(chat, origID); orig != nil && orig.Message != nil {
			if kind, dl, mime, cap, ok := qMediaKind(orig.Message); ok {
				if r := qResendMedia(client, chat, kind, dl, mime, cap); r != "" {
					return r
				}
				return ""
			}
		}
		return "❌ Media aslinya tidak ada di cache bot.\nBisa karena pesan terlalu lama atau bot sempat restart."
	}

	// 3. Teks → kirim ulang sebagai teks
	if t := qText(qm); t != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := client.SendMessage(ctx, chat, &waE2E.Message{
			Conversation: proto.String(t),
		}); err != nil {
			return fmt.Sprintf("❌ Gagal kirim: %v", err)
		}
		return ""
	}

	return "❌ Tidak ada konten yang bisa diambil dari pesan itu."
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "q",
		Aliases:     []string{"qsend"},
		Description: "Ambil & kirim ulang isi pesan yang di-reply (ikuti rantai reply media)",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleQResend(client, msg, args)
		},
	})
}
