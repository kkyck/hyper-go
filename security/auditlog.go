package main

/**
 * 📝 auditlog.go — Structured audit log for sensitive owner commands
 * ════════════════════════════════════════════════════════════
 * 2026-10-22: migrated from ./data/audit.jsonl (filesystem) to PostgreSQL
 * audit_log table. PG writes are ASYNC (go func) to avoid blocking message
 * processing (latency regression fix).
 */

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow/types/events"
)

type AuditEntry struct {
	Timestamp  string `json:"ts"`
	Actor      string `json:"actor"`
	ActorPhone string `json:"actor_phone,omitempty"`
	Chat       string `json:"chat,omitempty"`
	Cmd        string `json:"cmd"`
	Args       string `json:"args,omitempty"`
	Status     string `json:"status"` // ok | denied | error
	DurMs      int64  `json:"dur_ms,omitempty"`
	Note       string `json:"note,omitempty"`
}

// auditLog appends an entry to PG audit_log table.
// ASYNC: runs in background goroutine to avoid blocking message processing.
func auditLog(entry AuditEntry) {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().In(time.FixedZone("WIB", 7*3600)).Format("2006-01-02 15:04:05")
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[audit] panic: %v\n", r)
			}
		}()
		details, err := json.Marshal(entry)
		if err != nil {
			fmt.Printf("[audit] marshal error: %v\n", err)
			return
		}
		action := entry.Cmd
		if action == "" {
			action = "unknown"
		}
		if len(action) > 200 {
			action = action[:200]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		db, err := pgGet()
		if err != nil {
			fmt.Printf("[audit] pg error: %v\n", err)
			return
		}
		_, err = pgExec(ctx, db,
			`INSERT INTO audit_log (ts, jid, action, details) VALUES (NOW(), ?, ?, ?)`,
			entry.Actor, action, string(details))
		if err != nil {
			fmt.Printf("[audit] insert error: %v\n", err)
		}
	}()
}

// maskArgs redacts potentially sensitive args (paths, shell commands).
func maskArgs(args string) string {
	if args == "" {
		return ""
	}
	const maxShow = 64
	const tailShow = 8
	if len(args) <= maxShow {
		return args
	}
	head := args[:maxShow]
	tail := args[len(args)-tailShow:]
	return fmt.Sprintf("%s…%d…%s", head, len(args)-maxShow-tailShow, tail)
}

// auditCommand is a convenience wrapper for owner actions.
func auditCommand(msg *events.Message, cmd, args, status string, durMs int64) {
	if msg == nil {
		return
	}
	actor := msg.Info.Sender.String()
	phone := senderPhoneBest(botClient, msg)
	chat := msg.Info.Chat.String()
	auditLog(AuditEntry{
		Actor:      actor,
		ActorPhone: phone,
		Chat:       chat,
		Cmd:        cmd,
		Args:       maskArgs(args),
		Status:     status,
		DurMs:      durMs,
	})
}

// queryAuditLog retrieves recent audit entries from PG.
func queryAuditLog(limit int) string {
	if limit <= 0 {
		limit = 50
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := pgGet()
	if err != nil {
		return "[]"
	}
	rows, err := pgQuery(ctx, db,
		`SELECT details FROM audit_log ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return "[]"
	}
	defer rows.Close()
	var entries []string
	for rows.Next() {
		var details string
		if err := rows.Scan(&details); err == nil {
			entries = append(entries, details)
		}
	}
	if entries == nil {
		return "[]"
	}
	return "[" + strings.Join(entries, ",") + "]"
}
