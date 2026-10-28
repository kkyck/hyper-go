package main

// pg.go — Storage layer: PostgreSQL (default) or SQLite, one shared API.
// ════════════════════════════════════════════════════════════
//
// Every plugin stores its data through the four helpers below:
//
//	pgGet()                      → shared *sql.DB for the active backend
//	pgExec(ctx, db, q, args...)  → exec a statement
//	pgQuery(ctx, db, q, args...) → run a query
//	pgQueryRow(ctx, db, q, ...)  → run a single-row query
//
// The SQL written by plugins is dialect-neutral: `?` placeholders, no casts.
// The wrappers translate it to the active backend:
//
//	postgres → ? becomes $1, $2, ... (lib/pq only understands $N)
//	sqlite   → ? is native; PostgreSQL-only constructs are rewritten
//	            (see plugins/pg_sqlite.go for the transpiler + DDL)
//
// Backend selection lives in core/storage.go (STORAGE / POSTGRES_DSN /
// SQLITE_DB). The whatsmeow session store uses the same database, so the
// session tables and the bot tables always live side by side.
//
// Tables created by pgInitAll():
//   messages, nodes        — CRM storage
//   conversations, thoughts — AI conversation memory
//   message_archive         — full message envelope archive
//   kv_store, group_settings — JSON state / per-group plugin config
//   ai_feedback, ai_call_config, audit_log, deleted_log

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	_ "modernc.org/sqlite"
)

var (
	storeMu   sync.Mutex
	storeConn *sql.DB
)

// pgGet returns the shared database handle for the active backend.
// Singleton with double-check locking; ping timeout 10s to avoid blocking
// forever when the database is unreachable.
func pgGet() (*sql.DB, error) {
	storeMu.Lock()
	if storeConn != nil {
		db := storeConn
		storeMu.Unlock()
		return db, nil
	}
	storeMu.Unlock()

	var (
		db  *sql.DB
		err error
	)
	if storageIsSQLite() {
		db, err = openSQLiteStore()
	} else {
		db, err = openPostgresStore()
	}
	if err != nil {
		return nil, err
	}

	storeMu.Lock()
	defer storeMu.Unlock()
	if storeConn != nil {
		_ = db.Close()
		return storeConn, nil
	}
	storeConn = db
	return db, nil
}

// openPostgresStore opens (and verifies) the PostgreSQL connection.
// POSTGRES_DSN is mandatory — there is no built-in fallback credential.
func openPostgresStore() (*sql.DB, error) {
	dsn := postgresDSN()
	if dsn == "" {
		return nil, fmt.Errorf("POSTGRES_DSN is not set (set it, or use STORAGE=sqlite)")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("pg open: %w", err)
	}

	db.SetMaxOpenConns(15)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(1 * time.Minute)

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pingErr := db.PingContext(pingCtx)
	pingCancel()
	if pingErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pg ping: %w", pingErr)
	}

	fmt.Println("[PG] Connected to PostgreSQL")
	return db, nil
}

// openSQLiteStore opens the single-file SQLite database used by the bot and
// the whatsmeow session store.
//
// Pragmas:
//
//	foreign_keys(1)   — honour FK constraints
//	journal_mode(WAL) — readers never block the writer
//	busy_timeout      — wait instead of failing with SQLITE_BUSY
//	_time_format      — write timestamps as "YYYY-MM-DD HH:MM:SS" so they can
//	                    be read back into time.Time
func openSQLiteStore() (*sql.DB, error) {
	path := sqlitePath()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}

	addr := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-1000)&_time_format=datetime&_texttotime=1", path)

	db, err := sql.Open("sqlite", addr)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}

	// SQLite allows a single writer. A small pool plus WAL keeps concurrent
	// reads cheap while the busy timeout absorbs write contention.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0)

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pingErr := db.PingContext(pingCtx)
	pingCancel()
	if pingErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite ping: %w", pingErr)
	}

	fmt.Printf("[SQLITE] Connected to %s\n", path)
	return db, nil
}

// pgRewritePlaceholders converts MySQL/SQLite-style ? placeholders to
// PostgreSQL-style $1, $2, ... Skips ? inside string literals.
func pgRewritePlaceholders(query string) string {
	n := 0
	var b strings.Builder
	b.Grow(len(query) + 16)
	inString := false
	var stringChar byte
	for i := 0; i < len(query); i++ {
		c := query[i]
		if inString {
			if c == stringChar {
				inString = false
			}
			b.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			inString = true
			stringChar = c
			b.WriteByte(c)
			continue
		}
		if c == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// storeSQL adapts a dialect-neutral statement to the active backend.
func storeSQL(query string) string {
	if storageIsSQLite() {
		return sqliteTranspile(query)
	}
	return pgRewritePlaceholders(query)
}

// pgExec wraps db.ExecContext with backend-specific SQL translation.
func pgExec(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
	return db.ExecContext(ctx, storeSQL(query), args...)
}

// pgQuery wraps db.QueryContext with backend-specific SQL translation.
func pgQuery(ctx context.Context, db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	return db.QueryContext(ctx, storeSQL(query), args...)
}

// pgQueryRow wraps db.QueryRowContext with backend-specific SQL translation.
func pgQueryRow(ctx context.Context, db *sql.DB, query string, args ...any) *sql.Row {
	return db.QueryRowContext(ctx, storeSQL(query), args...)
}

// pgExecLog runs DDL and logs the error (no panic on failure).
func pgExecLog(db *sql.DB, name, query string) {
	if _, err := db.Exec(query); err != nil {
		fmt.Printf("[STORAGE] init table %s error: %v\n", name, err)
	}
}

var (
	pgInitMu       sync.Mutex
	pgInitDone     bool
	pgRetryStarted bool
)

// pgInitAll creates all required tables on the active backend.
// Safe to call more than once — every statement is IF NOT EXISTS.
func pgInitAll() {
	pgInitMu.Lock()
	if pgInitDone {
		pgInitMu.Unlock()
		return
	}
	pgInitMu.Unlock()

	db, err := pgGet()
	if err != nil {
		fmt.Printf("[STORAGE] init error: %v\n", err)
		pgInitMu.Lock()
		if !pgRetryStarted {
			pgRetryStarted = true
			go func() {
				for {
					time.Sleep(5 * time.Minute)
					pgInitMu.Lock()
					done := pgInitDone
					pgInitMu.Unlock()
					if done {
						return
					}
					pgInitAll()
				}
			}()
		}
		pgInitMu.Unlock()
		return
	}

	if storageIsSQLite() {
		pgInitAllSQLite(db)
	} else {
		pgInitAllPostgres(db)
	}

	fmt.Printf("[STORAGE] All tables created/verified (%s)\n", storageMode())
	pgInitMu.Lock()
	pgInitDone = true
	pgInitMu.Unlock()
}

// pgInitSessionSchema patches the whatsmeow session tables with the extra
// columns the bot relies on.
//
// It MUST run after the session store has been initialised, because the
// tables are created by whatsmeow's own schema upgrade:
//
//	whatsmeow_device.platform        — connection mode (chrome/android)
//	whatsmeow_sender_keys.created_at  — sender-key age metrics
func pgInitSessionSchema() {
	db, err := pgGet()
	if err != nil {
		fmt.Printf("[STORAGE] session schema patch skipped: %v\n", err)
		return
	}

	if storageIsSQLite() {
		// SQLite has no "ADD COLUMN IF NOT EXISTS" — check first.
		if !sqliteHasColumn(db, "whatsmeow_device", "platform") {
			pgExecLog(db, "whatsmeow_device.platform", `ALTER TABLE whatsmeow_device ADD COLUMN platform TEXT DEFAULT ''`)
		}
		if !sqliteHasColumn(db, "whatsmeow_sender_keys", "created_at") {
			pgExecLog(db, "whatsmeow_sender_keys.created_at", `ALTER TABLE whatsmeow_sender_keys ADD COLUMN created_at TIMESTAMP`)
		}
		return
	}

	pgExecLog(db, "whatsmeow_device.platform", `ALTER TABLE whatsmeow_device ADD COLUMN IF NOT EXISTS platform TEXT DEFAULT ''`)
	pgExecLog(db, "whatsmeow_sender_keys.created_at", `ALTER TABLE whatsmeow_sender_keys ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ`)
}

// init — ensure pq is imported (registered as "postgres" driver for
// database/sql). whatsmeow's sqlstore.PostgresArrayWrapper is set in
// core/session_sqlite.go.
func init() {
	_ = pq.Array // ensure pq is referenced (driver registered via blank import)
}
