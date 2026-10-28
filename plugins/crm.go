package main

// crm.go — CRM (Capture/Relay/Modify) — Go port of Fiora CRM by Nixel
// ════════════════════════════════════════════════════════════
//
// Port of lib/crmstore.js + plugins/owner-crm.js to Go.
// Uses data/crmdb.db (SQLite, WAL mode) — same schema as JS version.
//
// Commands:
//   .crm [reply] [#rowid] [-snip] [-raw] [-file]  → Generate relay code
//   .insp [reply] [#rowid] [-raw] [-file]          → Inspect raw payload
//   .relay [reply] [#rowid]                        → Relay message
//   .lastchat [N]                                  → Last N messages
//   .crmstats                                      → CRM database stats
//
// Original: https://whatsapp.com/channel/0029VbCV1ck8fewpdNb2TY2k
// Ported to Go for Bot MD.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

const fioraCRMDBPath = "data/crmdb.db"

var (
	fioraCRMDBMu   sync.Mutex
	fioraCRMDBConn *sql.DB
	fioraCRMDBOnce sync.Once

	fioraCRMDBPathOnce sync.Once
	fioraCRMDBPathAbs  string
)

// fioraCRMDBPathResolved mengembalikan path ABSOLUT ke DB CRM.
// Dibuat absolut sekali via filepath.Abs + sync.Once (pola spr_senderkey.go)
// agar tidak tergantung CWD pada saat dipanggil — path relatif bisa membuka
// file DB yang salah (kosong) jika CWD proses berubah.
func fioraCRMDBPathResolved() string {
	fioraCRMDBPathOnce.Do(func() {
		p := fioraCRMDBPath
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		fioraCRMDBPathAbs = p
	})
	return fioraCRMDBPathAbs
}

// fioraCRMDBGet opens shared SQLite connection to data/crmdb.db
func fioraCRMDBGet() (*sql.DB, error) {
	fioraCRMDBMu.Lock()
	defer fioraCRMDBMu.Unlock()
	if fioraCRMDBConn != nil {
		return fioraCRMDBConn, nil
	}
	dbPath := fioraCRMDBPathResolved()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir crmdb dir: %w", err)
	}
	db, err := pgGet()
	if err != nil {
		return nil, fmt.Errorf("open crmdb: %w", err)
	}
	fioraCRMDBConn = db
	return db, nil
}

// fioraCRMDBInit creates tables if they don't exist
func fioraCRMDBInit() {
	// Tables now created by pgInitAll() — no-op
}

// fioraCRMSaveMessage saves a WhatsApp message to CRM DB
func fioraCRMSaveMessage(client *whatsmeow.Client, msg *events.Message, chat string) bool {
	if msg == nil || msg.Message == nil || chat == "" {
		return false
	}
	id := msg.Info.ID
	if id == "" {
		return false
	}
	// Encode message to protobuf bytes
	encoded, err := proto.Marshal(msg.Message)
	if err != nil {
		return false
	}
	// Resolve sender phone (handle LID → PN conversion)
	sender := ""
	if !msg.Info.Sender.IsEmpty() {
		sender = resolvePhone(client, msg.Info.Sender)
	}
	ts := msg.Info.Timestamp.Unix()
	if ts == 0 {
		ts = time.Now().Unix()
	}

	db, err := fioraCRMDBGet()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pgExec(ctx, db,
		`INSERT INTO messages (id, chat, sender, timestamp, data) VALUES (?, ?, ?, ?, ?)`,
		id, chat, sender, ts, encoded)
	return err == nil
}

// fioraCRMLoadMessage loads a message by ID + chat from CRM DB
func fioraCRMLoadMessage(id, chat string) *waE2E.Message {
	db, err := fioraCRMDBGet()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var data []byte
	var row sql.Row
	if chat != "" {
		row = *pgQueryRow(ctx, db,
			`SELECT data FROM messages WHERE id = ? AND chat = ? ORDER BY rowid DESC LIMIT 1`, id, chat)
	} else {
		row = *pgQueryRow(ctx, db,
			`SELECT data FROM messages WHERE id = ? ORDER BY rowid DESC LIMIT 1`, id)
	}
	if err := row.Scan(&data); err != nil {
		return nil
	}
	msg := &waE2E.Message{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil
	}
	return msg
}

// fioraCRMHasMessage checks if a message exists
func fioraCRMHasMessage(id, chat string) bool {
	db, err := fioraCRMDBGet()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var one int
	if chat != "" {
		err = pgQueryRow(ctx, db, `SELECT 1 FROM messages WHERE id = ? AND chat = ? LIMIT 1`, id, chat).Scan(&one)
	} else {
		err = pgQueryRow(ctx, db, `SELECT 1 FROM messages WHERE id = ? LIMIT 1`, id).Scan(&one)
	}
	return err == nil
}

// fioraCRMDeleteMessage deletes a message by ID + chat
func fioraCRMDeleteMessage(id, chat string) bool {
	db, err := fioraCRMDBGet()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if chat != "" {
		_, err = pgExec(ctx, db, `DELETE FROM messages WHERE id = ? AND chat = ?`, id, chat)
	} else {
		_, err = pgExec(ctx, db, `DELETE FROM messages WHERE id = ?`, id)
	}
	return err == nil
}

// fioraCRMGetMessages returns last N messages for a chat
func fioraCRMGetMessages(chat string, limit int) []fioraCRMMessageRow {
	db, err := fioraCRMDBGet()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if limit <= 0 {
		limit = 50
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := pgQuery(ctx, db,
		`SELECT rowid, id, chat, sender, timestamp, data FROM messages WHERE chat = ? ORDER BY rowid DESC LIMIT ?`,
		chat, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []fioraCRMMessageRow
	for rows.Next() {
		var r fioraCRMMessageRow
		var sender sql.NullString
		var ts sql.NullInt64
		if err := rows.Scan(&r.RowID, &r.ID, &r.Chat, &sender, &ts, &r.Data); err != nil {
			continue
		}
		r.Sender = sender.String
		r.Timestamp = ts.Int64
		result = append(result, r)
	}
	return result
}

// fioraCRMCountMessages returns total message count
func fioraCRMCountMessages() (int, error) {
	db, err := fioraCRMDBGet()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := pgQueryRow(ctx, db, `SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count messages: %w", err)
	}
	return count, nil
}

// fioraCRMGetStats returns database stats
func fioraCRMGetStats() string {
	db, err := fioraCRMDBGet()
	if err != nil {
		return "❌ CRM DB error"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var msgCount, nodeCount int
	if err := pgQueryRow(ctx, db, `SELECT COUNT(*) FROM messages`).Scan(&msgCount); err != nil {
		fmt.Printf("[CRM] stats count messages error: %v\n", err)
		return fmt.Sprintf("❌ CRM DB error (count messages): %v", err)
	}
	if err := pgQueryRow(ctx, db, `SELECT COUNT(*) FROM nodes`).Scan(&nodeCount); err != nil {
		fmt.Printf("[CRM] stats count nodes error: %v\n", err)
		return fmt.Sprintf("❌ CRM DB error (count nodes): %v", err)
	}

	// File size
	var dbSize int64
	if st, err := os.Stat(fioraCRMDBPathResolved()); err == nil {
		dbSize = st.Size()
	}

	return fmt.Sprintf("📊 *CRM Stats*\n\n• Messages: %d\n• Nodes: %d\n• DB size: %s",
		msgCount, nodeCount, formatBytes(uint64(dbSize)))
}

// fioraCRMCleanup deletes messages older than 7 days
func fioraCRMCleanup() {
	db, err := fioraCRMDBGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cutoff := time.Now().Unix() - 7*24*60*60
	if _, err := pgExec(ctx, db, `DELETE FROM messages WHERE timestamp IS NOT NULL AND timestamp < ?`, cutoff); err != nil {
		fmt.Printf("[CRM] cleanup messages error: %v\n", err)
	}
	if _, err := pgExec(ctx, db, `DELETE FROM nodes WHERE NOT EXISTS (SELECT 1 FROM messages WHERE messages.id = nodes.id AND messages.chat = nodes.chat)`); err != nil {
		fmt.Printf("[CRM] cleanup nodes error: %v\n", err)
	}
}

// ─── Types ────────────────────────────────────────────────────────

type fioraCRMMessageRow struct {
	RowID     int64
	ID        string
	Chat      string
	Sender    string
	Timestamp int64
	Data      []byte
}

// ─── Command handlers ────────────────────────────────────────────

// handleFioraCRMCommand — .crm / .insp / .relay / .lastchat / .crmstats
func handleFioraCRMCommand(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	switch cmd {
	case ".crmstats":
		return fioraCRMGetStats()
	case ".lastchat":
		return fioraCRMHandleLastChat(client, msg, args)
	case ".insp":
		return fioraCRMHandleInspect(client, msg, args)
	case ".relay":
		return fioraCRMHandleRelay(client, msg, args)
	case ".crm":
		return fioraCRMHandleCRM(client, msg, args)
	case ".run", ".crmrun":
		return fioraCRMHandleRun(client, msg, args)
	case ".adn":
		return fioraCRMHandleADN(client, msg, args)
	default:
		return "❓ *Cara pakai:* pilih salah satu:\n• .crm — generate relay code\n• .insp — inspect raw payload\n• .relay — relay message\n• .run — execute relay\n• .lastchat [N] — last messages\n• .crmstats — DB stats\n• .adn — additional nodes"
	}
}

// fioraCRMHandleLastChat — .lastchat [N] (N maks 50, anti dump besar-besaran)
func fioraCRMHandleLastChat(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	limit := 10
	if args != "" {
		var n int
		fmt.Sscanf(args, "%d", &n)
		if n > 0 && n <= 50 {
			limit = n
		}
	}
	chat := msg.Info.Chat.String()
	rows := fioraCRMGetMessages(chat, limit)
	if len(rows) == 0 {
		return "📭 Tidak ada pesan tersimpan"
	}

	// Tampilan default: plain text (rich UI dimatikan per perintah antibot)
	var entries []string
	for _, r := range rows {
		ts := time.Unix(r.Timestamp, 0).In(time.FixedZone("WIB", 7*3600))
		tsStr := ts.Format("02-01 15:04")
		sender := r.Sender
		if sender == "" {
			sender = "?"
		} else if len(sender) > 15 {
			sender = sender[:15]
		}
		entries = append(entries, fmt.Sprintf("#%d [%s] %s\n  ID: %s", r.RowID, tsStr, sender, r.ID))
	}
	return fmt.Sprintf("📋 Last %d Messages\nChat: %s\n\n%s", len(rows), chat[:min(40, len(chat))], strings.Join(entries, "\n\n"))
}

// fioraCRMHandleInspect — .insp [reply] [#rowid] [-env] [-node] [-raw] [-webmsg] [-file]
//
// Default mode: tampilkan envelope rich (semua info dari message WA yang
//
//	dicapture — key, sender, push_name, addressing, flags, content,
//	quoted, message_secret, synthetic raw node, raw proto fields).
//
// -env      : tampilkan full envelope JSONB (pretty-printed).
// -node     : tampilkan synthetic raw node + attrs saja (compact).
// -raw      : tampilkan raw_message (protojson dari msg.Message — parsed).
// -unwrapped: tampilkan raw_message_unwrapped (protojson dari msg.RawMessage — raw, mungkin masih wrapped).
// -webmsg   : tampilkan source_webmsg (protojson dari msg.SourceWebMsg — history sync).
// -newsletter: tampilkan newsletter_meta JSON.
// -file     : kirim sebagai dokumen JSON (berguna untuk payload besar).
func fioraCRMHandleInspect(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	targetID, chat := fioraCRMResolveTarget(msg, args)
	if targetID == "" {
		return "❓ *Cara pakai:* reply pesan atau *.insp #rowid*\n\nOpsi: `-env` `-node` `-raw` `-unwrapped` `-webmsg` `-newsletter` `-file`"
	}

	// Pull full row from message_archive (rich data lives there now).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row, err := queryArchiveRowByID(ctx, targetID, chat)
	if err != nil || row == nil {
		// Fallback: CRM cache (legacy protobuf-only path)
		loaded := fioraCRMLoadMessage(targetID, chat)
		if loaded == nil {
			return "❌ Pesan tidak ditemukan di archive maupun CRM cache"
		}
		jsonBytes, mErr := json.MarshalIndent(loaded, "", "  ")
		if mErr != nil {
			return fmt.Sprintf("❌ JSON marshal error: %v", mErr)
		}
		return fmt.Sprintf("🔍 Message Inspect (CRM cache fallback)\n• ID: `%s`\n\n```json\n%s\n```",
			targetID, string(jsonBytes))
	}

	// Decide what to show based on flags
	switch {
	case strings.Contains(args, "-file"):
		// Send full row as JSON file
		fullJSON := buildFullRowJSON(row)
		fileName := fmt.Sprintf("msg_%s.json", row.Type)
		meta := fmt.Sprintf("• Type: %s\n• Chat: %s\n• ID: %s\n• Sender: %s",
			row.Type, truncateStr(row.GroupJID, 30), row.MsgID, row.Sender)
		return fioraCRMSendAsDocument(client, msg, fullJSON, fileName, "application/json", meta)

	case strings.Contains(args, "-env"):
		env, perr := queryEnvelopeFromPG(ctx, targetID)
		if env == nil || perr != nil {
			return fmt.Sprintf("❌ envelope: %v", perr)
		}
		jsonBytes, _ := json.MarshalIndent(env, "", "  ")
		return fmt.Sprintf("🔍 Envelope (FullEnvelope JSONB)\n• ID: `%s`\n\n```json\n%s\n```",
			row.MsgID, prettyLimit(string(jsonBytes), 20000))

	case strings.Contains(args, "-node"):
		var sb strings.Builder
		sb.WriteString("📦 *Synthetic Raw Node*\n\n")
		sb.WriteString(fmt.Sprintf("Tag: `%s`\n\n", row.NodeTag))
		if row.NodeAttrs != "" && row.NodeAttrs != "{}" {
			var attrs map[string]string
			if json.Unmarshal([]byte(row.NodeAttrs), &attrs) == nil {
				sb.WriteString("Attrs:\n")
				// Sort keys
				var keys []string
				for k := range attrs {
					keys = append(keys, k)
				}
				sortStrings(keys)
				for _, k := range keys {
					v := attrs[k]
					if len(v) > 80 {
						v = v[:77] + "…"
					}
					sb.WriteString(fmt.Sprintf("  `%s` = `%s`\n", k, v))
				}
			}
		}
		return sb.String()

	case strings.Contains(args, "-raw"):
		if row.RawMessage == "" || row.RawMessage == "{}" {
			return "📭 raw_message kosong"
		}
		return fmt.Sprintf("🔍 raw_message (parsed waE2E.Message)\n• ID: `%s`\n\n```json\n%s\n```",
			row.MsgID, prettyLimit(row.RawMessage, 20000))

	case strings.Contains(args, "-unwrapped"):
		if row.RawMessageUnwrapped == "" || row.RawMessageUnwrapped == "{}" {
			return "📭 raw_message_unwrapped kosong"
		}
		return fmt.Sprintf("🔍 raw_message_unwrapped (raw, possibly wrapped)\n• ID: `%s`\n\n```json\n%s\n```",
			row.MsgID, prettyLimit(row.RawMessageUnwrapped, 20000))

	case strings.Contains(args, "-webmsg"):
		if row.SourceWebMsg == "" || row.SourceWebMsg == "{}" {
			return "📭 source_webmsg kosong (bukan dari history sync)"
		}
		return fmt.Sprintf("🔍 source_webmsg (history sync)\n• ID: `%s`\n\n```json\n%s\n```",
			row.MsgID, prettyLimit(row.SourceWebMsg, 20000))

	case strings.Contains(args, "-newsletter"):
		if row.NewsletterMeta == "" || row.NewsletterMeta == "{}" {
			return "📭 newsletter_meta kosong (bukan dari newsletter)"
		}
		return fmt.Sprintf("🔍 newsletter_meta\n• ID: `%s`\n\n```json\n%s\n```",
			row.MsgID, prettyLimit(row.NewsletterMeta, 20000))
	}

	// Default: rich formatted envelope display
	env, _ := queryEnvelopeFromPG(ctx, targetID)
	if env == nil {
		// Couldn't parse envelope, but row exists — show basic info
		return fmt.Sprintf("🔍 Message (basic)\n• ID: `%s`\n• Chat: `%s`\n• Sender: `%s`\n• Type: %s\n• Text: %s",
			row.MsgID, row.GroupJID, row.Sender, row.Type, truncateStr(row.Text, 200))
	}
	return formatEnvelopeForDisplay(env)
}

// buildFullRowJSON serializes the full archiveRow to pretty JSON for -file mode.
func buildFullRowJSON(row *archiveRow) []byte {
	// Re-create a map of all fields, parsing JSONB strings back to objects
	// so the output is one coherent JSON document, not nested strings.
	out := map[string]any{
		"msg_id":                   row.MsgID,
		"group_jid":                row.GroupJID,
		"sender":                   row.Sender,
		"sender_user":              row.SenderUser,
		"sender_server":            row.SenderServer,
		"push_name":                row.PushName,
		"from_me":                  row.FromMe,
		"timestamp":                row.Timestamp,
		"is_group":                 row.IsGroup,
		"addressing_mode":          row.AddressingMode,
		"participant":              row.Participant,
		"participant_pn":           row.ParticipantPN,
		"is_ephemeral":             row.IsEphemeral,
		"is_view_once":             row.IsViewOnce,
		"is_view_once_v2":          row.IsViewOnceV2,
		"is_view_once_v2_ext":      row.IsViewOnceV2Ext,
		"is_document_with_caption": row.IsDocumentWithCaption,
		"is_lottie_sticker":        row.IsLottieSticker,
		"is_bot_invoke":            row.IsBotInvoke,
		"is_edit":                  row.IsEdit,
		"broadcast":                row.Broadcast,
		"retry_count":              row.RetryCount,
		"unavailable_request_id":   row.UnavailableRequestID,
		"type":                     row.Type,
		"text":                     row.Text,
		"caption":                  row.Caption,
		"media_path":               row.MediaPath,
		"media_url":                row.MediaURL,
		"media_mime":               row.MediaMime,
		"media_key":                row.MediaKey,
		"file_length":              row.FileLength,
		"quoted_msg_id":            row.QuotedMsgID,
		"quoted_participant":       row.QuotedParticipant,
		"quoted_msg_text":          row.QuotedMsgText,
		"quoted_msg_type":          row.QuotedMsgType,
		"message_secret":           row.MessageSecret,
		"node_tag":                 row.NodeTag,
		"created_at":               row.CreatedAt,
	}
	// Parse JSONB strings into objects so they're not nested strings
	for _, pair := range [][2]string{
		{"node_attrs", row.NodeAttrs},
		{"envelope", row.Envelope},
		{"raw_message", row.RawMessage},
		{"raw_message_unwrapped", row.RawMessageUnwrapped},
		{"source_webmsg", row.SourceWebMsg},
		{"newsletter_meta", row.NewsletterMeta},
		{"additional_attrs", row.AdditionalAttrs},
		{"additional_nodes", row.AdditionalNodes},
	} {
		if pair[1] != "" && pair[1] != "{}" {
			var v any
			if json.Unmarshal([]byte(pair[1]), &v) == nil {
				out[pair[0]] = v
			}
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return b
}

// prettyLimit truncates a JSON string to maxLen characters, appending
// a truncation marker if needed. Always preserves the last valid JSON
// prefix when possible.
func prettyLimit(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}

// truncateStr is provided by kangwifi.go (same package).

// fioraCRMHandleRelay — .relay [reply] [#rowid]
func fioraCRMHandleRelay(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	targetID, chat := fioraCRMResolveTarget(msg, args)
	if targetID == "" {
		return "❓ *Cara pakai:* reply pesan atau *.relay #rowid*"
	}

	loaded := fioraCRMLoadMessage(targetID, chat)
	if loaded == nil {
		return "❌ Pesan tidak ditemukan di CRM cache"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.SendMessage(ctx, msg.Info.Chat, loaded)
	if err != nil {
		return fmt.Sprintf("❌ Relay error: %v", err)
	}
	return fmt.Sprintf("✅ Relayed: %s", resp.ID)
}

// fioraCRMHandleCRM — .crm [reply] [#rowid] [-snip]
func fioraCRMHandleCRM(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	targetID, chat := fioraCRMResolveTarget(msg, args)
	if targetID == "" {
		return "❓ *Cara pakai:* reply pesan atau *.crm #rowid*\n\n*Command:*\n• .crm — generate relay code\n• .insp — inspect raw payload\n• .relay — relay message\n• .lastchat [N] — last messages\n• .crmstats — DB stats"
	}

	loaded := fioraCRMLoadMessage(targetID, chat)
	if loaded == nil {
		return "❌ Pesan tidak ditemukan di CRM cache"
	}

	// Generate relay code (Go version)
	msgType := fioraCRMGetMessageType(loaded)
	relayCode := fioraCRMBuildRelayCode(loaded, targetID, chat)

	metadata := fmt.Sprintf("• Type: %s\n• Chat: %s\n• ID: %s",
		msgType, chat[:min(30, len(chat))], targetID[:min(20, len(targetID))])

	if strings.Contains(args, "-file") {
		return fioraCRMSendAsDocument(client, msg, []byte(relayCode), fmt.Sprintf("%s.go", msgType), "text/plain", metadata)
	}

	// Tampilan default: plain text
	if len(relayCode) <= 20000 {
		return fmt.Sprintf("⚡ Relay Code\n%s\n\n```go\n%s\n```", metadata, relayCode)
	}

	// Too large — send as file
	return fioraCRMSendAsDocument(client, msg, []byte(relayCode), fmt.Sprintf("%s.go", msgType), "text/plain", metadata)
}

// ─── Helpers ─────────────────────────────────────────────────────

// fioraCRMResolveTarget extracts target message ID + chat from args or reply
func fioraCRMResolveTarget(msg *events.Message, args string) (string, string) {
	if msg == nil {
		return "", ""
	}
	// Check for #rowid
	if strings.HasPrefix(args, "#") {
		var rowid int64
		fmt.Sscanf(args, "#%d", &rowid)
		if rowid > 0 {
			db, err := fioraCRMDBGet()
			if err != nil {
				return "", ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var id, chat string
			err = pgQueryRow(ctx, db,
				`SELECT id, chat FROM messages WHERE rowid = ? LIMIT 1`, rowid).Scan(&id, &chat)
			if err == nil {
				return id, chat
			}
		}
	}

	// Check reply
	quoted := getQuotedMessage(msg)
	if quoted != nil && quoted.Info.ID != "" {
		chat := msg.Info.Chat.String()
		return quoted.Info.ID, chat
	}

	return "", ""
}

// fioraCRMGetMessageType returns the type of a WhatsApp message
func fioraCRMGetMessageType(msg *waE2E.Message) string {
	if msg == nil {
		return "unknown"
	}
	if msg.GetConversation() != "" {
		return "conversation"
	}
	if msg.GetExtendedTextMessage() != nil {
		return "extendedText"
	}
	if msg.GetImageMessage() != nil {
		return "image"
	}
	if msg.GetVideoMessage() != nil {
		return "video"
	}
	if msg.GetStickerMessage() != nil {
		return "sticker"
	}
	if msg.GetAudioMessage() != nil {
		return "audio"
	}
	if msg.GetDocumentMessage() != nil {
		return "document"
	}
	if msg.GetTemplateMessage() != nil {
		return "template"
	}
	if msg.GetStickerPackMessage() != nil {
		return "stickerPack"
	}
	return "other"
}

// fioraCRMBuildRelayCode generates Go relay code for a message
func fioraCRMBuildRelayCode(msg *waE2E.Message, id, chat string) string {
	msgType := fioraCRMGetMessageType(msg)

	// Get a snippet of the message data
	jsonBytes, _ := json.MarshalIndent(msg, "", "  ")
	jsonStr := string(jsonBytes)
	if len(jsonStr) > 2000 {
		jsonStr = jsonStr[:2000] + "\n// ... (truncated)"
	}

	return fmt.Sprintf(`// Relay code for message: %s
// Type: %s
// Chat: %s
// Generated by Bot MD CRM

// To relay this message:
// client.SendMessage(ctx, chat, msg)

// Message proto (base64):
// %s

// JSON representation:
/*
%s
*/
`, id, msgType, chat, base64.StdEncoding.EncodeToString(fioraCRMEncodeMessage(msg)), jsonStr)
}

// fioraCRMEncodeMessage encodes a message to protobuf bytes
func fioraCRMEncodeMessage(msg *waE2E.Message) []byte {
	data, err := proto.Marshal(msg)
	if err != nil {
		return nil
	}
	return data
}

// ─── .run / .crmrun — Execute relay from CRM cache ───────────────

// fioraCRMHandleRun — .run [reply] [#rowid]
// Loads message from CRM cache + relays it WITH its child messages
// (messages that quote/reference it). This is the full relay: parent + children.
func fioraCRMHandleRun(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	targetID, chat := fioraCRMResolveTarget(msg, args)
	if targetID == "" {
		return "❓ *Cara pakai:* reply pesan atau *.run #rowid*"
	}

	loaded := fioraCRMLoadMessage(targetID, chat)
	if loaded == nil {
		return "❌ Pesan tidak ditemukan di CRM cache"
	}

	msgType := fioraCRMGetMessageType(loaded)

	// Relay the parent message
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.SendMessage(ctx, msg.Info.Chat, loaded)
	if err != nil {
		return fmt.Sprintf("❌ Relay error: %v", err)
	}

	// Find and relay child messages (messages that quote this one)
	children := fioraCRMFindChildren(chat, targetID, 20)
	childCount := 0
	for _, childID := range children {
		childMsg := fioraCRMLoadMessage(childID, chat)
		if childMsg == nil {
			continue
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := client.SendMessage(ctx2, msg.Info.Chat, childMsg)
		cancel2()
		if err == nil {
			childCount++
		}
		time.Sleep(200 * time.Millisecond) // rate limit
	}

	metadata := fmt.Sprintf("• Type: %s\n• Parent ID: %s\n• Children relayed: %d\n• Total: %d",
		msgType, resp.ID, childCount, 1+childCount)

	// Tampilan default: plain text
	return fmt.Sprintf("✅ Relay Executed\n%s", metadata)
}

// fioraCRMFindChildren finds messages that quote/reference the parent message ID.
// Searches CRM cache for messages whose ContextInfo.StanzaID matches parentID.
func fioraCRMFindChildren(chat, parentID string, limit int) []string {
	db, err := fioraCRMDBGet()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get all messages in this chat, check if any quote the parent
	rows, err := pgQuery(ctx, db,
		`SELECT id, data FROM messages WHERE chat = ? ORDER BY rowid DESC LIMIT ?`,
		chat, limit*5) // search wider
	if err != nil {
		return nil
	}
	defer rows.Close()

	var children []string
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			continue
		}
		if id == parentID {
			continue // skip parent itself
		}

		// Decode message and check if it quotes parentID
		msg := &waE2E.Message{}
		if err := proto.Unmarshal(data, msg); err != nil {
			continue
		}

		// Check ExtendedTextMessage.ContextInfo.StanzaID
		if ext := msg.GetExtendedTextMessage(); ext != nil {
			if ci := ext.GetContextInfo(); ci != nil {
				if ci.GetStanzaID() == parentID {
					children = append(children, id)
					if len(children) >= limit {
						break
					}
				}
			}
		}
	}
	return children
}

// ─── .adn — Additional Nodes ────────────────────────────────────

// fioraCRMHandleADN — .adn [reply] [#rowid]
// Extracts and displays additional protocol nodes from a message.
func fioraCRMHandleADN(client *whatsmeow.Client, msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	targetID, chat := fioraCRMResolveTarget(msg, args)
	if targetID == "" {
		return "❓ *Cara pakai:* reply pesan atau *.adn #rowid*"
	}

	loaded := fioraCRMLoadMessage(targetID, chat)
	if loaded == nil {
		return "❌ Pesan tidak ditemukan di CRM cache"
	}

	msgType := fioraCRMGetMessageType(loaded)

	// Extract all nested messages and special fields
	var nodes []string

	// Check protocolMessage (edits, revokes)
	if pm := loaded.GetProtocolMessage(); pm != nil {
		pmType := "unknown"
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			pmType = "REVOKE"
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			pmType = "MESSAGE_EDIT"
		case waE2E.ProtocolMessage_EPHEMERAL_SETTING:
			pmType = "EPHEMERAL_SETTING"
		}
		nodes = append(nodes, fmt.Sprintf("protocolMessage: type=%s", pmType))
		if pm.GetKey() != nil {
			nodes = append(nodes, fmt.Sprintf("  key.id=%s", pm.GetKey().GetID()))
		}
		if pm.GetEditedMessage() != nil {
			editedType := fioraCRMGetMessageType(pm.GetEditedMessage())
			nodes = append(nodes, fmt.Sprintf("  editedMessage.type=%s", editedType))
		}
	}

	// Check secretEncryptedMessage
	if se := loaded.GetSecretEncryptedMessage(); se != nil {
		nodes = append(nodes, fmt.Sprintf("secretEncryptedMessage: type=%v", se.GetSecretEncType()))
		if se.GetTargetMessageKey() != nil {
			nodes = append(nodes, fmt.Sprintf("  targetKey.id=%s", se.GetTargetMessageKey().GetID()))
		}
	}

	// Check messageContextInfo
	if mci := loaded.GetMessageContextInfo(); mci != nil {
		if mci.GetMessageSecret() != nil {
			nodes = append(nodes, fmt.Sprintf("messageContextInfo.messageSecret: %d bytes", len(mci.GetMessageSecret())))
		}
		if mci.GetDeviceListMetadata() != nil {
			nodes = append(nodes, "messageContextInfo.deviceListMetadata: present")
		}
	}

	// Check contextInfo (from extendedText, image, video, etc.)
	if ext := loaded.GetExtendedTextMessage(); ext != nil {
		if ci := ext.GetContextInfo(); ci != nil {
			if ci.GetStanzaID() != "" {
				nodes = append(nodes, fmt.Sprintf("contextInfo.stanzaID: %s", ci.GetStanzaID()))
			}
			if ci.GetParticipant() != "" {
				nodes = append(nodes, fmt.Sprintf("contextInfo.participant: %s", ci.GetParticipant()))
			}
			if ci.GetIsForwarded() {
				nodes = append(nodes, "contextInfo.isForwarded: true")
			}
			if ci.GetForwardingScore() > 0 {
				nodes = append(nodes, fmt.Sprintf("contextInfo.forwardingScore: %d", ci.GetForwardingScore()))
			}
			if ci.GetMentionedJID() != nil {
				nodes = append(nodes, fmt.Sprintf("contextInfo.mentionedJID: %v", ci.GetMentionedJID()))
			}
			if ci.GetQuotedMessage() != nil {
				quotedType := fioraCRMGetMessageType(ci.GetQuotedMessage())
				nodes = append(nodes, fmt.Sprintf("contextInfo.quotedMessage.type: %s", quotedType))
			}
		}
	}

	// Check button/message types
	if loaded.GetTemplateMessage() != nil {
		nodes = append(nodes, "templateMessage: present")
	}
	if loaded.GetInteractiveMessage() != nil {
		nodes = append(nodes, "interactiveMessage: present")
	}
	if loaded.GetStickerPackMessage() != nil {
		nodes = append(nodes, "stickerPackMessage: present")
	}
	if loaded.GetBotForwardedMessage() != nil {
		nodes = append(nodes, "botForwardedMessage: present")
	}
	if loaded.GetReactionMessage() != nil {
		rm := loaded.GetReactionMessage()
		nodes = append(nodes, fmt.Sprintf("reactionMessage: text=%s key.id=%s",
			rm.GetText(), rm.GetKey().GetID()))
	}

	if len(nodes) == 0 {
		return "📭 No AdditionalNodes captured"
	}

	metadata := fmt.Sprintf("• Type: %s\n• Chat: %s\n• ID: %s\n• Nodes: %d",
		msgType, chat[:min(30, len(chat))], targetID[:min(20, len(targetID))], len(nodes))

	// Tampilan default: plain text
	return fmt.Sprintf("📸 Additional Nodes\n%s\n\n```\n%s\n```", metadata, strings.Join(nodes, "\n"))
}

// fioraCRMSendAsDocument sends data as a WhatsApp document message
func fioraCRMSendAsDocument(client *whatsmeow.Client, msg *events.Message, data []byte, fileName, mimeType, caption string) string {
	if msg == nil {
		return ""
	}
	if client == nil {
		return "❌ Client nil"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	up, err := client.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return fmt.Sprintf("❌ Upload: %v", err)
	}

	docMsg := &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			FileName:      proto.String(fileName),
			Caption:       proto.String(caption),
		},
	}
	docMsg = humanizeMessage(docMsg)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	_, err = client.SendMessage(ctx2, msg.Info.Chat, docMsg)
	if err != nil {
		return fmt.Sprintf("❌ Send: %v", err)
	}
	return "" // silent — document itself is the response
}

// ─── Auto-save hook ──────────────────────────────────────────────

// fioraCRMAutoSave is called from main.go message handler to capture every message.
// DEFAULT OFF: hanya arsipkan bila grup diaktifkan via .crmon.
func fioraCRMAutoSave(client *whatsmeow.Client, msg *events.Message) {
	if msg == nil || msg.Message == nil {
		return
	}
	chat := msg.Info.Chat.String()
	if chat == "" {
		return
	}
	if !crmGroupEnabled(chat) {
		return // arsip mati untuk grup ini (default)
	}
	go fioraCRMSaveMessage(client, msg, chat)
}

// keep imports referenced
var _ = filepath.Join
var _ = types.JID{}

// ─── Legacy CRM stubs (for backward compat with antifdmsg.go, say.go, etc) ──

var (
	crmMu    sync.RWMutex
	crmCache = map[string]*crmEntry{}
)

type crmEntry struct {
	ID      string
	Chat    string
	Sender  string
	Message *waE2E.Message
	Content *waE2E.Message
	SavedAt time.Time
}

// Batas crmCache agar tidak bocor memori di grup ramai.
// Tanpa eviction, map ini tumbuh tanpa batas karena diisi tiap pesan masuk.
const (
	crmCacheMaxEntries = 300
	crmCacheTTL        = 30 * time.Minute
)

var crmJanitorOnce sync.Once

// crmEvictLocked menghapus entry kedaluwarsa (>TTL); jika masih melebihi
// batas, hapus yang terlama dulu. Pemanggil harus memegang crmMu.
func crmEvictLocked(now time.Time) {
	for k, e := range crmCache {
		if now.Sub(e.SavedAt) > crmCacheTTL {
			delete(crmCache, k)
		}
	}
	for len(crmCache) > crmCacheMaxEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, e := range crmCache {
			if first || e.SavedAt.Before(oldestTime) {
				oldestKey, oldestTime, first = k, e.SavedAt, false
			}
		}
		if first {
			break
		}
		delete(crmCache, oldestKey)
	}
}

// crmStartJanitor menjalankan pembersih periodik (sekali per proses).
func crmStartJanitor() {
	crmJanitorOnce.Do(func() {
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for range t.C {
				crmMu.Lock()
				crmEvictLocked(time.Now())
				crmMu.Unlock()
			}
		}()
	})
}

func crmRememberIncoming(msg *events.Message) {
	if msg == nil || msg.Message == nil {
		return
	}
	crmStartJanitor()
	crmMu.Lock()
	defer crmMu.Unlock()
	id := msg.Info.ID
	chat := msg.Info.Chat.String()
	now := time.Now()
	// Evict malas: hanya scan saat map mendekati batas (hemat CPU).
	if len(crmCache) >= crmCacheMaxEntries {
		crmEvictLocked(now)
	}
	crmCache[id+chat] = &crmEntry{
		ID:      id,
		Chat:    chat,
		Sender:  msg.Info.Sender.String(),
		Message: msg.Message,
		SavedAt: now,
	}
	// Also save to fiora CRM DB (hanya bila grup aktif)
	if crmGroupEnabled(chat) {
		go fioraCRMSaveMessage(nil, msg, chat)
	}
}

func crmListByChat(chat string, limit int) []*crmEntry {
	crmMu.RLock()
	defer crmMu.RUnlock()
	var result []*crmEntry
	for _, e := range crmCache {
		if e.Chat == chat {
			result = append(result, e)
		}
	}
	if len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result
}

func crmUnwrap(msg *waE2E.Message) *waE2E.Message {
	if msg == nil {
		return nil
	}
	// Unwrap ViewOnceMessage envelope
	if vom := msg.GetViewOnceMessage(); vom != nil {
		if inner := vom.GetMessage(); inner != nil {
			return crmUnwrap(inner)
		}
	}
	// Unwrap ViewOnceMessageV2 envelope
	if vom2 := msg.GetViewOnceMessageV2(); vom2 != nil {
		if inner := vom2.GetMessage(); inner != nil {
			return crmUnwrap(inner)
		}
	}
	// Unwrap ViewOnceMessageV2Extension (kadang dipakai)
	if ext := msg.GetViewOnceMessageV2Extension(); ext != nil {
		if inner := ext.GetMessage(); inner != nil {
			return crmUnwrap(inner)
		}
	}
	return msg
}

func handleCRMRunCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	return "❓ .crmrun deprecated — pakai *.crm* atau *.relay*"
}

func handleCekMsgCommand(msg *events.Message, args string) string {
	if msg == nil {
		return ""
	}
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	chat := msg.Info.Chat.String()
	count, err := fioraCRMCountMessages()
	if err != nil {
		fmt.Printf("[CRM] .cekmsg count error: %v\n", err)
		return fmt.Sprintf("❌ CRM DB error: %v", err)
	}
	return fmt.Sprintf("📋 *CRM Cache*\n\n• Messages: %d\n• Chat: %s", count, chat[:min(30, len(chat))])
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "crm",
		Aliases:     []string{"cap", "cms", "capture"},
		PreGate:     true,
		Description: "crm",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFioraCRMCommand(client, msg, ".crm", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "insp",
		PreGate:     true,
		Description: "insp",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFioraCRMCommand(client, msg, ".insp", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "relay",
		PreGate:     true,
		Description: "relay",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFioraCRMCommand(client, msg, ".relay", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "lastchat",
		PreGate:     true,
		Description: "lastchat",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFioraCRMCommand(client, msg, ".lastchat", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "crmstats",
		PreGate:     true,
		Description: "crmstats",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFioraCRMCommand(client, msg, ".crmstats", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "run",
		Aliases:     []string{"crmrun"},
		PreGate:     true,
		Description: "run",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFioraCRMCommand(client, msg, ".run", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "adn",
		PreGate:     true,
		Description: "adn",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleFioraCRMCommand(client, msg, ".adn", args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "cekmsg",
		PreGate:     true,
		Description: "cekmsg",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCekMsgCommand(msg, args)
		},
	})
}
