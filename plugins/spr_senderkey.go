package main

/**
 * 🔑 spr_senderkey.go — SenderKey rotation for SPR
 * ════════════════════════════════════════════════════════════
 * SPR (Secret Private Relay) works by restricting SKDM delivery to only
 * target devices. BUT: if non-targets already have the SenderKey (from
 * a previous normal group message), they CAN decrypt the SPR skmsg.
 *
 * Fix: delete the SenderKey from the DB before & after SPR send.
 *   - Before: forces sendGroup to create a NEW key → only SPR targets get it
 *   - After: forces next normal group message to create ANOTHER new key
 *     (different from SPR key) → non-targets get the new normal key via
 *     retry mechanism, but can NEVER get the SPR key
 *
 * The SenderKey table (whatsmeow_sender_keys) is in PostgreSQL now
 * (alongside all other bot data). Connection via pgGet() shared pool.
 *
 * We delete by (our_jid, chat_id) to remove ALL sender keys for this group.
 *
 * 2026-10-22: migrated from SQLite (sessions.db) to PostgreSQL (botdb).
 * Removed: sprGetDB, sprGetDBPath, sprKey* vars (all handled by pg.go).
 */

import (
	"context"
	"fmt"
	"time"

	"github.com/polymorfa/hypermeow/types"
)

// sprRotateSenderKey deletes ALL sender keys for the given group from the
// DB. This forces whatsmeow's sendGroup to create a NEW SenderKey on next
// send, ensuring only SPR targets receive the SKDM.
func sprRotateSenderKey(groupJID types.JID) error {
	db, err := pgGet()
	if err != nil {
		return err
	}

	if botClient == nil || botClient.Store == nil || botClient.Store.ID == nil {
		return fmt.Errorf("bot not logged in")
	}
	groupID := groupJID.ToNonAD().String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Diagnostic: count rows BEFORE delete
	var beforeCount int
	_ = pgQueryRow(ctx, db,
		`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ?`,
		groupID).Scan(&beforeCount)
	fmt.Printf("[SPR] sender keys BEFORE rotation: %d rows for group %s\n", beforeCount, groupID)

	// Delete ALL sender keys for this group (regardless of our_jid or sender_id form)
	result, err := pgExec(ctx, db,
		`DELETE FROM whatsmeow_sender_keys WHERE chat_id = ?`,
		groupID)
	if err != nil {
		return fmt.Errorf("delete sender key: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows > 0 {
		fmt.Printf("[SPR] sender key rotation: deleted %d/%d rows for group %s\n", rows, beforeCount, groupID)
	} else if beforeCount > 0 {
		fmt.Printf("[SPR] ⚠️ sender key rotation: 0 deleted but %d existed! (our_jid mismatch)\n", beforeCount)
	}
	return nil
}

// sprIsolatedSenderID returns the sender_id string for the SPR-only namespace.
// Format mirrors SignalAddress.String(): "<liduser>|spr:<device>".
// Normal group sends use "<liduser>:<device>" so the namespaces never collide.
func sprIsolatedSenderID() (string, error) {
	if botClient == nil || botClient.Store == nil {
		return "", fmt.Errorf("bot not logged in")
	}
	di := botClient.DangerousInternals()
	botLID := di.GetOwnLID()
	if botLID.IsEmpty() {
		return "", fmt.Errorf("bot LID belum tersedia")
	}
	base := botLID.SignalAddress()
	return fmt.Sprintf("%s|spr:%d", base.Name(), base.DeviceID()), nil
}

// sprDeleteIsolatedSenderKey deletes the SPR-namespace sender key for a group.
// MUST be called BEFORE every restricted (SPR) send so each SPR mints a FRESH
// key: reusing one isolated key across SPRs with different targets would let
// an earlier target decrypt a later SPR's message (they kept the old key).
//
// Fail-closed: returns error if the DELETE itself fails (DB problem) —
// callers must abort the send rather than risk leaking.
func sprDeleteIsolatedSenderKey(groupJID types.JID) error {
	db, err := pgGet()
	if err != nil {
		return err
	}
	senderID, err := sprIsolatedSenderID()
	if err != nil {
		return err
	}
	groupID := groupJID.ToNonAD().String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := pgExec(ctx, db,
		`DELETE FROM whatsmeow_sender_keys WHERE chat_id = ? AND sender_id = ?`,
		groupID, senderID)
	if err != nil {
		return fmt.Errorf("delete isolated SPR sender key: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows > 0 {
		fmt.Printf("[SPR] isolated namespace: deleted stale SPR key (%d rows) for group %s\n", rows, groupID)
	}
	return nil
}

// sprRotateSenderKeyForSender deletes only the bot's own sender key for
// the group (more targeted than deleting all).
func sprRotateSenderKeyForSender(groupJIDStr, senderSignalAddr string) error {
	db, err := pgGet()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := pgExec(ctx, db,
		`DELETE FROM whatsmeow_sender_keys WHERE chat_id = ? AND sender_id = ?`,
		groupJIDStr, senderSignalAddr)
	if err != nil {
		return fmt.Errorf("delete sender key: %w", err)
	}

	rows, _ := result.RowsAffected()
	fmt.Printf("[SPR] sender key rotation (targeted): deleted %d rows for sender %s\n", rows, senderSignalAddr)
	return nil
}

// sprCloseDB — no-op: PG connection managed by pg.go's shared pool
func sprCloseDB() {}
