package main

// pg_postgres.go — PostgreSQL schema.
// ════════════════════════════════════════════════════════════
//
// The PostgreSQL counterpart of pg_sqlite.go. Both create the same logical
// schema so a single set of queries works on either backend.
//
// The whatsmeow session tables (whatsmeow_*) are NOT created here — they are
// owned by the session store's own schema upgrade. pgInitSessionSchema()
// only adds the two extra columns the bot reads.

import (
	"database/sql"
	"fmt"
)

// pgInitAllPostgres creates all required tables on PostgreSQL.
func pgInitAllPostgres(db *sql.DB) {
	// ─── CRM tables ─────────────────────────────────────────────
	pgExecLog(db, "messages", `CREATE TABLE IF NOT EXISTS messages (
                rowid BIGSERIAL PRIMARY KEY,
                id TEXT NOT NULL,
                chat TEXT NOT NULL,
                sender TEXT,
                timestamp BIGINT,
                data BYTEA
        )`)
	pgExecLog(db, "messages_idx", `CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages (chat)`)
	pgExecLog(db, "messages_idx_ts", `CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages (timestamp)`)

	pgExecLog(db, "nodes", `CREATE TABLE IF NOT EXISTS nodes (
                id TEXT NOT NULL,
                chat TEXT NOT NULL,
                data TEXT NOT NULL,
                created_at BIGINT DEFAULT (EXTRACT(EPOCH FROM NOW())::BIGINT),
                PRIMARY KEY(id, chat)
        )`)
	pgExecLog(db, "nodes_idx", `CREATE INDEX IF NOT EXISTS idx_nodes_chat ON nodes (chat)`)

	// ─── AI conversation memory ─────────────────────────────────
	pgExecLog(db, "conversations", `CREATE TABLE IF NOT EXISTS conversations (
                id BIGSERIAL PRIMARY KEY,
                chat_jid TEXT NOT NULL,
                role TEXT NOT NULL,
                content TEXT NOT NULL,
                sender_name TEXT DEFAULT '',
                timestamp BIGINT NOT NULL
        )`)
	pgExecLog(db, "conversations_idx", `CREATE INDEX IF NOT EXISTS idx_conversations_chat ON conversations (chat_jid, timestamp DESC)`)

	pgExecLog(db, "thoughts", `CREATE TABLE IF NOT EXISTS thoughts (
                id BIGSERIAL PRIMARY KEY,
                chat_jid TEXT NOT NULL,
                thought TEXT NOT NULL,
                timestamp BIGINT NOT NULL
        )`)
	pgExecLog(db, "thoughts_idx", `CREATE INDEX IF NOT EXISTS idx_thoughts_chat ON thoughts (chat_jid, timestamp DESC)`)

	// ─── Message archive ────────────────────────────────────────
	pgExecLog(db, "message_archive_v2_drop", `DROP TABLE IF EXISTS message_archive`)
	pgExecLog(db, "message_archive_v2", `CREATE TABLE IF NOT EXISTS message_archive (
                id                          BIGSERIAL PRIMARY KEY,
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
                retry_count                 INT DEFAULT 0,
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
                node_attrs                  JSONB DEFAULT '{}'::jsonb,
                envelope                    JSONB DEFAULT '{}'::jsonb,
                raw_message                 JSONB DEFAULT '{}'::jsonb,
                raw_message_unwrapped       JSONB DEFAULT '{}'::jsonb,
                source_webmsg               JSONB DEFAULT '{}'::jsonb,
                newsletter_meta             JSONB DEFAULT '{}'::jsonb,
                additional_attrs            JSONB DEFAULT '{}'::jsonb,
                additional_nodes            JSONB DEFAULT '{}'::jsonb,
                created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
        )`)
	pgExecLog(db, "message_archive_idx_group", `CREATE INDEX IF NOT EXISTS idx_archive_group       ON message_archive (group_jid)`)
	pgExecLog(db, "message_archive_idx_sender", `CREATE INDEX IF NOT EXISTS idx_archive_sender     ON message_archive (sender)`)
	pgExecLog(db, "message_archive_idx_msgid", `CREATE INDEX IF NOT EXISTS idx_archive_msgid      ON message_archive (msg_id)`)
	pgExecLog(db, "message_archive_idx_ts", `CREATE INDEX IF NOT EXISTS idx_archive_ts         ON message_archive ("timestamp" DESC)`)
	pgExecLog(db, "message_archive_idx_group_ts", `CREATE INDEX IF NOT EXISTS idx_archive_group_ts   ON message_archive (group_jid, "timestamp" DESC)`)
	pgExecLog(db, "message_archive_idx_sender_ts", `CREATE INDEX IF NOT EXISTS idx_archive_sender_ts  ON message_archive (sender, "timestamp" DESC)`)
	pgExecLog(db, "message_archive_idx_quoted", `CREATE INDEX IF NOT EXISTS idx_archive_quoted     ON message_archive (quoted_msg_id) WHERE quoted_msg_id <> ''`)
	pgExecLog(db, "message_archive_idx_type", `CREATE INDEX IF NOT EXISTS idx_archive_type       ON message_archive (type)`)
	pgExecLog(db, "message_archive_idx_broadcast", `CREATE INDEX IF NOT EXISTS idx_archive_broadcast  ON message_archive (broadcast) WHERE broadcast = true`)

	// ─── KV store ───────────────────────────────────────────────
	pgExecLog(db, "kv_store", `CREATE TABLE IF NOT EXISTS kv_store (
                key TEXT PRIMARY KEY,
                value JSONB NOT NULL,
                updated_at BIGINT DEFAULT (EXTRACT(EPOCH FROM NOW())::BIGINT)
        )`)

	// ─── Per-group plugin settings ──────────────────────────────
	pgExecLog(db, "group_settings", `CREATE TABLE IF NOT EXISTS group_settings (
                group_jid  TEXT NOT NULL,
                plugin     TEXT NOT NULL,
                enabled    BOOLEAN NOT NULL DEFAULT FALSE,
                config     JSONB NOT NULL DEFAULT '{}'::jsonb,
                updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                PRIMARY KEY (group_jid, plugin)
        )`)
	pgExecLog(db, "group_settings_idx_plugin", `CREATE INDEX IF NOT EXISTS idx_group_settings_plugin  ON group_settings (plugin)`)
	pgExecLog(db, "group_settings_idx_enabled", `CREATE INDEX IF NOT EXISTS idx_group_settings_enabled ON group_settings (plugin) WHERE enabled = true`)
	pgExecLog(db, "group_settings_idx_jid", `CREATE INDEX IF NOT EXISTS idx_group_settings_jid     ON group_settings (group_jid)`)

	// ─── AI feedback ────────────────────────────────────────────
	pgExecLog(db, "ai_feedback", `CREATE TABLE IF NOT EXISTS ai_feedback (
                id           BIGSERIAL PRIMARY KEY,
                msg_id       TEXT NOT NULL,
                chat         TEXT NOT NULL,
                sender       TEXT,
                rating       TEXT NOT NULL,
                ai_text      TEXT DEFAULT '',
                user_feedback TEXT DEFAULT '',
                timestamp   BIGINT NOT NULL,
                created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
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
                extra       JSONB NOT NULL DEFAULT '{}'::jsonb,
                updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
        )`)

	// ─── Audit log ──────────────────────────────────────────────
	pgExecLog(db, "audit_log", `CREATE TABLE IF NOT EXISTS audit_log (
                id BIGSERIAL PRIMARY KEY,
                ts TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                jid TEXT,
                action TEXT NOT NULL,
                details JSONB
        )`)
	pgExecLog(db, "audit_log_idx_ts", `CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log (ts)`)

	// ─── Deleted log ────────────────────────────────────────────
	pgExecLog(db, "deleted_log", `CREATE TABLE IF NOT EXISTS deleted_log (
                id BIGSERIAL PRIMARY KEY,
                msg_id TEXT NOT NULL,
                chat TEXT NOT NULL,
                sender TEXT,
                timestamp BIGINT,
                reason TEXT,
                deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
        )`)
	pgExecLog(db, "deleted_log_idx_chat", `CREATE INDEX IF NOT EXISTS idx_deleted_chat ON deleted_log (chat)`)

	fmt.Println("[PG] Schema verified")
}
