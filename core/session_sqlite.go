package main

/**
 * Session Store — PostgreSQL OR SQLite
 * ════════════════════════════════════════════════════════════
 *
 * The backend is chosen by core/storage.go:
 *   STORAGE=postgres → PostgreSQL (requires POSTGRES_DSN)
 *   STORAGE=sqlite  → SQLite file
 *   STORAGE=auto    → PostgreSQL when POSTGRES_DSN is set, else SQLite
 *
 * The session store and the bot's own tables live in the SAME database, so
 * sender keys / device state stay consistent with the bot's metrics.
 *
 * PostgreSQL session migration:
 *   - On first run with POSTGRES_DSN set, whatsmeow auto-creates schema in PG
 *     via container.Upgrade()
 *   - Existing sessions in SQLite are NOT auto-migrated — copy the
 *     prekeys/sessions/identities tables over manually if you switch backend
 *   - If migration skipped, bot will need re-pairing (QR / pairing code)
 *
 * Note: whatsmeow's sqlstore.PostgresArrayWrapper must be set to pq.Array
 * when using lib/pq driver for PG arrays (e.g., for prekey storage).
 */

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lib/pq"
	"github.com/polymorfa/hypermeow/store/sqlstore"
	waLog "github.com/polymorfa/hypermeow/util/log"

	_ "modernc.org/sqlite" // register "sqlite" driver
)

var sqliteStore *sqlstore.Container

// maskDSN hides password in DSN for safe logging.
// "postgres://user:pass@host:5432/db?..." → "postgres://user:***@host:5432/db?..."
func maskDSN(dsn string) string {
	// Find password part: between "://" + user ":" and "@"
	idx := strings.Index(dsn, "://")
	if idx < 0 {
		return dsn
	}
	rest := dsn[idx+3:]
	atIdx := strings.Index(rest, "@")
	if atIdx < 0 {
		return dsn
	}
	// rest[:atIdx] = "user:pass"
	colonIdx := strings.Index(rest[:atIdx], ":")
	if colonIdx < 0 {
		return dsn
	}
	return dsn[:idx+3] + rest[:colonIdx+1] + "***" + rest[atIdx:]
}

func initSQLiteStore(log waLog.Logger) (*sqlstore.Container, error) {
	// Priority 1: PostgreSQL
	if storageIsPostgres() {
		dsn := postgresDSN()
		if dsn == "" {
			return nil, fmt.Errorf("STORAGE=postgres requires POSTGRES_DSN to be set")
		}

		// Set whatsmeow PG array wrapper for lib/pq driver.
		// Required for array columns (prekeys, etc.) to work correctly.
		sqlstore.PostgresArrayWrapper = pq.Array

		fmt.Printf("🔌 Membuka PostgreSQL session: %s\n", maskDSN(dsn))
		container, err := sqlstore.New(context.Background(), "postgres", dsn, log)
		if err != nil {
			return nil, fmt.Errorf("pg open: %w", err)
		}
		if err := container.Upgrade(context.Background()); err != nil {
			fmt.Printf("⚠️  PostgreSQL schema upgrade: %v\n", err)
		}
		sqliteStore = container
		return container, nil
	}

	// Priority 2: SQLite (file-based)
	dbPath := sqlitePath()

	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}

	addr := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-1000)&_time_format=datetime&_texttotime=1", dbPath)

	fmt.Printf("🔌 Membuka SQLite session: %s\n", dbPath)
	container, err := sqlstore.New(context.Background(), "sqlite", addr, log)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}

	if err := container.Upgrade(context.Background()); err != nil {
		fmt.Printf("⚠️  SQLite upgrade: %v\n", err)
	}

	sqliteStore = container
	return container, nil
}
