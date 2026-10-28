package main

// crm_envelope.go — Full message envelope capture for CRM/archive (v2)
// ════════════════════════════════════════════════════════════
//
// Captures EVERY available data from incoming WhatsApp messages:
//
//   • Message key (msg_id, chat, sender, from_me, participant, sender_user/server)
//   • Sender info (push_name, timestamp, is_group, broadcast)
//   • Addressing (lid vs pn, participant_pn for LID-aliased senders)
//   • Unwrapped message flags (IsEphemeral, IsViewOnce, IsViewOnceV2,
//     IsViewOnceV2Extension, IsDocumentWithCaption, IsLottieSticker,
//     IsBotInvoke, IsEdit)
//   • Retry + unavailable message info
//   • Message content (text, image, video, audio, sticker, document, contact,
//     location, reaction, poll, protocol, …) with full media metadata
//   • ContextInfo (quoted/replied-to message: id, participant, text, type)
//   • MessageContextInfo (messageSecret, base64)
//   • Synthetic raw XML node reconstructed from msg.Info
//     (tag + attrs: from, to, id, participant, t, notify, addressing_mode, …)
//   • Full protobuf messages as JSON:
//       - raw_message           = msg.Message (parsed/extracted, post-unwrap)
//       - raw_message_unwrapped = msg.RawMessage (raw, possibly still wrapped)
//       - source_webmsg         = msg.SourceWebMsg (history sync, if any)
//   • NewsletterMeta (if from a newsletter)
//
// Used by:  archiveIncomingMessage → archiveMessage → PG message_archive
// Displayed by: .insp, .q, .crm commands
//
// Storage: PG table message_archive (schema v2 — see pg.go pgInitAll).
// Cheap filterable fields are first-class columns; full structured data
// lives in JSONB blobs (envelope, raw_message, raw_message_unwrapped,
// source_webmsg, newsletter_meta, node_attrs, additional_attrs/nodes).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/encoding/protojson"
)

// FullEnvelope is the complete captured message data, stored as JSONB in PG.
// Field names match JSON keys stored in message_archive.envelope column.
type FullEnvelope struct {
	// ─── Identity / Key ───────────────────────────────────────────
	MsgID        string `json:"msg_id"`
	Chat         string `json:"chat"`
	Sender       string `json:"sender"`
	SenderUser   string `json:"sender_user"`
	SenderServer string `json:"sender_server"`
	FromMe       bool   `json:"from_me"`

	// ─── Sender Info ──────────────────────────────────────────────
	PushName  string `json:"push_name,omitempty"`
	Timestamp int64  `json:"timestamp"`
	IsGroup   bool   `json:"is_group"`

	// ─── Addressing ───────────────────────────────────────────────
	AddressingMode string `json:"addressing_mode,omitempty"` // "lid" or "pn"
	Participant    string `json:"participant,omitempty"`     // group participant JID
	ParticipantPN  string `json:"participant_pn,omitempty"`  // phone number when LID

	// ─── Message Flags (unwrapped status from events.Message) ─────
	MsgFlags struct {
		IsEphemeral           bool `json:"is_ephemeral"`
		IsViewOnce            bool `json:"is_view_once"`
		IsViewOnceV2          bool `json:"is_view_once_v2"`
		IsViewOnceV2Extension bool `json:"is_view_once_v2_ext"`
		IsDocumentWithCaption bool `json:"is_document_with_caption"`
		IsLottieSticker       bool `json:"is_lottie_sticker"`
		IsBotInvoke           bool `json:"is_bot_invoke"`
		IsEdit                bool `json:"is_edit"`
	} `json:"msg_flags"`

	// ─── Retry / unavailable message ─────────────────────────────
	RetryCount           int    `json:"retry_count,omitempty"`
	UnavailableRequestID string `json:"unavailable_request_id,omitempty"`

	// ─── Newsletter metadata (if from a broadcast/newsletter) ────
	NewsletterMeta *events.NewsletterMessageMeta `json:"newsletter_meta,omitempty"`

	// ─── Broadcast detection ──────────────────────────────────────
	Broadcast bool `json:"broadcast,omitempty"` // chat server == "broadcast"

	// ─── Message Content ─────────────────────────────────────────
	MessageType string `json:"message_type"`
	Text        string `json:"text,omitempty"`
	Caption     string `json:"caption,omitempty"`
	MediaURL    string `json:"media_url,omitempty"`
	MediaMime   string `json:"media_mime,omitempty"`
	MediaKey    string `json:"media_key,omitempty"` // base64
	FileLength  uint64 `json:"file_length,omitempty"`

	// ─── ContextInfo (reply/quoted) ──────────────────────────────
	QuotedMsgID       string `json:"quoted_msg_id,omitempty"`
	QuotedParticipant string `json:"quoted_participant,omitempty"`
	QuotedMsgText     string `json:"quoted_msg_text,omitempty"`
	QuotedMsgType     string `json:"quoted_msg_type,omitempty"`

	// ─── MessageContextInfo ───────────────────────────────────────
	MessageSecret string `json:"message_secret,omitempty"` // base64

	// ─── Synthetic raw XML node (reconstructed from msg.Info) ────
	// This mirrors what the original waBinary.Node looked like over the wire.
	NodeTag       string            `json:"node_tag,omitempty"`        // "message"
	NodeAttrs     map[string]string `json:"node_attrs,omitempty"`      // from,to,id,participant,t,notify,...
	NodeAttrsList []NodeAttr        `json:"node_attrs_list,omitempty"` // ordered attrs (for display)

	// ─── Full protobuf messages as JSON ───────────────────────────
	RawMessage          json.RawMessage `json:"raw_message,omitempty"`           // msg.Message (parsed/extracted)
	RawMessageUnwrapped json.RawMessage `json:"raw_message_unwrapped,omitempty"` // msg.RawMessage (unmodified)
	SourceWebMsg        json.RawMessage `json:"source_webmsg,omitempty"`         // msg.SourceWebMsg (history sync)
}

// NodeAttr is one ordered (key, value) pair from the synthetic raw node attrs.
// Used for display only (since map iteration order is random in Go).
type NodeAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// captureEnvelope extracts ALL available data from a WhatsApp message event.
// Returns JSON bytes ready to store as JSONB in PostgreSQL message_archive.envelope.
//
// This is the single source of truth for "what we know about this message".
// All downstream display commands (.insp, .q, .crm) read this envelope.
func captureEnvelope(msg *events.Message) []byte {
	if msg == nil || msg.Message == nil {
		return nil
	}

	env := FullEnvelope{
		// Key
		MsgID:        msg.Info.ID,
		Chat:         msg.Info.Chat.String(),
		Sender:       msg.Info.Sender.String(),
		SenderUser:   msg.Info.Sender.User,
		SenderServer: msg.Info.Sender.Server,
		FromMe:       msg.Info.IsFromMe,

		// Info
		PushName:  msg.Info.PushName,
		Timestamp: msg.Info.Timestamp.Unix(),
		IsGroup:   msg.Info.IsGroup,
	}

	// Detect addressing mode (LID vs PN)
	if msg.Info.Sender.Server == "lid" {
		env.AddressingMode = "lid"
	} else if msg.Info.Sender.Server == "s.whatsapp.net" {
		env.AddressingMode = "pn"
	}

	// Broadcast detection — chat server "broadcast" means this came from a WA broadcast list
	if msg.Info.Chat.Server == types.BroadcastServer {
		env.Broadcast = true
	}

	// Participant in group
	if msg.Info.IsGroup {
		if !msg.Info.SenderAlt.IsEmpty() {
			env.Participant = msg.Info.SenderAlt.String()
		} else {
			env.Participant = msg.Info.Sender.String()
		}
	}

	// Message flags (from events.Message struct)
	env.MsgFlags.IsEphemeral = msg.IsEphemeral
	env.MsgFlags.IsViewOnce = msg.IsViewOnce
	env.MsgFlags.IsViewOnceV2 = msg.IsViewOnceV2
	env.MsgFlags.IsViewOnceV2Extension = msg.IsViewOnceV2Extension
	env.MsgFlags.IsDocumentWithCaption = msg.IsDocumentWithCaption
	env.MsgFlags.IsLottieSticker = msg.IsLottieSticker
	env.MsgFlags.IsBotInvoke = msg.IsBotInvoke
	env.MsgFlags.IsEdit = msg.IsEdit

	// Retry + unavailable
	env.RetryCount = msg.RetryCount
	if msg.UnavailableRequestID != "" {
		env.UnavailableRequestID = msg.UnavailableRequestID
	}

	// Newsletter meta
	if msg.NewsletterMeta != nil {
		env.NewsletterMeta = msg.NewsletterMeta
	}

	// Build synthetic raw XML node attrs (reconstruct what came over the wire)
	env.NodeTag = "message"
	attrs := map[string]string{
		"from": env.Sender,
		"to":   env.Chat,
		"id":   env.MsgID,
		"t":    fmt.Sprintf("%d", env.Timestamp),
	}
	if env.PushName != "" {
		attrs["notify"] = env.PushName
	}
	if env.IsGroup {
		attrs["participant"] = env.Participant
	}
	if env.AddressingMode != "" {
		attrs["addressing_mode"] = env.AddressingMode
	}
	if env.ParticipantPN != "" {
		attrs["participant_pn"] = env.ParticipantPN
	}
	if env.FromMe {
		attrs["from_me"] = "true"
	}
	if env.Broadcast {
		attrs["broadcast"] = "true"
	}
	env.NodeAttrs = attrs
	// Ordered list for display
	env.NodeAttrsList = []NodeAttr{
		{"from", env.Sender},
		{"to", env.Chat},
		{"id", env.MsgID},
		{"t", fmt.Sprintf("%d", env.Timestamp)},
	}
	if env.PushName != "" {
		env.NodeAttrsList = append(env.NodeAttrsList, NodeAttr{"notify", env.PushName})
	}
	if env.IsGroup {
		env.NodeAttrsList = append(env.NodeAttrsList, NodeAttr{"participant", env.Participant})
	}
	if env.AddressingMode != "" {
		env.NodeAttrsList = append(env.NodeAttrsList, NodeAttr{"addressing_mode", env.AddressingMode})
	}
	if env.ParticipantPN != "" {
		env.NodeAttrsList = append(env.NodeAttrsList, NodeAttr{"participant_pn", env.ParticipantPN})
	}
	if env.FromMe {
		env.NodeAttrsList = append(env.NodeAttrsList, NodeAttr{"from_me", "true"})
	}
	if env.Broadcast {
		env.NodeAttrsList = append(env.NodeAttrsList, NodeAttr{"broadcast", "true"})
	}

	// Extract message type + content
	m := msg.Message
	switch {
	case m.GetConversation() != "":
		env.MessageType = "text"
		env.Text = m.GetConversation()

	case m.GetExtendedTextMessage() != nil:
		env.MessageType = "text"
		etm := m.GetExtendedTextMessage()
		env.Text = etm.GetText()
		// Extract ContextInfo (quoted message)
		if ci := etm.GetContextInfo(); ci != nil {
			env.QuotedMsgID = ci.GetStanzaID()
			env.QuotedParticipant = ci.GetParticipant()
			if qm := ci.GetQuotedMessage(); qm != nil {
				if qm.GetConversation() != "" {
					env.QuotedMsgText = qm.GetConversation()
					env.QuotedMsgType = "text"
				} else if qm.GetExtendedTextMessage() != nil {
					env.QuotedMsgText = qm.GetExtendedTextMessage().GetText()
					env.QuotedMsgType = "text"
				} else if qm.GetImageMessage() != nil {
					env.QuotedMsgType = "image"
					env.QuotedMsgText = qm.GetImageMessage().GetCaption()
				} else if qm.GetVideoMessage() != nil {
					env.QuotedMsgType = "video"
					env.QuotedMsgText = qm.GetVideoMessage().GetCaption()
				} else if qm.GetAudioMessage() != nil {
					env.QuotedMsgType = "audio"
				} else if qm.GetDocumentMessage() != nil {
					env.QuotedMsgType = "document"
					env.QuotedMsgText = qm.GetDocumentMessage().GetFileName()
				} else if qm.GetStickerMessage() != nil {
					env.QuotedMsgType = "sticker"
				}
			}
		}

	case m.GetImageMessage() != nil:
		env.MessageType = "image"
		im := m.GetImageMessage()
		env.Caption = im.GetCaption()
		env.MediaURL = im.GetURL()
		env.MediaMime = im.GetMimetype()
		env.FileLength = im.GetFileLength()
		if mk := im.GetMediaKey(); len(mk) > 0 {
			env.MediaKey = base64.StdEncoding.EncodeToString(mk)
		}

	case m.GetVideoMessage() != nil:
		env.MessageType = "video"
		vm := m.GetVideoMessage()
		env.Caption = vm.GetCaption()
		env.MediaURL = vm.GetURL()
		env.MediaMime = vm.GetMimetype()
		env.FileLength = vm.GetFileLength()
		if mk := vm.GetMediaKey(); len(mk) > 0 {
			env.MediaKey = base64.StdEncoding.EncodeToString(mk)
		}

	case m.GetAudioMessage() != nil:
		env.MessageType = "audio"
		am := m.GetAudioMessage()
		env.MediaURL = am.GetURL()
		env.MediaMime = am.GetMimetype()
		env.FileLength = am.GetFileLength()
		if mk := am.GetMediaKey(); len(mk) > 0 {
			env.MediaKey = base64.StdEncoding.EncodeToString(mk)
		}

	case m.GetStickerMessage() != nil:
		env.MessageType = "sticker"
		sm := m.GetStickerMessage()
		env.MediaURL = sm.GetURL()
		env.MediaMime = sm.GetMimetype()
		env.FileLength = sm.GetFileLength()
		if mk := sm.GetMediaKey(); len(mk) > 0 {
			env.MediaKey = base64.StdEncoding.EncodeToString(mk)
		}

	case m.GetDocumentMessage() != nil:
		env.MessageType = "document"
		dm := m.GetDocumentMessage()
		env.Caption = dm.GetFileName()
		env.MediaURL = dm.GetURL()
		env.MediaMime = dm.GetMimetype()
		env.FileLength = dm.GetFileLength()
		if mk := dm.GetMediaKey(); len(mk) > 0 {
			env.MediaKey = base64.StdEncoding.EncodeToString(mk)
		}

	case m.GetContactMessage() != nil:
		env.MessageType = "contact"
		env.Text = m.GetContactMessage().GetDisplayName()

	case m.GetContactsArrayMessage() != nil:
		env.MessageType = "contacts"

	case m.GetLocationMessage() != nil:
		env.MessageType = "location"
		lm := m.GetLocationMessage()
		env.Text = fmt.Sprintf("lat=%f lon=%f", lm.GetDegreesLatitude(), lm.GetDegreesLongitude())

	case m.GetLiveLocationMessage() != nil:
		env.MessageType = "live_location"

	case m.GetReactionMessage() != nil:
		env.MessageType = "reaction"
		rm := m.GetReactionMessage()
		env.Text = rm.GetText()
		if rm.GetKey() != nil {
			env.QuotedMsgID = rm.GetKey().GetID()
		}

	case m.GetPollCreationMessage() != nil:
		env.MessageType = "poll"
		env.Text = m.GetPollCreationMessage().GetName()
	case m.GetPollCreationMessageV2() != nil:
		env.MessageType = "poll"
		env.Text = m.GetPollCreationMessageV2().GetName()
	case m.GetPollCreationMessageV3() != nil:
		env.MessageType = "poll"
		env.Text = m.GetPollCreationMessageV3().GetName()
	case m.GetPollCreationMessageV5() != nil:
		env.MessageType = "poll"
		env.Text = m.GetPollCreationMessageV5().GetName()
	case m.GetPollCreationMessageV6() != nil:
		env.MessageType = "poll"
		env.Text = m.GetPollCreationMessageV6().GetName()
	case m.GetPollCreationMessageV4() != nil:
		env.MessageType = "poll"
		env.Text = "(poll)"

	case m.GetPollUpdateMessage() != nil:
		env.MessageType = "poll_update"

	case m.GetProtocolMessage() != nil:
		env.MessageType = "protocol"
		pm := m.GetProtocolMessage()
		env.Text = fmt.Sprintf("type=%d", pm.GetType())
		if pm.GetKey() != nil {
			env.QuotedMsgID = pm.GetKey().GetID()
		}

	case m.GetButtonsResponseMessage() != nil:
		env.MessageType = "buttons_response"
		env.Text = m.GetButtonsResponseMessage().GetSelectedButtonID()
	case m.GetTemplateButtonReplyMessage() != nil:
		env.MessageType = "button_reply"
		env.Text = m.GetTemplateButtonReplyMessage().GetSelectedDisplayText()
	case m.GetInteractiveResponseMessage() != nil:
		env.MessageType = "interactive_response"
	case m.GetInteractiveMessage() != nil:
		env.MessageType = "interactive"
		env.Text = m.GetInteractiveMessage().GetBody().GetText()
	case m.GetBotForwardedMessage() != nil:
		env.MessageType = "bot_forwarded"
	case m.GetAlbumMessage() != nil:
		env.MessageType = "album"

	default:
		env.MessageType = detectMessageType(m)
	}

	// Extract MessageContextInfo (messageSecret)
	if mci := m.GetMessageContextInfo(); mci != nil {
		if len(mci.GetMessageSecret()) > 0 {
			env.MessageSecret = base64.StdEncoding.EncodeToString(mci.GetMessageSecret())
		}
	}

	// Capture full raw protobuf message as JSON (parsed/extracted)
	if rawBytes, err := protojson.Marshal(m); err == nil {
		env.RawMessage = json.RawMessage(rawBytes)
	}

	// Capture raw unmodified waE2E.Message (may still be wrapped in
	// DeviceSentMessage/EphemeralMessage/ViewOnceMessage)
	if msg.RawMessage != nil && msg.RawMessage != m {
		if rawUnwrapped, err := protojson.Marshal(msg.RawMessage); err == nil {
			env.RawMessageUnwrapped = json.RawMessage(rawUnwrapped)
		}
	}

	// Capture SourceWebMsg (history sync / unavailable message request)
	if msg.SourceWebMsg != nil {
		if swmBytes, err := protojson.Marshal(msg.SourceWebMsg); err == nil {
			env.SourceWebMsg = json.RawMessage(swmBytes)
		}
	}

	data, err := json.Marshal(env)
	if err != nil {
		return []byte(`{"error":"marshal failed"}`)
	}
	return data
}

// detectMessageType tries to detect message type from available getters.
// Fallback for message types not explicitly handled in captureEnvelope.
func detectMessageType(m *waE2E.Message) string {
	if m == nil {
		return "other"
	}
	// Order matters: more specific first.
	switch {
	case m.GetSenderKeyDistributionMessage() != nil:
		return "sender_key_distribution"
	case m.GetDeviceSentMessage() != nil:
		return "device_sent"
	case m.GetEphemeralMessage() != nil:
		return "ephemeral"
	case m.GetViewOnceMessage() != nil:
		return "view_once"
	case m.GetViewOnceMessageV2() != nil:
		return "view_once_v2"
	case m.GetViewOnceMessageV2Extension() != nil:
		return "view_once_v2_ext"
	case m.GetDocumentWithCaptionMessage() != nil:
		return "document_with_caption"
	case m.GetLottieStickerMessage() != nil:
		return "lottie_sticker"
	case m.GetBotInvokeMessage() != nil:
		return "bot_invoke"
	case m.GetEditedMessage() != nil:
		return "edited"
	case m.GetGroupInviteMessage() != nil:
		return "group_invite"
	case m.GetNewsletterAdminInviteMessage() != nil:
		return "newsletter_admin_invite"
	case m.GetProductMessage() != nil:
		return "product"
	case m.GetOrderMessage() != nil:
		return "order"
	case m.GetInvoiceMessage() != nil:
		return "invoice"
	case m.GetListMessage() != nil:
		return "list"
	case m.GetButtonsMessage() != nil:
		return "buttons"
	case m.GetTemplateMessage() != nil:
		return "template"
	case m.GetGroupMentionedMessage() != nil:
		return "group_mentioned"
	}
	return "other"
}

// ─── PG helpers (used by display commands) ──────────────────────

// queryEnvelopeFromPG retrieves the envelope JSONB for a given message ID.
// Used by .insp / .q / .crm to display full message data.
func queryEnvelopeFromPG(ctx context.Context, msgID string) (*FullEnvelope, error) {
	db, err := pgGet()
	if err != nil {
		return nil, fmt.Errorf("pg: %w", err)
	}
	var rawJSON string
	err = pgQueryRow(ctx, db,
		`SELECT envelope::text FROM message_archive WHERE msg_id = ? ORDER BY id DESC LIMIT 1`,
		msgID).Scan(&rawJSON)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	if rawJSON == "" || rawJSON == "null" || rawJSON == "{}" {
		return nil, fmt.Errorf("no envelope data")
	}
	var env FullEnvelope
	if err := json.Unmarshal([]byte(rawJSON), &env); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &env, nil
}

// queryAllFieldsFromPG returns a map of all columns for a message,
// used when .insp -raw wants the FULL row (including JSONB blobs).
type archiveRow struct {
	MsgID                 string
	GroupJID              string
	Sender                string
	SenderUser            string
	SenderServer          string
	PushName              string
	FromMe                bool
	Timestamp             int64
	IsGroup               bool
	AddressingMode        string
	Participant           string
	ParticipantPN         string
	IsEphemeral           bool
	IsViewOnce            bool
	IsViewOnceV2          bool
	IsViewOnceV2Ext       bool
	IsDocumentWithCaption bool
	IsLottieSticker       bool
	IsBotInvoke           bool
	IsEdit                bool
	Broadcast             bool
	RetryCount            int
	UnavailableRequestID  string
	Type                  string
	Text                  string
	Caption               string
	MediaPath             string
	MediaURL              string
	MediaMime             string
	MediaKey              string
	FileLength            int64
	QuotedMsgID           string
	QuotedParticipant     string
	QuotedMsgText         string
	QuotedMsgType         string
	MessageSecret         string
	NodeTag               string
	NodeAttrs             string // JSON text
	Envelope              string // JSON text
	RawMessage            string // JSON text
	RawMessageUnwrapped   string // JSON text
	SourceWebMsg          string // JSON text
	NewsletterMeta        string // JSON text
	AdditionalAttrs       string // JSON text
	AdditionalNodes       string // JSON text
	CreatedAt             time.Time
}

// queryArchiveRowByID fetches a full row from message_archive by msg_id (or rowid via #).
func queryArchiveRowByID(ctx context.Context, msgID, chat string) (*archiveRow, error) {
	db, err := pgGet()
	if err != nil {
		return nil, err
	}
	q := `SELECT msg_id, group_jid, sender, sender_user, sender_server, push_name,
                from_me, timestamp, is_group, addressing_mode, participant, participant_pn,
                is_ephemeral, is_view_once, is_view_once_v2, is_view_once_v2_ext,
                is_document_with_caption, is_lottie_sticker, is_bot_invoke, is_edit,
                broadcast, retry_count, unavailable_request_id,
                type, text, caption, media_path, media_url, media_mime, media_key, file_length,
                quoted_msg_id, quoted_participant, quoted_msg_text, quoted_msg_type,
                message_secret, node_tag,
                COALESCE(node_attrs::text,'{}'),
                COALESCE(envelope::text,'{}'),
                COALESCE(raw_message::text,'{}'),
                COALESCE(raw_message_unwrapped::text,'{}'),
                COALESCE(source_webmsg::text,'{}'),
                COALESCE(newsletter_meta::text,'{}'),
                COALESCE(additional_attrs::text,'{}'),
                COALESCE(additional_nodes::text,'{}'),
                created_at
        FROM message_archive`
	args := []any{}
	if chat != "" {
		q += " WHERE msg_id = ? AND group_jid = ? ORDER BY id DESC LIMIT 1"
		args = append(args, msgID, chat)
	} else {
		q += " WHERE msg_id = ? ORDER BY id DESC LIMIT 1"
		args = append(args, msgID)
	}
	var r archiveRow
	err = pgQueryRow(ctx, db, q, args...).Scan(
		&r.MsgID, &r.GroupJID, &r.Sender, &r.SenderUser, &r.SenderServer, &r.PushName,
		&r.FromMe, &r.Timestamp, &r.IsGroup, &r.AddressingMode, &r.Participant, &r.ParticipantPN,
		&r.IsEphemeral, &r.IsViewOnce, &r.IsViewOnceV2, &r.IsViewOnceV2Ext,
		&r.IsDocumentWithCaption, &r.IsLottieSticker, &r.IsBotInvoke, &r.IsEdit,
		&r.Broadcast, &r.RetryCount, &r.UnavailableRequestID,
		&r.Type, &r.Text, &r.Caption, &r.MediaPath, &r.MediaURL, &r.MediaMime, &r.MediaKey, &r.FileLength,
		&r.QuotedMsgID, &r.QuotedParticipant, &r.QuotedMsgText, &r.QuotedMsgType,
		&r.MessageSecret, &r.NodeTag,
		&r.NodeAttrs, &r.Envelope, &r.RawMessage, &r.RawMessageUnwrapped,
		&r.SourceWebMsg, &r.NewsletterMeta, &r.AdditionalAttrs, &r.AdditionalNodes,
		&r.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ─── Display formatters ──────────────────────────────────────────

// formatEnvelopeForDisplay creates a human-readable summary of the envelope.
// Used by .insp / .q commands.
func formatEnvelopeForDisplay(env *FullEnvelope) string {
	if env == nil {
		return "❌ No envelope data"
	}
	var b strings.Builder
	b.WriteString("📋 *Message Envelope*\n\n")
	b.WriteString(fmt.Sprintf("🆔 ID: `%s`\n", env.MsgID))
	b.WriteString(fmt.Sprintf("💬 Chat: `%s`\n", env.Chat))
	b.WriteString(fmt.Sprintf("👤 Sender: `%s`\n", env.Sender))
	if env.SenderUser != "" {
		b.WriteString(fmt.Sprintf("📡 Sender User: `%s`\n", env.SenderUser))
	}
	if env.SenderServer != "" {
		b.WriteString(fmt.Sprintf("📡 Sender Server: `%s`\n", env.SenderServer))
	}
	b.WriteString(fmt.Sprintf("📤 From Me: %v\n", env.FromMe))
	if env.PushName != "" {
		b.WriteString(fmt.Sprintf("📛 Push Name: %s\n", env.PushName))
	}
	b.WriteString(fmt.Sprintf("⏰ Timestamp: %s\n", time.Unix(env.Timestamp, 0).Format("02-01-2006 15:04:05")))
	b.WriteString(fmt.Sprintf("👥 Is Group: %v\n", env.IsGroup))
	b.WriteString(fmt.Sprintf("🔤 Message Type: `%s`\n", env.MessageType))
	if env.AddressingMode != "" {
		b.WriteString(fmt.Sprintf("🔗 Addressing Mode: `%s`\n", env.AddressingMode))
	}
	if env.ParticipantPN != "" {
		b.WriteString(fmt.Sprintf("📞 Participant PN: `%s`\n", env.ParticipantPN))
	}
	if env.Participant != "" {
		b.WriteString(fmt.Sprintf("👥 Participant: `%s`\n", env.Participant))
	}
	if env.Broadcast {
		b.WriteString("📡 Broadcast: yes\n")
	}
	if env.RetryCount > 0 {
		b.WriteString(fmt.Sprintf("🔁 Retry Count: %d\n", env.RetryCount))
	}
	if env.UnavailableRequestID != "" {
		b.WriteString(fmt.Sprintf("⚠️ Unavailable Request ID: `%s`\n", env.UnavailableRequestID))
	}

	// Message flags
	var flags []string
	if env.MsgFlags.IsEphemeral {
		flags = append(flags, "ephemeral")
	}
	if env.MsgFlags.IsViewOnce {
		flags = append(flags, "view_once")
	}
	if env.MsgFlags.IsViewOnceV2 {
		flags = append(flags, "view_once_v2")
	}
	if env.MsgFlags.IsViewOnceV2Extension {
		flags = append(flags, "view_once_v2_ext")
	}
	if env.MsgFlags.IsDocumentWithCaption {
		flags = append(flags, "doc_with_caption")
	}
	if env.MsgFlags.IsLottieSticker {
		flags = append(flags, "lottie_sticker")
	}
	if env.MsgFlags.IsBotInvoke {
		flags = append(flags, "bot_invoke")
	}
	if env.MsgFlags.IsEdit {
		flags = append(flags, "edit")
	}
	if len(flags) > 0 {
		b.WriteString(fmt.Sprintf("🏷️ Flags: %s\n", strings.Join(flags, ", ")))
	}

	if env.Text != "" {
		text := env.Text
		if len(text) > 200 {
			text = text[:200] + "…"
		}
		b.WriteString(fmt.Sprintf("📝 Text: %s\n", text))
	}
	if env.Caption != "" {
		b.WriteString(fmt.Sprintf("📝 Caption: %s\n", env.Caption))
	}
	if env.MediaURL != "" {
		b.WriteString(fmt.Sprintf("📎 Media URL: `%s`\n", env.MediaURL))
	}
	if env.MediaMime != "" {
		b.WriteString(fmt.Sprintf("📎 Media MIME: `%s`\n", env.MediaMime))
	}
	if env.FileLength > 0 {
		b.WriteString(fmt.Sprintf("📦 File Size: %d bytes\n", env.FileLength))
	}
	if env.MediaKey != "" {
		k := env.MediaKey
		if len(k) > 40 {
			k = k[:37] + "…"
		}
		b.WriteString(fmt.Sprintf("🔑 Media Key: `%s`\n", k))
	}

	if env.QuotedMsgID != "" {
		b.WriteString("\n💬 *Quoted Message:*\n")
		b.WriteString(fmt.Sprintf("🆔 Quoted ID: `%s`\n", env.QuotedMsgID))
		if env.QuotedParticipant != "" {
			b.WriteString(fmt.Sprintf("👤 Quoted Participant: `%s`\n", env.QuotedParticipant))
		}
		if env.QuotedMsgText != "" {
			text := env.QuotedMsgText
			if len(text) > 200 {
				text = text[:200] + "…"
			}
			b.WriteString(fmt.Sprintf("📝 Quoted Text: %s\n", text))
		}
		if env.QuotedMsgType != "" {
			b.WriteString(fmt.Sprintf("🔤 Quoted Type: `%s`\n", env.QuotedMsgType))
		}
	}

	if env.MessageSecret != "" {
		s := env.MessageSecret
		if len(s) > 40 {
			s = s[:37] + "…"
		}
		b.WriteString(fmt.Sprintf("\n🔐 Message Secret: `%s`\n", s))
	}

	// Synthetic raw node
	if env.NodeTag != "" {
		b.WriteString("\n📦 *Synthetic Raw Node:*\n")
		b.WriteString(fmt.Sprintf("Tag: `%s`\n", env.NodeTag))
		if len(env.NodeAttrsList) > 0 {
			b.WriteString("Attrs:\n")
			for _, a := range env.NodeAttrsList {
				v := a.Value
				if len(v) > 60 {
					v = v[:57] + "…"
				}
				b.WriteString(fmt.Sprintf("  `%s` = `%s`\n", a.Key, v))
			}
		}
	}

	// Raw protobuf fields summary
	if len(env.RawMessage) > 0 {
		var raw map[string]any
		if json.Unmarshal(env.RawMessage, &raw) == nil {
			b.WriteString(fmt.Sprintf("\n📦 Raw Proto Fields: %d\n", len(raw)))
			var fields []string
			for k := range raw {
				fields = append(fields, k)
			}
			if len(fields) > 0 {
				// Sort fields alphabetically for stable display
				sortStrings(fields)
				preview := strings.Join(fields, "`, `")
				if len(preview) > 200 {
					preview = preview[:197] + "…"
				}
				b.WriteString(fmt.Sprintf("📋 Fields: `%s`\n", preview))
			}
		}
	}

	// Raw unwrapped (may differ from RawMessage if message was wrapped)
	if len(env.RawMessageUnwrapped) > 0 {
		var raw map[string]any
		if json.Unmarshal(env.RawMessageUnwrapped, &raw) == nil {
			b.WriteString(fmt.Sprintf("\n📦 Raw Unwrapped Fields: %d\n", len(raw)))
		}
	}

	if len(env.SourceWebMsg) > 0 {
		b.WriteString("\n📜 Source WebMsg: present (history sync)\n")
	}

	if env.NewsletterMeta != nil {
		b.WriteString("\n📰 Newsletter Meta: present\n")
	}

	return b.String()
}

// sortStrings sorts a slice of strings in place, alphabetically.
// Tiny helper to avoid importing sort just for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
