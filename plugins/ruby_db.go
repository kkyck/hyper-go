package main

// ruby_db.go — SQLite database for AI conversation persistence
// ════════════════════════════════════════════════════════════
//
// Stores conversation history + AI thinking logs in data/ruby.db
// so they survive bot restarts. Uses modernc.org/sqlite (pure Go, no CGO).
//
// Tables:
//   conversations — chat_jid, role, content, sender_name, timestamp
//   thoughts      — chat_jid, thought, timestamp
//
// Used by:
//   - aiAgenticProcess — loads recent turns before calling AI
//   - thinking tool    — stores AI's internal reasoning
//
// Connection: shared singleton, WAL mode, MaxOpenConns=1 (SQLite safe)

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

const rubyDBFile = "data/ruby.db"

var (
	rubyDBMu   sync.Mutex
	rubyDBConn *sql.DB
	rubyDBOnce sync.Once
)

// rubyDBGet opens a shared SQLite connection to data/ruby.db.
// Initializes tables on first call.
func rubyDBGet() (*sql.DB, error) {
	rubyDBMu.Lock()
	defer rubyDBMu.Unlock()

	if rubyDBConn != nil {
		return rubyDBConn, nil
	}
	db, err := pgGet()
	if err != nil {
		return nil, fmt.Errorf("open ruby.db: %w", err)
	}
	rubyDBConn = db
	return db, nil
}

// rubyDBInit creates tables if they don't exist. Called once on startup.
func rubyDBInit() {
	// Tables now created by pgInitAll() — no-op
}

// rubyDBAddMessage stores a conversation turn in the database.
// role = "user" or "assistant". senderName = display name (for user messages).
func rubyDBAddMessage(chatJID, role, content, senderName string) error {
	db, err := rubyDBGet()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = pgExec(ctx, db,
		`INSERT INTO conversations (chat_jid, role, content, sender_name, timestamp) VALUES (?, ?, ?, ?, ?)`,
		chatJID, role, content, senderName, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("insert conversation: %w", err)
	}

	// Trim old entries: keep last 100 per chat
	go func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		pgExec(ctx2, db,
			`DELETE FROM conversations WHERE chat_jid = ? AND id NOT IN (
				SELECT id FROM conversations WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT 100
			)`, chatJID, chatJID)
	}()

	return nil
}

// rubyDBGetRecent returns the last N conversation turns for a chat.
// Returns []ChatTurn (role + content), oldest first.
func rubyDBGetRecent(chatJID string, limit int) []ChatTurn {
	db, err := rubyDBGet()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := pgQuery(ctx, db,
		`SELECT role, content FROM conversations WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?`,
		chatJID, limit)
	if err != nil {
		fmt.Printf("[RUBY-DB] query error: %v\n", err)
		return nil
	}
	defer rows.Close()

	var turns []ChatTurn
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			continue
		}
		turns = append(turns, ChatTurn{Role: role, Content: content})
	}

	// Reverse to oldest-first
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns
}

// rubyDBClear clears all conversation history for a chat.
func rubyDBClear(chatJID string) error {
	db, err := rubyDBGet()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = pgExec(ctx, db, `DELETE FROM conversations WHERE chat_jid = ?`, chatJID)
	if err != nil {
		return fmt.Errorf("clear conversations: %w", err)
	}
	_, err = pgExec(ctx, db, `DELETE FROM thoughts WHERE chat_jid = ?`, chatJID)
	if err != nil {
		return fmt.Errorf("clear thoughts: %w", err)
	}
	return nil
}

// rubyDBAddThought stores an AI thinking log.
// Used by the 'thinking' tool — AI can reason internally before responding.
func rubyDBAddThought(chatJID, thought string) error {
	db, err := rubyDBGet()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = pgExec(ctx, db,
		`INSERT INTO thoughts (chat_jid, thought, timestamp) VALUES (?, ?, ?)`,
		chatJID, thought, time.Now().Unix())
	return err
}

// rubyDBGetThoughts returns recent AI thoughts for a chat.
func rubyDBGetThoughts(chatJID string, limit int) []string {
	db, err := rubyDBGet()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := pgQuery(ctx, db,
		`SELECT thought FROM thoughts WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?`,
		chatJID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var thoughts []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil {
			thoughts = append(thoughts, t)
		}
	}
	return thoughts
}

// rubyDBStats returns conversation + thought counts.
func rubyDBStats() (conversations int, thoughts int) {
	db, err := rubyDBGet()
	if err != nil {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pgQueryRow(ctx, db, `SELECT COUNT(*) FROM conversations`).Scan(&conversations)
	pgQueryRow(ctx, db, `SELECT COUNT(*) FROM thoughts`).Scan(&thoughts)
	return
}
