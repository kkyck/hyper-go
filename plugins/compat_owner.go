package main

// owner_extras.go — Extra owner commands (v2.34)
// ════════════════════════════════════════════════════════════
//
// Commands implemented in this file:
//
//   .stealth on|off|status
//        Alias for .self spr on|off|status (per-group Self SPR mode).
//        Toggles whether bot replies in THIS group are routed via SPR Hide.
//
//   .owner
//        Shows owner list with REDACTED names (only the triggering owner
//        sees their own full info; all other owners' names/phones are masked).
//        When OWNER_BRANDING=on env is set, also sends a native rich card
//        branding card (banner + entity card + body + footer).
//
//   .listgroup
//        Lists every group the bot has joined: JID + name + member count.
//
//   .listmsg [group_jid] [user_phone_or_jid]
//        - .listmsg                         → recent messages in current chat
//        - .listmsg <group_jid>             → recent messages in that group
//        - .listmsg <group_jid> <user>      → messages from that user in that group
//        Each entry: timestamp, sender, type, text (or [MEDIA:type]).
//
//   .restoremsg <id>
//        Restores a single archived message by ID. Re-sends it to the
//        current chat (text + media if any). No data loss.
//
//   .rvo  /  .readviewonce
//        Reply a view-once image/video/audio message → bot downloads it
//        (one-time access) and re-sends it to the same chat as a NORMAL
//        (non-view-once) message. The view-once flag is stripped.
//
// All commands here are OWNER-ONLY unless noted otherwise.
//
// Data persistence:
//   - data/message_archive.json   — append-only log of recent messages
//                                   (capped at 10000 entries, FIFO)
//   - data/media_archive/          — downloaded media files (jpg, mp4, etc.)
//                                   named by message ID
// ════════════════════════════════════════════════════════════

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Message archive (in-memory + JSON persistence) ─────────────

// archivedMessage represents one captured message.
type archivedMessage struct {
	ID          string `json:"id"`
	GroupJID    string `json:"group_jid"`
	Sender      string `json:"sender"`
	PushName    string `json:"push_name,omitempty"` // display name of sender (if available)
	Timestamp   int64  `json:"timestamp"`
	Type        string `json:"type"` // "text", "image", "video", "audio", "sticker", "document", "other"
	Text        string `json:"text"`
	MediaPath   string `json:"media_path,omitempty"`   // empty if no media saved
	RawEnvelope []byte `json:"raw_envelope,omitempty"` // full message envelope as JSON
}

const (
	msgArchiveFile       = "data/message_archive.json"
	msgArchiveMediaDir   = "data/media_archive"
	msgArchiveMaxEntries = 300
)

var (
	msgArchiveMu     sync.RWMutex
	msgArchiveLoaded bool
	msgArchive       []archivedMessage
)

func loadMsgArchive() {
	// No-op: data lives in PG message_archive table, queried on demand.
	msgArchiveLoaded = true
}

func saveMsgArchive() {
	// No-op: archiveMessage() INSERTs to PG directly.
}

// archiveMessage appends a message to the archive (thread-safe, async).
// Writes to PG message_archive v2 schema (rich columns + JSONB blobs).
// The FullEnvelope captured by captureEnvelope() is split into:
//   - filterable first-class columns (sender_user, from_me, is_group, ...)
//   - JSONB blobs (envelope, raw_message, raw_message_unwrapped, source_webmsg,
//     node_attrs, ...)
//
// This keeps simple queries fast (.listmsg, .restoremsg, .deleted) while
// preserving ALL available data from the WA message event for .insp/.crm.
func archiveMessage(m archivedMessage) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[archive] panic: %v\n", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		db, err := pgGet()
		if err != nil {
			return
		}
		msgType := m.Type
		if msgType == "" {
			msgType = "other"
		}

		// Parse the captured envelope JSON once to extract first-class columns.
		var env FullEnvelope
		hasEnvelope := false
		if len(m.RawEnvelope) > 0 && string(m.RawEnvelope) != "null" {
			if err := json.Unmarshal(m.RawEnvelope, &env); err == nil {
				hasEnvelope = true
			}
		}

		// Dedupe by msg_id — keep latest
		if m.ID != "" {
			_, _ = pgExec(ctx, db, `DELETE FROM message_archive WHERE msg_id = ?`, m.ID)
		}

		// Defaults for nullable fields
		senderUser := ""
		senderServer := ""
		fromMe := false
		isGroup := false
		addressingMode := ""
		participant := ""
		participantPN := ""
		isEphemeral := false
		isViewOnce := false
		isViewOnceV2 := false
		isViewOnceV2Ext := false
		isDocumentWithCaption := false
		isLottieSticker := false
		isBotInvoke := false
		isEdit := false
		broadcast := false
		retryCount := 0
		unavailableReqID := ""
		caption := ""
		mediaURL := ""
		mediaMime := ""
		mediaKey := ""
		var fileLength int64
		quotedMsgID := ""
		quotedParticipant := ""
		quotedMsgText := ""
		quotedMsgType := ""
		messageSecret := ""
		nodeTag := ""
		nodeAttrsJSON := "{}"
		rawMessageJSON := "{}"
		rawMessageUnwrappedJSON := "{}"
		sourceWebMsgJSON := "{}"
		newsletterMetaJSON := "{}"

		if hasEnvelope {
			senderUser = env.SenderUser
			senderServer = env.SenderServer
			fromMe = env.FromMe
			isGroup = env.IsGroup
			addressingMode = env.AddressingMode
			participant = env.Participant
			participantPN = env.ParticipantPN
			isEphemeral = env.MsgFlags.IsEphemeral
			isViewOnce = env.MsgFlags.IsViewOnce
			isViewOnceV2 = env.MsgFlags.IsViewOnceV2
			isViewOnceV2Ext = env.MsgFlags.IsViewOnceV2Extension
			isDocumentWithCaption = env.MsgFlags.IsDocumentWithCaption
			isLottieSticker = env.MsgFlags.IsLottieSticker
			isBotInvoke = env.MsgFlags.IsBotInvoke
			isEdit = env.MsgFlags.IsEdit
			broadcast = env.Broadcast
			retryCount = env.RetryCount
			unavailableReqID = env.UnavailableRequestID
			caption = env.Caption
			mediaURL = env.MediaURL
			mediaMime = env.MediaMime
			mediaKey = env.MediaKey
			fileLength = int64(env.FileLength)
			quotedMsgID = env.QuotedMsgID
			quotedParticipant = env.QuotedParticipant
			quotedMsgText = env.QuotedMsgText
			quotedMsgType = env.QuotedMsgType
			messageSecret = env.MessageSecret
			nodeTag = env.NodeTag
			if len(env.NodeAttrs) > 0 {
				if b, err := json.Marshal(env.NodeAttrs); err == nil {
					nodeAttrsJSON = string(b)
				}
			}
			if len(env.RawMessage) > 0 {
				rawMessageJSON = string(env.RawMessage)
			}
			if len(env.RawMessageUnwrapped) > 0 {
				rawMessageUnwrappedJSON = string(env.RawMessageUnwrapped)
			}
			if len(env.SourceWebMsg) > 0 {
				sourceWebMsgJSON = string(env.SourceWebMsg)
			}
			if env.NewsletterMeta != nil {
				if b, err := json.Marshal(env.NewsletterMeta); err == nil {
					newsletterMetaJSON = string(b)
				}
			}
		}

		// Ensure all JSONB blobs are valid JSON (PG will reject otherwise)
		if !json.Valid([]byte(nodeAttrsJSON)) {
			nodeAttrsJSON = "{}"
		}
		if !json.Valid([]byte(rawMessageJSON)) {
			rawMessageJSON = "{}"
		}
		if !json.Valid([]byte(rawMessageUnwrappedJSON)) {
			rawMessageUnwrappedJSON = "{}"
		}
		if !json.Valid([]byte(sourceWebMsgJSON)) {
			sourceWebMsgJSON = "{}"
		}
		if !json.Valid([]byte(newsletterMetaJSON)) {
			newsletterMetaJSON = "{}"
		}
		envelopeJSON := "{}"
		if hasEnvelope {
			envelopeJSON = string(m.RawEnvelope)
		}
		if !json.Valid([]byte(envelopeJSON)) {
			envelopeJSON = "{}"
		}

		_, err = pgExec(ctx, db,
			`INSERT INTO message_archive (
                        msg_id, group_jid, sender, sender_user, sender_server,
                        push_name, from_me, timestamp, is_group,
                        addressing_mode, participant, participant_pn,
                        is_ephemeral, is_view_once, is_view_once_v2, is_view_once_v2_ext,
                        is_document_with_caption, is_lottie_sticker, is_bot_invoke, is_edit,
                        broadcast, retry_count, unavailable_request_id,
                        type, text, caption, media_path, media_url, media_mime, media_key, file_length,
                        quoted_msg_id, quoted_participant, quoted_msg_text, quoted_msg_type,
                        message_secret,
                        node_tag, node_attrs,
                        envelope, raw_message, raw_message_unwrapped, source_webmsg, newsletter_meta
                ) VALUES (
                        ?, ?, ?, ?, ?,
                        ?, ?, ?, ?,
                        ?, ?, ?,
                        ?, ?, ?, ?,
                        ?, ?, ?, ?,
                        ?, ?, ?,
                        ?, ?, ?, ?, ?, ?, ?, ?,
                        ?, ?, ?, ?,
                        ?,
                        ?, ?::jsonb,
                        ?::jsonb, ?::jsonb, ?::jsonb, ?::jsonb, ?::jsonb
                )`,
			m.ID, m.GroupJID, m.Sender, senderUser, senderServer,
			m.PushName, fromMe, m.Timestamp, isGroup,
			addressingMode, participant, participantPN,
			isEphemeral, isViewOnce, isViewOnceV2, isViewOnceV2Ext,
			isDocumentWithCaption, isLottieSticker, isBotInvoke, isEdit,
			broadcast, retryCount, unavailableReqID,
			msgType, m.Text, caption, m.MediaPath, mediaURL, mediaMime, mediaKey, fileLength,
			quotedMsgID, quotedParticipant, quotedMsgText, quotedMsgType,
			messageSecret,
			nodeTag, nodeAttrsJSON,
			envelopeJSON, rawMessageJSON, rawMessageUnwrappedJSON, sourceWebMsgJSON, newsletterMetaJSON)
		if err != nil {
			fmt.Printf("[archive] insert error: %v\n", err)
		}
	}()
}

// handleOwnerListCommand — .owner
// Shows owner list. Triggering owner sees their OWN full info;
// all other owners' names/phones are masked (redacted).
func handleOwnerListCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	owners := ownerJIDs()
	if len(owners) == 0 {
		return "❌ OWNER_JID belum di-set"
	}

	// Default: text list (redacted for non-triggering owners)
	sender := msg.Info.Sender.ToNonAD()
	senderPhone := sender.User
	senderAlt := msg.Info.SenderAlt.ToNonAD()
	if senderAlt.User != "" {
		if strings.Contains(sender.Server, "lid") && senderAlt.Server == "s.whatsapp.net" {
			senderPhone = senderAlt.User
		}
	}

	ownerName := getenvDefault("OWNER_NAME", "Owner")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👑 *Daftar Owner* (%d)\n\n", len(owners)))

	for _, o := range owners {
		isSelf := false
		oPhone := extractPhone(o)
		if oPhone == senderPhone && oPhone != "" {
			isSelf = true
		}
		if o == sender.String() || o == senderAlt.String() {
			isSelf = true
		}

		if isSelf {
			sb.WriteString(fmt.Sprintf("• %s (YOU) — +%s\n", ownerName, oPhone))
		} else {
			sb.WriteString(fmt.Sprintf("• %s — %s\n", redactName(ownerName), redactPhone(oPhone)))
		}
	}

	return sb.String()
}

// redactName masks the middle of a name.
// "Acaa Hoshino" -> "A*** H***"
// "Acaa" -> "A***"
func redactName(name string) string {
	if name == "" {
		return "****"
	}
	parts := strings.Fields(name)
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(" ")
		}
		runes := []rune(p)
		if len(runes) <= 1 {
			b.WriteString("*")
			continue
		}
		b.WriteRune(runes[0])
		for j := 1; j < len(runes); j++ {
			b.WriteRune('*')
		}
	}
	return b.String()
}

// redactPhone masks the middle of a phone number.
// "628123456789" -> "628***6789"
func redactPhone(phone string) string {
	if phone == "" {
		return "****"
	}
	digits := phone
	prefix := ""
	if strings.HasPrefix(digits, "+") {
		prefix = "+"
		digits = digits[1:]
	}
	if len(digits) <= 4 {
		return prefix + strings.Repeat("*", len(digits))
	}
	keepFront := 3
	keepBack := 4
	if len(digits) < keepFront+keepBack {
		keepFront = len(digits) / 3
		keepBack = len(digits) / 3
	}
	middle := len(digits) - keepFront - keepBack
	if middle < 1 {
		middle = 1
	}
	return prefix + digits[:keepFront] + strings.Repeat("*", middle) + digits[len(digits)-keepBack:]
}

// redactJID masks the user portion of a JID.
// "628123456789@s.whatsapp.net" -> "628***6789@s.whatsapp.net"
func redactJID(jid string) string {
	if jid == "" {
		return "****"
	}
	at := strings.Index(jid, "@")
	if at <= 0 {
		return redactPhone(jid)
	}
	phone := jid[:at]
	domain := jid[at:]
	return redactPhone(phone) + domain
}

// keep imports referenced — whatsmeow.Client is used as func param type above.

// handleListGroupCommand — .listgroup
// Lists every group the bot has joined: compact one-line per group.
func handleListGroupCommand(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "❌ Client nil"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	groups, err := client.GetJoinedGroups(ctx)
	if err != nil {
		return fmt.Sprintf("❌ %v", err)
	}
	if len(groups) == 0 {
		return "📭 Belum join grup"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👥 *Daftar Grup* (%d)\n\n", len(groups)))
	for _, g := range groups {
		sb.WriteString(fmt.Sprintf("• *%s* — %d member\n  `%s`\n",
			g.Name, len(g.Participants), g.JID.String()))
	}
	return sb.String()
}

// ─── .listmsg — recent messages with optional filters ──────────

// handleListMsgCommand — .listmsg [group_jid] [user_phone_or_jid]
// Compact one-line per message. Each line: [DD-MM HH:MM] +phone: preview | id
func handleListMsgCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	loadMsgArchive()
	parts := strings.Fields(args)

	var filterGroup, filterUser string
	if len(parts) >= 1 {
		filterGroup = parts[0]
	}
	if len(parts) >= 2 {
		filterUser = parts[1]
	}

	if filterGroup == "" {
		filterGroup = msg.Info.Chat.String()
	}
	filterGroup = normalizeGroupJID(filterGroup)
	if filterUser != "" {
		filterUser = extractPhone(filterUser)
	}

	msgArchiveMu.RLock()
	defer msgArchiveMu.RUnlock()

	var matched []archivedMessage
	for _, m := range msgArchive {
		if filterGroup != "" && m.GroupJID != filterGroup {
			continue
		}
		if filterUser != "" {
			mu := extractPhone(m.Sender)
			if mu != filterUser {
				continue
			}
		}
		matched = append(matched, m)
	}

	if len(matched) == 0 {
		return "📭 Tidak ada pesan tersimpan"
	}

	limit := 20
	start := 0
	if len(matched) > limit {
		start = len(matched) - limit
	}
	shown := matched[start:]

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 *Riwayat Pesan*\n\n• Total: %d pesan\n• Ditampilkan: %d terakhir\n\n", len(matched), len(shown)))
	for _, m := range shown {
		ts := time.Unix(m.Timestamp, 0).In(time.FixedZone("WIB", 7*3600))
		tsStr := ts.Format("02-01 15:04")
		senderPhone := extractPhone(m.Sender)
		preview := m.Text
		if preview == "" {
			preview = fmt.Sprintf("[%s]", strings.ToUpper(m.Type))
		}
		if len(preview) > 50 {
			preview = preview[:47] + "..."
		}
		// Replace newlines in preview with space for compact view
		preview = strings.ReplaceAll(preview, "\n", " ")
		sb.WriteString(fmt.Sprintf("• [%s] +%s: %s\n  `%s`\n", tsStr, senderPhone, preview, m.ID))
	}
	return sb.String()
}

// Restores a single archived message by ID. Re-sends it to the current chat
// (text + media if any). Returns empty string on success (no follow-up text).
// ─── .rvo — read view-once media and re-send ────────────────────

// handleReadViewOnceCommand — .rvo / .readviewonce
// Owner-only. Reply a view-once image/video/audio message with .rvo.
// Bot downloads the media (one-time access) and re-sends it to the
// same chat as a NORMAL (non-view-once) message. The view-once flag is stripped.
func handleReadViewOnceCommand(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "❌ Client nil"
	}
	quoted := getQuotedMessage(msg)
	if quoted == nil || quoted.Message == nil {
		return "❌ Reply pesan view-once (gambar/video) lalu ketik *.rvo*"
	}

	// Unwrap view-once envelope
	inner := crmUnwrap(quoted.Message)
	if inner == nil {
		return "❌ Tidak bisa unwrap view-once message"
	}

	// Try image first
	if img := inner.GetImageMessage(); img != nil {
		return downloadAndResendImage(client, msg, img, "image/jpeg")
	}
	// Try video
	if vid := inner.GetVideoMessage(); vid != nil {
		mime := vid.GetMimetype()
		if mime == "" {
			mime = "video/mp4"
		}
		return downloadAndResendVideo(client, msg, vid, mime)
	}
	// Try audio
	if aud := inner.GetAudioMessage(); aud != nil {
		mime := aud.GetMimetype()
		if mime == "" {
			mime = "audio/mpeg"
		}
		return downloadAndResendAudio(client, msg, aud, mime)
	}

	return "❌ Pesan yang direply bukan image/video/audio view-once"
}

// downloadAndResendImage downloads an image message and re-sends it as a
// normal image (stripping view-once flag). Also saves to media_archive.
// Returns empty string on success (so caller doesn't send a follow-up text).
func downloadAndResendImage(client *whatsmeow.Client, msg *events.Message, img *waE2E.ImageMessage, mime string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := client.Download(ctx, img)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download: %v", err)
	}
	if len(data) == 0 {
		return "❌ Image kosong"
	}

	archivePath := saveMediaToArchive(msg.Info.ID, "image", ".jpg", data)
	archiveMessage(archivedMessage{
		ID:        msg.Info.ID,
		GroupJID:  msg.Info.Chat.String(),
		Sender:    msg.Info.Sender.String(),
		Timestamp: time.Now().Unix(),
		Type:      "image",
		Text:      img.GetCaption(),
		MediaPath: archivePath,
	})

	// Preserve original caption if any, else empty (no extra text)
	caption := img.GetCaption()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	err = sprSendImage(client, ctx2, msg.Info.Chat, data, mime, caption)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return "" // success — no follow-up text needed
}

// downloadAndResendVideo downloads a video message and re-sends it as a
// normal video (stripping view-once flag).
// Returns empty string on success (so caller doesn't send a follow-up text).
func downloadAndResendVideo(client *whatsmeow.Client, msg *events.Message, vid *waE2E.VideoMessage, mime string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	data, err := client.Download(ctx, vid)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download: %v", err)
	}
	if len(data) == 0 {
		return "❌ Video kosong"
	}

	archivePath := saveMediaToArchive(msg.Info.ID, "video", ".mp4", data)
	archiveMessage(archivedMessage{
		ID:        msg.Info.ID,
		GroupJID:  msg.Info.Chat.String(),
		Sender:    msg.Info.Sender.String(),
		Timestamp: time.Now().Unix(),
		Type:      "video",
		Text:      vid.GetCaption(),
		MediaPath: archivePath,
	})

	caption := vid.GetCaption()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel2()
	err = sprSendVideo(client, ctx2, msg.Info.Chat, data, mime, caption)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return "" // success — no follow-up text needed
}

// downloadAndResendAudio downloads an audio message and re-sends it as a
// normal audio (stripping view-once flag).
// Returns empty string on success (so caller doesn't send a follow-up text).
func downloadAndResendAudio(client *whatsmeow.Client, msg *events.Message, aud *waE2E.AudioMessage, mime string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := client.Download(ctx, aud)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download: %v", err)
	}
	if len(data) == 0 {
		return "❌ Audio kosong"
	}

	archivePath := saveMediaToArchive(msg.Info.ID, "audio", ".mp3", data)
	archiveMessage(archivedMessage{
		ID:        msg.Info.ID,
		GroupJID:  msg.Info.Chat.String(),
		Sender:    msg.Info.Sender.String(),
		Timestamp: time.Now().Unix(),
		Type:      "audio",
		Text:      "",
		MediaPath: archivePath,
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	err = sprSendAudio(client, ctx2, msg.Info.Chat, data, mime, "")
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim: %v", err)
	}
	return "" // success — no follow-up text needed
}

// saveMediaToArchive saves media bytes to data/media_archive/<id>_<kind>.<ext>
// Returns the full path, or empty string on error.
func saveMediaToArchive(msgID, kind, ext string, data []byte) string {
	_ = os.MkdirAll(msgArchiveMediaDir, 0o755)
	if msgID == "" {
		msgID = fmt.Sprintf("media_%d", time.Now().UnixNano())
	}
	safeID := sanitizeFilenameForArchive(msgID)
	path := filepath.Join(msgArchiveMediaDir, safeID+"_"+kind+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Printf("[archive] failed to save media %s: %v\n", path, err)
		return ""
	}
	return path
}

// sanitizeFilenameForArchive replaces characters unsafe for filenames.
func sanitizeFilenameForArchive(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "@", "_")
	return s
}

// normalizeGroupJID ensures the JID has a @g.us suffix.
func normalizeGroupJID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "@") {
		return s
	}
	return s + "@g.us"
}

// fallbackIfEmpty returns def if s is empty.
func fallbackIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ─── Auto-archive hook (called from message handler) ────────────

// archiveIncomingMessage is called from core/main.go's message handler
// to capture every incoming message into the archive.
// This makes .listmsg work retroactively AND enables group context awareness.
func archiveIncomingMessage(msg *events.Message) {
	if msg == nil || msg.Message == nil {
		return
	}
	// Pesan yang dihapus (REVOKE) tidak perlu diarsipkan ulang.
	if pm := msg.Message.GetProtocolMessage(); pm != nil &&
		pm.GetType() == waE2E.ProtocolMessage_REVOKE {
		return
	}
	m := archivedMessage{
		ID:        msg.Info.ID,
		GroupJID:  msg.Info.Chat.String(),
		Sender:    msg.Info.Sender.String(),
		PushName:  msg.Info.PushName,
		Timestamp: time.Now().Unix(),
	}
	m.RawEnvelope = captureEnvelope(msg)
	// Skip noise: senderKeyDistributionMessage murni = key exchange E2E tanpa konten.
	// Monitor run 2: ~200 blob/jam, tidak ada nilai analitis — jangan cemari arsip.
	if msg.Message.GetSenderKeyDistributionMessage() != nil &&
		msg.Message.GetConversation() == "" &&
		msg.Message.GetExtendedTextMessage() == nil &&
		msg.Message.GetImageMessage() == nil &&
		msg.Message.GetVideoMessage() == nil &&
		msg.Message.GetAudioMessage() == nil &&
		msg.Message.GetStickerMessage() == nil &&
		msg.Message.GetDocumentMessage() == nil {
		return
	}
	switch {
	case msg.Message.GetConversation() != "":
		m.Type = "text"
		m.Text = msg.Message.GetConversation()
	case msg.Message.GetExtendedTextMessage() != nil:
		m.Type = "text"
		m.Text = msg.Message.GetExtendedTextMessage().GetText()
	case msg.Message.GetImageMessage() != nil:
		m.Type = "image"
		m.Text = msg.Message.GetImageMessage().GetCaption()
	case msg.Message.GetVideoMessage() != nil:
		m.Type = "video"
		m.Text = msg.Message.GetVideoMessage().GetCaption()
	case msg.Message.GetAudioMessage() != nil:
		m.Type = "audio"
	case msg.Message.GetStickerMessage() != nil:
		m.Type = "sticker"
	case msg.Message.GetDocumentMessage() != nil:
		m.Type = "document"
		m.Text = msg.Message.GetDocumentMessage().GetFileName()
	case msg.Message.GetReactionMessage() != nil:
		// Reaksi emoji — engagement yang selama ini hilang sebagai "other"
		m.Type = "reaction"
		m.Text = msg.Message.GetReactionMessage().GetText()
	case msg.Message.GetButtonsResponseMessage() != nil:
		// User klik tombol
		m.Type = "buttons_response"
		m.Text = msg.Message.GetButtonsResponseMessage().GetSelectedButtonID()
	case msg.Message.GetTemplateButtonReplyMessage() != nil:
		// User klik tombol template (field 29) — ditemukan di monitor run 2
		m.Type = "button_reply"
		m.Text = msg.Message.GetTemplateButtonReplyMessage().GetSelectedDisplayText()
	case msg.Message.GetInteractiveResponseMessage() != nil:
		m.Type = "interactive_response"
	case msg.Message.GetInteractiveMessage() != nil:
		m.Type = "interactive"
		m.Text = msg.Message.GetInteractiveMessage().GetBody().GetText()
	case msg.Message.GetBotForwardedMessage() != nil:
		m.Type = "bot_forwarded"
	case getAnyPollMessage(msg.Message) != "":
		m.Type = "poll"
		m.Text = getAnyPollMessage(msg.Message)
	case msg.Message.GetAlbumMessage() != nil:
		m.Type = "album"
	default:
		m.Type = "other"
	}
	archiveMessage(m)
}

// getAnyPollMessage mengembalikan pertanyaan poll dari versi manapun (V1–V6).
func getAnyPollMessage(msg *waE2E.Message) string {
	// Catatan: waE2E diimpor di file ini? cek — archiveIncomingMessage ada di owner_extras.go
	// yang sudah mengimpor hypermeow types. Gunakan accessor generik via getter.
	if p := msg.GetPollCreationMessage(); p != nil {
		return p.GetName()
	}
	if p := msg.GetPollCreationMessageV2(); p != nil {
		return p.GetName()
	}
	if p := msg.GetPollCreationMessageV3(); p != nil {
		return p.GetName()
	}
	// V4 adalah FutureProofMessage (tidak ada GetName) — deteksi saja tanpa teks
	if msg.GetPollCreationMessageV4() != nil {
		return "(poll)"
	}
	if p := msg.GetPollCreationMessageV5(); p != nil {
		return p.GetName()
	}
	if p := msg.GetPollCreationMessageV6(); p != nil {
		return p.GetName()
	}
	return ""
}

// keep imports referenced — whatsmeow.Client is used as func param type above.
// sync.RWMutex is used by msgArchiveMu (declared above).

// ─── Group context (for AI situational awareness) ─────────────

// getGroupContext returns a compact summary of the current group's state:
// - last N messages (with sender names + text previews)
// - active participants (those who chatted in the last N messages)
// - dominant topic guess (most common words, very naive)
//
// Used by AI to "read the room" before replying. The AI is encouraged to
// call this tool whenever it needs to understand what's going on in the
// current chat before responding.
//
// Args: empty = use current chat, last 15 messages. If "N" (number), use last N.
func getGroupContext(client *whatsmeow.Client, msg *events.Message, args string) string {
	loadMsgArchive()

	limit := 15
	if args != "" {
		var n int
		if _, err := fmt.Sscanf(args, "%d", &n); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}

	chat := msg.Info.Chat.String()

	msgArchiveMu.RLock()
	defer msgArchiveMu.RUnlock()

	// Collect messages for this chat
	var matched []archivedMessage
	for _, m := range msgArchive {
		if m.GroupJID == chat {
			matched = append(matched, m)
		}
	}
	if len(matched) == 0 {
		return "📭 Grup belum punya riwayat percakapan"
	}

	// Take last N
	start := 0
	if len(matched) > limit {
		start = len(matched) - limit
	}
	recent := matched[start:]

	// Build active participants set (last N only)
	type participantInfo struct {
		Name   string
		Phone  string
		Count  int
		LastTS int64
	}
	participants := map[string]*participantInfo{}
	for _, m := range recent {
		key := m.Sender
		p, ok := participants[key]
		if !ok {
			p = &participantInfo{}
			participants[key] = p
			p.Phone = extractPhone(m.Sender)
			if m.PushName != "" {
				p.Name = m.PushName
			} else {
				p.Name = "+" + p.Phone
			}
		}
		p.Count++
		if m.Timestamp > p.LastTS {
			p.LastTS = m.Timestamp
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👥 *Group context* (last %d messages):\n\n", len(recent)))

	// Active participants (sorted by message count desc)
	var pList []*participantInfo
	for _, p := range participants {
		pList = append(pList, p)
	}
	// Simple sort: most active first
	for i := 0; i < len(pList); i++ {
		for j := i + 1; j < len(pList); j++ {
			if pList[j].Count > pList[i].Count {
				pList[i], pList[j] = pList[j], pList[i]
			}
		}
	}
	sb.WriteString("Active chatters:\n")
	for i, p := range pList {
		if i >= 5 {
			break
		}
		sb.WriteString(fmt.Sprintf("  • %s (+%s) — %d msg\n", p.Name, p.Phone, p.Count))
	}
	if len(pList) > 5 {
		sb.WriteString(fmt.Sprintf("  • ... %d others\n", len(pList)-5))
	}

	// Recent messages (compact preview)
	sb.WriteString("\nRecent messages:\n")
	for _, m := range recent {
		ts := time.Unix(m.Timestamp, 0).In(time.FixedZone("WIB", 7*3600))
		tsStr := ts.Format("15:04")
		name := m.PushName
		if name == "" {
			name = "+" + extractPhone(m.Sender)
		}
		preview := m.Text
		if preview == "" {
			preview = fmt.Sprintf("[%s]", strings.ToUpper(m.Type))
		}
		if len(preview) > 60 {
			preview = preview[:57] + "..."
		}
		preview = strings.ReplaceAll(preview, "\n", " ")
		sb.WriteString(fmt.Sprintf("  [%s] %s: %s\n", tsStr, name, preview))
	}

	return sb.String()
}

// getRecentActivity returns a compact summary of recent activity in the
// current group. Useful for AI to "feel" the group vibe without reading
// every message: who's active, how busy, last message time.
func getRecentActivity(msg *events.Message) string {
	loadMsgArchive()
	chat := msg.Info.Chat.String()

	msgArchiveMu.RLock()
	defer msgArchiveMu.RUnlock()

	// Last 1 hour window for "active" status
	now := time.Now().Unix()
	hourAgo := now - 3600
	dayAgo := now - 86400

	var lastHour, lastDay, allRecent []archivedMessage
	var lastMsg archivedMessage
	var hasLast bool

	for _, m := range msgArchive {
		if m.GroupJID != chat {
			continue
		}
		allRecent = append(allRecent, m)
		if m.Timestamp >= hourAgo {
			lastHour = append(lastHour, m)
		}
		if m.Timestamp >= dayAgo {
			lastDay = append(lastDay, m)
		}
		if !hasLast || m.Timestamp > lastMsg.Timestamp {
			lastMsg = m
			hasLast = true
		}
	}
	if !hasLast {
		return "📭 Grup belum punya aktivitas"
	}

	// Unique senders in last hour
	senders := map[string]int{}
	for _, m := range lastHour {
		key := m.PushName
		if key == "" {
			key = "+" + extractPhone(m.Sender)
		}
		senders[key]++
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 Recent activity:\n"))
	sb.WriteString(fmt.Sprintf("• Total archived: %d\n", len(allRecent)))
	sb.WriteString(fmt.Sprintf("• Last 24h: %d msgs\n", len(lastDay)))
	sb.WriteString(fmt.Sprintf("• Last 1h: %d msgs (%d unique senders)\n", len(lastHour), len(senders)))

	if len(senders) > 0 {
		sb.WriteString("• Active last hour:\n")
		type kv struct {
			Name  string
			Count int
		}
		var sorted []kv
		for n, c := range senders {
			sorted = append(sorted, kv{n, c})
		}
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].Count > sorted[i].Count {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		for i, s := range sorted {
			if i >= 3 {
				break
			}
			sb.WriteString(fmt.Sprintf("    %s (%d)\n", s.Name, s.Count))
		}
	}

	// Last message info
	ts := time.Unix(lastMsg.Timestamp, 0).In(time.FixedZone("WIB", 7*3600))
	name := lastMsg.PushName
	if name == "" {
		name = "+" + extractPhone(lastMsg.Sender)
	}
	sb.WriteString(fmt.Sprintf("• Last msg: %s at %s\n", name, ts.Format("15:04:05")))
	return sb.String()
}
