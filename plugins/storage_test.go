package main

// storage_test.go — Storage backend tests (transpiler + SQLite schema).
// ════════════════════════════════════════════════════════════
//
// The bot writes its SQL once, in a dialect-neutral style. These tests pin
// down the SQLite translation and prove the resulting statements actually
// run against a real SQLite database.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/polymorfa/hypermeow/store/sqlstore"
	waLog "github.com/polymorfa/hypermeow/util/log"
)

// resetStoreForTest drops the shared database handle so a test can point the
// storage layer at its own file.
func resetStoreForTest(t *testing.T) {
	t.Helper()
	closeStore := func() {
		storeMu.Lock()
		defer storeMu.Unlock()
		if storeConn != nil {
			_ = storeConn.Close()
			storeConn = nil
		}
	}
	closeStore()
	t.Cleanup(closeStore)
}

// newTestSQLite opens a throwaway SQLite database with the same pragmas the
// bot uses.
func newTestSQLite(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bot.db")
	addr := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_time_format=datetime&_texttotime=1", path)
	db, err := sql.Open("sqlite", addr)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// execT runs a statement through the SQLite transpiler.
func execT(t *testing.T, db *sql.DB, q string, args ...any) sql.Result {
	t.Helper()
	res, err := db.ExecContext(context.Background(), sqliteTranspile(q), args...)
	if err != nil {
		t.Fatalf("exec failed: %v\nSQL: %s", err, sqliteTranspile(q))
	}
	return res
}

func TestSQLiteTranspile(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no change",
			in:   "SELECT id FROM messages WHERE chat = ? LIMIT ?",
			want: "SELECT id FROM messages WHERE chat = ? LIMIT ?",
		},
		{
			name: "text cast",
			in:   "SELECT COALESCE(config::text,'{}') FROM group_settings",
			want: "SELECT COALESCE(CAST(config AS TEXT),'{}') FROM group_settings",
		},
		{
			name: "jsonb parameter cast",
			in:   "INSERT INTO kv_store (key, value) VALUES (?, ?::jsonb)",
			want: "INSERT INTO kv_store (key, value) VALUES (?, CAST(? AS TEXT))",
		},
		{
			name: "now",
			in:   "INSERT INTO audit_log (ts, action) VALUES (NOW(), 'x')",
			want: "INSERT INTO audit_log (ts, action) VALUES (datetime('now'), 'x')",
		},
		{
			name: "interval",
			in:   "SELECT COUNT(*) FROM audit_log WHERE ts >= NOW() - INTERVAL '24 hours'",
			want: "SELECT COUNT(*) FROM audit_log WHERE ts >= datetime('now','-24 hours')",
		},
		{
			name: "epoch",
			in:   "INSERT INTO kv_store (key, value, updated_at) VALUES (?, ?::jsonb, EXTRACT(EPOCH FROM NOW())::BIGINT)",
			want: "INSERT INTO kv_store (key, value, updated_at) VALUES (?, CAST(? AS TEXT), CAST(strftime('%s','now') AS INTEGER))",
		},
		{
			name: "float cast on json path",
			in:   "SELECT COALESCE(AVG((details->>'latency_ms')::float8), 0) FROM audit_log",
			want: "SELECT COALESCE(AVG(CAST((details->>'latency_ms') AS REAL)), 0) FROM audit_log",
		},
		{
			name: "bigint cast on json path",
			in:   "DELETE FROM kv_store WHERE key LIKE ? AND (value->>'forwarded_at')::BIGINT < ?",
			want: "DELETE FROM kv_store WHERE key LIKE ? AND CAST((value->>'forwarded_at') AS INTEGER) < ?",
		},
		{
			name: "timestamptz literal",
			in:   "SELECT COALESCE(MAX(created_at), 'epoch'::timestamptz) FROM whatsmeow_sender_keys",
			want: "SELECT COALESCE(MAX(created_at), CAST('epoch' AS TEXT)) FROM whatsmeow_sender_keys",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqliteTranspile(tc.in)
			if got != tc.want {
				t.Fatalf("transpile mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestSQLiteSchemaCreatesAllTables(t *testing.T) {
	db := newTestSQLite(t)
	pgInitAllSQLite(db)

	want := []string{
		"messages", "nodes", "conversations", "thoughts", "message_archive",
		"kv_store", "group_settings", "ai_feedback", "ai_call_config",
		"audit_log", "deleted_log",
	}
	for _, table := range want {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestSQLiteKVStoreRoundTrip(t *testing.T) {
	db := newTestSQLite(t)
	pgInitAllSQLite(db)
	ctx := context.Background()

	// INSERT with a JSONB cast + epoch default + upsert.
	execT(t, db,
		`INSERT INTO kv_store (key, value, updated_at) VALUES (?, ?::jsonb, EXTRACT(EPOCH FROM NOW())::BIGINT)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		"demo", `{"a":1,"forwarded_at":100}`)

	var value string
	var updated int64
	if err := db.QueryRowContext(ctx, sqliteTranspile(`SELECT value::text, updated_at FROM kv_store WHERE key = ?`), "demo").
		Scan(&value, &updated); err != nil {
		t.Fatalf("select kv_store: %v", err)
	}
	if value != `{"a":1,"forwarded_at":100}` {
		t.Fatalf("unexpected value: %q", value)
	}
	if updated <= 0 {
		t.Fatalf("updated_at not set: %d", updated)
	}

	// The tag-forward cleanup query (JSON path + bigint cast).
	execT(t, db,
		`DELETE FROM kv_store WHERE key LIKE ? AND (value->>'forwarded_at')::BIGINT < ?`,
		"tag_forward:%", 50)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kv_store`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the row to survive (forwarded_at=100 >= 50), got %d rows", n)
	}
}

func TestSQLiteGroupSettingsUpsert(t *testing.T) {
	db := newTestSQLite(t)
	pgInitAllSQLite(db)
	ctx := context.Background()

	execT(t, db,
		`INSERT INTO group_settings (group_jid, plugin, enabled, config) VALUES (?, ?, ?, ?::jsonb)
		 ON CONFLICT (group_jid, plugin) DO UPDATE SET enabled = EXCLUDED.enabled, config = EXCLUDED.config, updated_at = NOW()`,
		"123@g.us", "autodel", true, `{"mode":"hard"}`)

	execT(t, db,
		`INSERT INTO group_settings (group_jid, plugin, enabled, config) VALUES (?, ?, ?, ?::jsonb)
		 ON CONFLICT (group_jid, plugin) DO UPDATE SET enabled = EXCLUDED.enabled, config = EXCLUDED.config, updated_at = NOW()`,
		"123@g.us", "autodel", false, `{"mode":"soft"}`)

	var enabled bool
	var cfg string
	if err := db.QueryRowContext(ctx,
		sqliteTranspile(`SELECT enabled, COALESCE(config::text,'{}') FROM group_settings WHERE group_jid = ? AND plugin = ?`),
		"123@g.us", "autodel").Scan(&enabled, &cfg); err != nil {
		t.Fatalf("select group_settings: %v", err)
	}
	if enabled {
		t.Fatal("expected enabled=false after the upsert")
	}
	if cfg != `{"mode":"soft"}` {
		t.Fatalf("unexpected config: %q", cfg)
	}
}

func TestSQLiteAuditLogAndPercentile(t *testing.T) {
	t.Setenv("STORAGE", "sqlite")
	db := newTestSQLite(t)
	pgInitAllSQLite(db)
	ctx := context.Background()

	for i, ms := range []int{10, 20, 30, 40, 50} {
		execT(t, db,
			`INSERT INTO audit_log (ts, jid, action, details) VALUES (NOW(), ?, ?, ?::jsonb)`,
			"123@g.us", "spr_send", fmt.Sprintf(`{"status":"ok","latency_ms":%d,"source":"test"}`, ms))
		_ = i
	}

	var total int
	if err := db.QueryRowContext(ctx, sqliteTranspile(`SELECT COUNT(*) FROM audit_log WHERE action='spr_send'`)).Scan(&total); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if total != 5 {
		t.Fatalf("expected 5 audit rows, got %d", total)
	}

	// JSON path filter + 24h window.
	var ok int
	if err := db.QueryRowContext(ctx, sqliteTranspile(
		`SELECT COUNT(*) FROM audit_log WHERE action='spr_send' AND details->>'status' = 'ok' AND ts >= NOW() - INTERVAL '24 hours'`)).Scan(&ok); err != nil {
		t.Fatalf("count ok: %v", err)
	}
	if ok != 5 {
		t.Fatalf("expected 5 ok rows, got %d", ok)
	}

	// Error-class grouping via a JSON path.
	execT(t, db, `INSERT INTO audit_log (ts, jid, action, details) VALUES (NOW(), ?, ?, ?::jsonb)`,
		"123@g.us", "spr_send", `{"status":"fail","error_class":"network"}`)

	rows, err := db.QueryContext(ctx, sqliteTranspile(
		`SELECT COALESCE(details->>'error_class','unknown') AS cls, COUNT(*) AS c
		 FROM audit_log WHERE action='spr_send' AND details->>'status' = 'fail' GROUP BY cls ORDER BY c DESC`))
	if err != nil {
		t.Fatalf("error class query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected one error-class row")
	}
	var cls string
	var c int
	if err := rows.Scan(&cls, &c); err != nil {
		t.Fatalf("scan error class: %v", err)
	}
	if cls != "network" || c != 1 {
		t.Fatalf("unexpected error class row: %s=%d", cls, c)
	}

	// Percentile (SQLite variant).
	var p50 float64
	if err := db.QueryRowContext(ctx, sprPercentileSQL(0.5)).Scan(&p50); err != nil {
		t.Fatalf("percentile: %v", err)
	}
	if p50 < 1 {
		t.Fatalf("unexpected p50: %v", p50)
	}
}

func TestSQLiteMessageArchiveAndMessages(t *testing.T) {
	db := newTestSQLite(t)
	pgInitAllSQLite(db)
	ctx := context.Background()

	// message_archive insert (the compat_owner shape: JSONB columns).
	execT(t, db,
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
		"MSG1", "123@g.us", "456@s.whatsapp.net", "456", "s.whatsapp.net",
		"Alice", false, 1700000000, true,
		"", "", "",
		false, false, false, false,
		false, false, false, false,
		false, 0, "",
		"text", "hello", "", "", "", "", "", 0,
		"", "", "", "",
		"",
		"", `{"a":1}`,
		`{"msg_id":"MSG1"}`, `{}`, `{}`, `{}`, `{}`)

	var envelope string
	if err := db.QueryRowContext(ctx,
		sqliteTranspile(`SELECT COALESCE(envelope::text,'{}') FROM message_archive WHERE msg_id = ? ORDER BY id DESC LIMIT 1`),
		"MSG1").Scan(&envelope); err != nil {
		t.Fatalf("select envelope: %v", err)
	}
	if envelope != `{"msg_id":"MSG1"}` {
		t.Fatalf("unexpected envelope: %q", envelope)
	}

	// created_at is a TIMESTAMP column: it must read back as time.Time.
	var created time.Time
	if err := db.QueryRowContext(ctx, `SELECT created_at FROM message_archive WHERE msg_id = ?`, "MSG1").Scan(&created); err != nil {
		t.Fatalf("scan created_at: %v", err)
	}
	if created.IsZero() {
		t.Fatal("created_at should not be zero")
	}

	// messages table: rowid ordering must work.
	for i := 0; i < 3; i++ {
		execT(t, db, `INSERT INTO messages (id, chat, sender, timestamp, data) VALUES (?, ?, ?, ?, ?)`,
			fmt.Sprintf("M%d", i), "123@g.us", "456@s.whatsapp.net", 1700000000+int64(i), []byte{0x01, 0x02})
	}
	rows, err := db.QueryContext(ctx, sqliteTranspile(
		`SELECT rowid, id, chat, sender, timestamp, data FROM messages WHERE chat = ? ORDER BY rowid DESC LIMIT ?`),
		"123@g.us", 2)
	if err != nil {
		t.Fatalf("select messages: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var rowid int64
		var id, chat, sender string
		var ts int64
		var data []byte
		if err := rows.Scan(&rowid, &id, &chat, &sender, &ts, &data); err != nil {
			t.Fatalf("scan messages: %v", err)
		}
		if len(data) != 2 {
			t.Fatalf("unexpected blob length: %d", len(data))
		}
		got = append(got, id)
	}
	if len(got) != 2 || got[0] != "M2" || got[1] != "M1" {
		t.Fatalf("unexpected rowid ordering: %v", got)
	}
}

func TestSQLiteSessionSchemaPatch(t *testing.T) {
	db := newTestSQLite(t)

	// Minimal stand-in for the session store tables.
	execT(t, db, `CREATE TABLE IF NOT EXISTS whatsmeow_device (jid TEXT PRIMARY KEY)`)
	execT(t, db, `CREATE TABLE IF NOT EXISTS whatsmeow_sender_keys (our_jid TEXT, chat_id TEXT, sender_id TEXT)`)

	if sqliteHasColumn(db, "whatsmeow_device", "platform") {
		t.Fatal("whatsmeow_device should not have a platform column yet")
	}
	execT(t, db, `ALTER TABLE whatsmeow_device ADD COLUMN platform TEXT DEFAULT ''`)
	if !sqliteHasColumn(db, "whatsmeow_device", "platform") {
		t.Fatal("platform column was not added")
	}
	if sqliteHasColumn(db, "whatsmeow_sender_keys", "created_at") {
		t.Fatal("whatsmeow_sender_keys should not have a created_at column yet")
	}
	execT(t, db, `ALTER TABLE whatsmeow_sender_keys ADD COLUMN created_at TIMESTAMP`)
	if !sqliteHasColumn(db, "whatsmeow_sender_keys", "created_at") {
		t.Fatal("created_at column was not added")
	}
}

func TestSQLiteSessionStoreSharesOneDatabase(t *testing.T) {
	// The whatsmeow session store and the bot tables must live in the same
	// SQLite file, otherwise sender-key metrics would always be empty.
	path := filepath.Join(t.TempDir(), "bot.db")
	t.Setenv("STORAGE", "sqlite")
	t.Setenv("SQLITE_DB", path)
	resetStoreForTest(t)

	addr := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-1000)&_time_format=datetime&_texttotime=1", path)
	container, err := sqlstore.New(context.Background(), "sqlite", addr, waLog.Stdout("Test", "INFO", true))
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	defer container.Close()
	if err := container.Upgrade(context.Background()); err != nil {
		t.Fatalf("session store upgrade: %v", err)
	}

	db, err := pgGet()
	if err != nil {
		t.Fatalf("pgGet: %v", err)
	}
	for _, table := range []string{"whatsmeow_device", "whatsmeow_sender_keys", "whatsmeow_sessions"} {
		var n int
		if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='%s'", table)).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s is missing from the shared database", table)
		}
	}

	// The bot's own tables and the extra session columns must be addable to
	// the very same file.
	pgInitAllSQLite(db)
	pgInitSessionSchema()

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='kv_store'").Scan(&n); err != nil {
		t.Fatalf("query kv_store: %v", err)
	}
	if n != 1 {
		t.Fatal("kv_store is missing from the shared database")
	}
	if !sqliteHasColumn(db, "whatsmeow_device", "platform") {
		t.Fatal("whatsmeow_device.platform was not added")
	}
	if !sqliteHasColumn(db, "whatsmeow_sender_keys", "created_at") {
		t.Fatal("whatsmeow_sender_keys.created_at was not added")
	}
}

func TestStorageModeSelection(t *testing.T) {
	t.Setenv("STORAGE", "")
	t.Setenv("POSTGRES_DSN", "")
	if storageMode() != storageSQLite {
		t.Fatalf("auto without DSN should select sqlite, got %s", storageMode())
	}

	t.Setenv("POSTGRES_DSN", "postgres://user:pass@localhost:5432/db")
	if storageMode() != storagePostgres {
		t.Fatalf("auto with DSN should select postgres, got %s", storageMode())
	}

	t.Setenv("STORAGE", "sqlite")
	if storageMode() != storageSQLite {
		t.Fatalf("explicit sqlite should win, got %s", storageMode())
	}

	t.Setenv("STORAGE", "postgres")
	if storageMode() != storagePostgres {
		t.Fatalf("explicit postgres should win, got %s", storageMode())
	}
}

func TestSQLitePathResolution(t *testing.T) {
	t.Setenv("SQLITE_DB", "")
	t.Setenv("SESSION_DB", "")
	if sqlitePath() != "./data/bot.db" {
		t.Fatalf("unexpected default sqlite path: %s", sqlitePath())
	}
	t.Setenv("SESSION_DB", "/tmp/legacy.db")
	if sqlitePath() != "/tmp/legacy.db" {
		t.Fatalf("SESSION_DB should be honoured, got %s", sqlitePath())
	}
	t.Setenv("SQLITE_DB", "/tmp/primary.db")
	if sqlitePath() != "/tmp/primary.db" {
		t.Fatalf("SQLITE_DB should win, got %s", sqlitePath())
	}
}

func TestPostgresRequiresDSN(t *testing.T) {
	t.Setenv("STORAGE", "postgres")
	t.Setenv("POSTGRES_DSN", "")

	// There must be no built-in fallback credential: without a DSN the
	// backend fails fast instead of trying to reach some default server.
	if _, err := openPostgresStore(); err == nil {
		t.Fatal("openPostgresStore must fail when POSTGRES_DSN is empty")
	}
}
