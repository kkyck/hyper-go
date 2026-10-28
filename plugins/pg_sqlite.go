package main

// pg_sqlite.go — SQLite backend: SQL transpiler + schema.
// ════════════════════════════════════════════════════════════
//
// The bot's SQL is written once in a dialect-neutral style (? placeholders,
// no explicit casts). PostgreSQL accepts it almost as-is; SQLite needs a few
// constructs rewritten. This file contains:
//
//	sqliteTranspile(q)  — rewrite PostgreSQL-only SQL for SQLite
//	pgInitAllSQLite(db) — create every table the bot needs
//	sqliteHasColumn(...)— column introspection (SQLite has no ADD COLUMN IF
//	                      NOT EXISTS)
//
// The transpiler handles the constructs actually used by the plugins:
//
//	NOW()                              → datetime('now')
//	NOW() - INTERVAL '24 hours'        → datetime('now','-24 hours')
//	EXTRACT(EPOCH FROM NOW())::BIGINT  → CAST(strftime('%s','now') AS INTEGER)
//	expr::text / ::jsonb / ::float8    → CAST(expr AS TEXT / REAL)
//	expr::bigint / ::timestamptz       → CAST(expr AS INTEGER / TEXT)
//
// SQLite 3.38+ understands the `->>` JSON operator natively, so JSONB
// lookups (`details->>'status'`) work unchanged.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// ─── SQL transpiler ───────────────────────────────────────────

// sqliteIntervalRe matches "NOW() - INTERVAL '24 hours'" (and the + variant).
var sqliteIntervalRe = regexp.MustCompile(`(?i)NOW\s*\(\s*\)\s*([+-])\s*INTERVAL\s+'([^']*)'`)

// sqliteTranspile rewrites PostgreSQL-only SQL into SQLite-compatible SQL.
// Statements without PostgreSQL constructs are returned unchanged.
func sqliteTranspile(query string) string {
	q := query

	// 1. Epoch extraction (must run before the generic NOW() replacement).
	q = strings.ReplaceAll(q, "EXTRACT(EPOCH FROM NOW())", "strftime('%s','now')")
	q = regexp.MustCompile(`(?i)EXTRACT\s*\(\s*EPOCH\s+FROM\s+NOW\s*\(\s*\)\s*\)`).ReplaceAllString(q, "strftime('%s','now')")

	// 2. NOW() ± INTERVAL
	q = sqliteIntervalRe.ReplaceAllStringFunc(q, func(m string) string {
		sub := sqliteIntervalRe.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		sign, spec := sub[1], strings.TrimSpace(sub[2])
		if spec == "" {
			return "datetime('now')"
		}
		return fmt.Sprintf("datetime('now','%s%s')", sign, spec)
	})

	// 3. Plain NOW()
	q = regexp.MustCompile(`(?i)\bNOW\s*\(\s*\)`).ReplaceAllString(q, "datetime('now')")

	// 4. PostgreSQL casts (::type)
	q = sqliteRewriteCasts(q)

	return q
}

// sqliteRewriteCasts turns PostgreSQL's `expr::type` into SQLite's
// `CAST(expr AS type)`.
func sqliteRewriteCasts(s string) string {
	out := make([]byte, 0, len(s)+32)
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\'' {
			j := skipSQLString(s, i)
			out = append(out, s[i:j]...)
			i = j
			continue
		}
		if c == ':' && i+1 < len(s) && s[i+1] == ':' {
			j := i + 2
			for j < len(s) && isTypeNameByte(s[j]) {
				j++
			}
			typeName := strings.ToLower(strings.TrimSpace(s[i+2 : j]))
			if typeName != "" {
				// Keep trailing spaces out of the consumed range — they belong
				// to the surrounding SQL, not to the type name.
				j = i + 2 + len(strings.TrimRight(s[i+2:j], " \t\n\r"))
				opStart := sqlExprStart(out)
				operand := string(out[opStart:])
				out = out[:opStart]
				out = append(out, "CAST("...)
				out = append(out, operand...)
				out = append(out, " AS "...)
				out = append(out, sqliteCastType(typeName)...)
				out = append(out, ')')
				i = j
				continue
			}
		}
		out = append(out, c)
		i++
	}
	return string(out)
}

// sqliteCastType maps a PostgreSQL type name to its SQLite storage class.
func sqliteCastType(t string) string {
	switch t {
	case "float8", "float4", "double precision", "real", "numeric", "decimal":
		return "REAL"
	case "bigint", "int", "integer", "smallint", "bigserial", "serial", "int2", "int4", "int8":
		return "INTEGER"
	case "boolean", "bool":
		return "INTEGER"
	case "bytea":
		return "BLOB"
	default:
		// text, varchar, json, jsonb, uuid, date, timestamp, timestamptz, ...
		return "TEXT"
	}
}

// skipSQLString returns the index just past the string literal that starts
// at position i (the opening quote).
func skipSQLString(s string, i int) int {
	quote := s[i]
	j := i + 1
	for j < len(s) {
		if s[j] == quote {
			// Doubled quote ('') is an escaped quote, not the end.
			if j+1 < len(s) && s[j+1] == quote {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(s)
}

// sqlExprStart returns the offset of the expression that ends at the end of
// buf. Handles parenthesised groups, quoted literals and bare identifiers.
func sqlExprStart(buf []byte) int {
	i := len(buf) - 1
	for i >= 0 && isSpaceByte(buf[i]) {
		i--
	}
	if i < 0 {
		return len(buf)
	}
	switch buf[i] {
	case ')':
		depth := 0
		for ; i >= 0; i-- {
			switch buf[i] {
			case ')':
				depth++
			case '(':
				depth--
				if depth == 0 {
					// Include a preceding function name (f(x) is one operand).
					j := i - 1
					for j >= 0 && isIdentByte(buf[j]) {
						j--
					}
					return j + 1
				}
			}
		}
		return len(buf)
	case '\'':
		j := i - 1
		for j >= 0 && buf[j] != '\'' {
			j--
		}
		if j >= 0 {
			return j
		}
		return i
	case '"':
		j := i - 1
		for j >= 0 && buf[j] != '"' {
			j--
		}
		if j >= 0 {
			return j
		}
		return i
	default:
		j := i
		for j >= 0 && isIdentByte(buf[j]) {
			j--
		}
		return j + 1
	}
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c == '.' || c == '?' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isTypeNameByte(c byte) bool {
	return c == '_' || c == ' ' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// sqliteHasColumn reports whether the given table already has the column.
// SQLite has no "ADD COLUMN IF NOT EXISTS", so we introspect first.
func sqliteHasColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			continue
		}
		if strings.EqualFold(name, column) {
			return true
		}
	}
	return false
}

// ─── Schema ───────────────────────────────────────────────────

// pgInitAllSQLite creates every table the bot needs on SQLite.
// Mirrors pgInitAllPostgres one-for-one so both backends expose the same
// schema and the same queries keep working.
func pgInitAllSQLite(db *sql.DB) {
	// ─── CRM tables ─────────────────────────────────────────────
	pgExecLog(db, "messages", `CREATE TABLE IF NOT EXISTS messages (
                rowid INTEGER PRIMARY KEY AUTOINCREMENT,
                id TEXT NOT NULL,
                chat TEXT NOT NULL,
                sender TEXT,
                timestamp BIGINT,
                data BLOB
        )`)
	pgExecLog(db, "messages_idx", `CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages (chat)`)
	pgExecLog(db, "messages_idx_ts", `CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages (timestamp)`)

	pgExecLog(db, "nodes", `CREATE TABLE IF NOT EXISTS nodes (
                id TEXT NOT NULL,
                chat TEXT NOT NULL,
                data TEXT NOT NULL,
                created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
                PRIMARY KEY(id, chat)
        )`)
	pgExecLog(db, "nodes_idx", `CREATE INDEX IF NOT EXISTS idx_nodes_chat ON nodes (chat)`)

	// ─── AI conversation memory ─────────────────────────────────
	pgExecLog(db, "conversations", `CREATE TABLE IF NOT EXISTS conversations (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_jid TEXT NOT NULL,
                role TEXT NOT NULL,
                content TEXT NOT NULL,
                sender_name TEXT DEFAULT '',
                timestamp BIGINT NOT NULL
        )`)
	pgExecLog(db, "conversations_idx", `CREATE INDEX IF NOT EXISTS idx_conversations_chat ON conversations (chat_jid, timestamp DESC)`)

	pgExecLog(db, "thoughts", `CREATE TABLE IF NOT EXISTS thoughts (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_jid TEXT NOT NULL,
                thought TEXT NOT NULL,
                timestamp BIGINT NOT NULL
        )`)
	pgExecLog(db, "thoughts_idx", `CREATE INDEX IF NOT EXISTS idx_thoughts_chat ON thoughts (chat_jid, timestamp DESC)`)

	// ─── Message archive ────────────────────────────────────────
	pgExecLog(db, "message_archive_v2_drop", `DROP TABLE IF EXISTS message_archive`)
	pgExecLog(db, "message_archive_v2", `CREATE TABLE IF NOT EXISTS message_archive (
                id                          INTEGER PRIMARY KEY AUTOINCREMENT,
                msg_id                      TEXT NOT NULL,
                group_jid                   TEXT NOT NULL,
                sender                      TEXT NOT NULL,
                sender_user                 TEXT DEFAULT '',
                sender_server               TEXT DEFAULT '',
                push_name                   TEXT DEFAULT '',
                from_me                     BOOLEAN DEFAULT FALSE,
                timestamp                   BIGINT NOT NULL,
                is_group                    BOOLEAN DEFAULT FALSE,
                addressing_mode             TEXT DEFAULT '',
                participant                 TEXT DEFAULT '',
                participant_pn              TEXT DEFAULT '',
                is_ephemeral                BOOLEAN DEFAULT FALSE,
                is_view_once                BOOLEAN DEFAULT FALSE,
                is_view_once_v2             BOOLEAN DEFAULT FALSE,
                is_view_once_v2_ext         BOOLEAN DEFAULT FALSE,
                is_document_with_caption    BOOLEAN DEFAULT FALSE,
                is_lottie_sticker           BOOLEAN DEFAULT FALSE,
                is_bot_invoke               BOOLEAN DEFAULT FALSE,
                is_edit                     BOOLEAN DEFAULT FALSE,
                broadcast                   BOOLEAN DEFAULT FALSE,
                retry_count                 INTEGER DEFAULT 0,
                unavailable_request_id      TEXT DEFAULT '',
                type                        TEXT DEFAULT 'other',
                text                        TEXT,
                caption                     TEXT DEFAULT '',
                media_path                  TEXT DEFAULT '',
                media_url                   TEXT DEFAULT '',
                media_mime                  TEXT DEFAULT '',
                media_key                   TEXT DEFAULT '',
                file_length                 BIGINT DEFAULT 0,
                quoted_msg_id               TEXT DEFAULT '',
                quoted_participant          TEXT DEFAULT '',
                quoted_msg_text             TEXT DEFAULT '',
                quoted_msg_type             TEXT DEFAULT '',
                message_secret              TEXT DEFAULT '',
                node_tag                    TEXT DEFAULT '',
                node_attrs                  TEXT DEFAULT '{}',
                envelope                    TEXT DEFAULT '{}',
                raw_message                 TEXT DEFAULT '{}',
                raw_message_unwrapped       TEXT DEFAULT '{}',
                source_webmsg               TEXT DEFAULT '{}',
                newsletter_meta             TEXT DEFAULT '{}',
                additional_attrs            TEXT DEFAULT '{}',
                additional_nodes            TEXT DEFAULT '{}',
                created_at                  TIMESTAMP NOT NULL DEFAULT (datetime('now'))
        )`)
	pgExecLog(db, "message_archive_idx_group", `CREATE INDEX IF NOT EXISTS idx_archive_group       ON message_archive (group_jid)`)
	pgExecLog(db, "message_archive_idx_sender", `CREATE INDEX IF NOT EXISTS idx_archive_sender     ON message_archive (sender)`)
	pgExecLog(db, "message_archive_idx_msgid", `CREATE INDEX IF NOT EXISTS idx_archive_msgid      ON message_archive (msg_id)`)
	pgExecLog(db, "message_archive_idx_ts", `CREATE INDEX IF NOT EXISTS idx_archive_ts         ON message_archive (timestamp DESC)`)
	pgExecLog(db, "message_archive_idx_group_ts", `CREATE INDEX IF NOT EXISTS idx_archive_group_ts   ON message_archive (group_jid, timestamp DESC)`)
	pgExecLog(db, "message_archive_idx_sender_ts", `CREATE INDEX IF NOT EXISTS idx_archive_sender_ts  ON message_archive (sender, timestamp DESC)`)
	pgExecLog(db, "message_archive_idx_quoted", `CREATE INDEX IF NOT EXISTS idx_archive_quoted     ON message_archive (quoted_msg_id) WHERE quoted_msg_id <> ''`)
	pgExecLog(db, "message_archive_idx_type", `CREATE INDEX IF NOT EXISTS idx_archive_type       ON message_archive (type)`)
	pgExecLog(db, "message_archive_idx_broadcast", `CREATE INDEX IF NOT EXISTS idx_archive_broadcast  ON message_archive (broadcast) WHERE broadcast = true`)

	// ─── KV store ───────────────────────────────────────────────
	pgExecLog(db, "kv_store", `CREATE TABLE IF NOT EXISTS kv_store (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL,
                updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
        )`)

	// ─── Per-group plugin settings ──────────────────────────────
	pgExecLog(db, "group_settings", `CREATE TABLE IF NOT EXISTS group_settings (
                group_jid  TEXT NOT NULL,
                plugin     TEXT NOT NULL,
                enabled    BOOLEAN NOT NULL DEFAULT FALSE,
                config     TEXT NOT NULL DEFAULT '{}',
                updated_at TIMESTAMP NOT NULL DEFAULT (datetime('now')),
                PRIMARY KEY (group_jid, plugin)
        )`)
	pgExecLog(db, "group_settings_idx_plugin", `CREATE INDEX IF NOT EXISTS idx_group_settings_plugin  ON group_settings (plugin)`)
	pgExecLog(db, "group_settings_idx_enabled", `CREATE INDEX IF NOT EXISTS idx_group_settings_enabled ON group_settings (plugin) WHERE enabled = true`)
	pgExecLog(db, "group_settings_idx_jid", `CREATE INDEX IF NOT EXISTS idx_group_settings_jid     ON group_settings (group_jid)`)

	// ─── AI feedback ────────────────────────────────────────────
	pgExecLog(db, "ai_feedback", `CREATE TABLE IF NOT EXISTS ai_feedback (
                id           INTEGER PRIMARY KEY AUTOINCREMENT,
                msg_id       TEXT NOT NULL,
                chat         TEXT NOT NULL,
                sender       TEXT,
                rating       TEXT NOT NULL,
                ai_text      TEXT DEFAULT '',
                user_feedback TEXT DEFAULT '',
                timestamp   BIGINT NOT NULL,
                created_at   TIMESTAMP NOT NULL DEFAULT (datetime('now'))
        )`)
	pgExecLog(db, "ai_feedback_idx_msg", `CREATE INDEX IF NOT EXISTS idx_ai_feedback_msg_id  ON ai_feedback (msg_id)`)
	pgExecLog(db, "ai_feedback_idx_chat", `CREATE INDEX IF NOT EXISTS idx_ai_feedback_chat    ON ai_feedback (chat, timestamp DESC)`)
	pgExecLog(db, "ai_feedback_idx_rating", `CREATE INDEX IF NOT EXISTS idx_ai_feedback_rating  ON ai_feedback (rating) WHERE rating = 'down'`)

	// ─── AI call config ─────────────────────────────────────────
	pgExecLog(db, "ai_call_config", `CREATE TABLE IF NOT EXISTS ai_call_config (
                group_jid   TEXT PRIMARY KEY,
                enabled     BOOLEAN NOT NULL DEFAULT FALSE,
                voice       TEXT DEFAULT '',
                model       TEXT DEFAULT '',
                extra       TEXT NOT NULL DEFAULT '{}',
                updated_at  TIMESTAMP NOT NULL DEFAULT (datetime('now'))
        )`)

	// ─── Audit log ──────────────────────────────────────────────
	pgExecLog(db, "audit_log", `CREATE TABLE IF NOT EXISTS audit_log (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                ts TIMESTAMP NOT NULL DEFAULT (datetime('now')),
                jid TEXT,
                action TEXT NOT NULL,
                details TEXT
        )`)
	pgExecLog(db, "audit_log_idx_ts", `CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log (ts)`)

	// ─── Deleted log ────────────────────────────────────────────
	pgExecLog(db, "deleted_log", `CREATE TABLE IF NOT EXISTS deleted_log (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                msg_id TEXT NOT NULL,
                chat TEXT NOT NULL,
                sender TEXT,
                timestamp BIGINT,
                reason TEXT,
                deleted_at TIMESTAMP NOT NULL DEFAULT (datetime('now'))
        )`)
	pgExecLog(db, "deleted_log_idx_chat", `CREATE INDEX IF NOT EXISTS idx_deleted_chat ON deleted_log (chat)`)
}
