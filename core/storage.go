package main

// storage.go — Storage backend selection (PostgreSQL or SQLite).
// ════════════════════════════════════════════════════════════
//
// The bot speaks ONE storage API (pgGet / pgExec / pgQuery / pgQueryRow)
// and the concrete backend is resolved once at startup:
//
//	STORAGE=postgres  → PostgreSQL   (requires POSTGRES_DSN)
//	STORAGE=sqlite   → SQLite file  (SQLITE_DB, zero-config default)
//	STORAGE=auto     → PostgreSQL when POSTGRES_DSN is set, else SQLite
//
// Both the whatsmeow session store and the bot's own tables live in the SAME
// database, so sender-key metrics, device state and audit data stay coherent
// on either backend.
//
// Nothing here is PostgreSQL-specific: plugins never branch on the backend,
// they only use the wrappers in plugins/pg.go which translate the
// dialect-neutral SQL (? placeholders) to the active backend.

import (
	"os"
	"strings"
)

const (
	storagePostgres = "postgres"
	storageSQLite   = "sqlite"
)

// storageMode resolves which backend is active.
//
// "auto" (or an empty/unknown value) picks PostgreSQL when POSTGRES_DSN is
// present and SQLite otherwise, so a fresh checkout runs without any
// database setup at all.
func storageMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE"))) {
	case storagePostgres, "pg", "postgresql":
		return storagePostgres
	case storageSQLite, "sqlite3", "file":
		return storageSQLite
	default:
		if strings.TrimSpace(os.Getenv("POSTGRES_DSN")) != "" {
			return storagePostgres
		}
		return storageSQLite
	}
}

// storageIsPostgres reports whether the PostgreSQL backend is active.
func storageIsPostgres() bool { return storageMode() == storagePostgres }

// storageIsSQLite reports whether the SQLite backend is active.
func storageIsSQLite() bool { return storageMode() == storageSQLite }

// postgresDSN returns the configured PostgreSQL DSN (may be empty).
func postgresDSN() string { return strings.TrimSpace(os.Getenv("POSTGRES_DSN")) }

// sqlitePath returns the SQLite file used for BOTH the whatsmeow session
// store and the bot's own tables.
//
//	SQLITE_DB   → primary override
//	SESSION_DB  → legacy override (kept for compatibility)
//	default     → ./data/bot.db
func sqlitePath() string {
	if p := strings.TrimSpace(os.Getenv("SQLITE_DB")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("SESSION_DB")); p != "" {
		return p
	}
	return "./data/bot.db"
}
