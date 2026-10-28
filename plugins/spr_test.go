package main

// spr_test.go — Thin wrapper around spr_metrics.go.
// ════════════════════════════════════════════════════════════
//
// This file is now a thin wrapper. All the metrics + diagnostics
// infrastructure lives in spr_metrics.go (shared with the rest of
// the SPR ecosystem).
//
// The .sprtest command remains functional for backward compatibility,
// but you can safely delete this file — the same features are
// available via .spr subcommands (.spr metrics, .spr audit, etc.)
// which call directly into spr_metrics.go functions.
//
// If you delete this file:
//   • .sprtest command disappears (expected — replaced by .spr)
//   • All SPR ecosystem functionality stays intact (spr.go, spr2.go,
//     sbpr.go, spr_cloak.go, spr_senderkey.go, stealth_primary.go)
//   • Metrics auto-recording via sendSPREngineAHide stays (spr.go)
//   • All diagnostics available via .spr subcommands

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waBinary "github.com/polymorfa/hypermeow/binary"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── SPR test-specific structs (NOT shared) ─────────────────────

// SPRTestConfig holds options for one SPR test send.
type SPRTestConfig struct {
	GroupJID     types.JID
	Message      string
	Mode         string
	Targets      []string
	TargetMe     *bool
	DeviceMode   string
	DryRun       bool
	RotateBefore bool
	RotateAfter  bool
}

// SPRTestResult captures the stats from one SPR test send.
type SPRTestResult struct {
	GroupJID            types.JID
	Message             string
	MsgID               string
	SentAt              time.Time
	ParticipantCount    int
	SenderKeyRowsBefore int
	SenderKeyRowsAfter  int
	StanzaXML           string
	ExtraNodesCount     int
	Error               string
}

// ─── .sprtest dispatcher ─────────────────────────────────────────

// handleSPRTestCommand — dispatcher for .sprtest subcommands.
//
// DEFAULT INVOCATION (mirrors Baileys sPR call):
//
//	.sprtest              → SPR send: target=[sender], text=BOT_NAME, mode=include
//	.sprtest <text>       → same but with custom text
//
// DIAGNOSTIC SUBCOMMANDS (delegate to spr_metrics.go):
//
//	.sprtest metrics|audit|health|probe|trace|keys|reset-metrics|history|bench
//
// All diagnostics are ALSO accessible via .spr (e.g. .spr metrics) — that's
// the preferred way going forward. .sprtest is kept for backward compat.
func handleSPRTestCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if client == nil {
		return "❌ Client nil"
	}
	parts := strings.Fields(args)
	if len(parts) == 0 {
		// No args = default SPR send with bot name as text
		return sprTestRunDefault(client, msg, "")
	}

	// Delegate diagnostics to spr_metrics.go via sprHandleDiagnosticSubcommand.
	// This way the same code path serves both .sprtest <diag> and .spr <diag>.
	if out, matched := sprHandleDiagnosticSubcommand(client, msg, args); matched {
		return out
	}

	// Also handle .sprtest-specific subcommands (status, info, rotate, send, spr, stanza)
	sub := strings.ToLower(parts[0])
	switch sub {
	case "help", "?":
		return sprTestHelp()
	case "status":
		groupJID := ""
		if len(parts) >= 2 {
			groupJID = parts[1]
		}
		return sprTestStatus(groupJID)
	case "info":
		groupJID := ""
		if len(parts) >= 2 {
			groupJID = parts[1]
		} else {
			groupJID = msg.Info.Chat.String()
		}
		return sprTestInfo(client, groupJID)
	case "rotate":
		if len(parts) < 2 {
			return "❌ Pakai: .sprtest rotate <group_jid>"
		}
		return sprTestRotate(parts[1])
	case "send":
		if len(parts) < 3 {
			return "❌ Pakai: .sprtest send <group_jid> <text>"
		}
		return sprTestSend(client, parts[1], strings.Join(parts[2:], " "), false)
	case "spr":
		if len(parts) < 3 {
			return "❌ Pakai: .sprtest spr <group_jid> <text>"
		}
		return sprTestSend(client, parts[1], strings.Join(parts[2:], " "), true)
	case "stanza":
		if len(parts) < 3 {
			return "❌ Pakai: .sprtest stanza <group_jid> <text>"
		}
		return sprTestStanzaPreview(client, parts[1], strings.Join(parts[2:], " "))
	default:
		// Not a known subcommand → treat the WHOLE args as text for default SPR
		return sprTestRunDefault(client, msg, args)
	}
}

// sprTestRunDefault runs the default SPR send.
// Mirrors Baileys: sPR(conn, m.chat, { conversation: text }, { mode: 'include', target: [m.sender] })
func sprTestRunDefault(client *whatsmeow.Client, msg *events.Message, text string) string {
	if !msg.Info.IsGroup {
		return "❌ SPR hanya bekerja di grup (butuh sender key distribution)"
	}
	target := msg.Info.Sender.ToNonAD()
	if target.IsEmpty() || target.Server == "" {
		target = msg.Info.SenderAlt.ToNonAD()
	}
	if target.IsEmpty() {
		return "❌ Tidak bisa resolve sender JID"
	}

	if text == "" {
		text = getenvDefault("BOT_NAME", "Bot")
	}

	participants, err := buildSPRParticipants(client, msg.Info.Chat, []types.JID{target})
	if err != nil {
		return fmt.Sprintf("❌ buildSPRParticipants: %v", err)
	}
	if len(participants) == 0 {
		return "❌ Tidak ada participant device yang di-resolve untuk target"
	}

	waMsg := &waE2E.Message{Conversation: &text}

	rowsBefore := sprCountSenderKeys(msg.Info.Chat.String())

	// Call sendSPREngineAHide directly — it auto-records metrics via spr_metrics.go
	// with source="spr_engine". No need for sprSendTracked wrapper.
	started := time.Now()
	msgID, err := sendSPREngineAHide(client, msg.Info.Chat, participants, waMsg)
	elapsed := time.Since(started)
	if err != nil {
		return fmt.Sprintf("❌ sendSPREngineAHide: %v", err)
	}

	rowsAfter := sprCountSenderKeys(msg.Info.Chat.String())

	var sb strings.Builder
	sb.WriteString("✅ *SPR Test Run*\n\n")
	sb.WriteString(fmt.Sprintf("• Msg ID: `%s`\n", msgID))
	sb.WriteString(fmt.Sprintf("• Group: `%s`\n", msg.Info.Chat.String()))
	sb.WriteString(fmt.Sprintf("• Target: `%s` (mode=include)\n", target.String()))
	sb.WriteString(fmt.Sprintf("• Participants (devices receiving SKDM): %d\n", len(participants)))
	sb.WriteString(fmt.Sprintf("• Text: %s\n", text))
	sb.WriteString(fmt.Sprintf("• Elapsed: %s\n", elapsed.Round(time.Millisecond)))
	sb.WriteString(fmt.Sprintf("• Sender keys BEFORE: %d\n", rowsBefore))
	sb.WriteString(fmt.Sprintf("• Sender keys AFTER:  %d\n", rowsAfter))
	if d := rowsAfter - rowsBefore; d > 0 {
		sb.WriteString(fmt.Sprintf("• Δ added: +%d (SPR engine minted new keys for target devices)\n", d))
	}
	// Quick metrics summary (from shared spr_metrics.go)
	if m := sprMetricsSnapshot(); m != nil && m.Total > 0 {
		sb.WriteString(fmt.Sprintf("\n📊 *Cumulative*: %d sends, %d ok, %d fail, avg %dms",
			m.Total, m.Success, m.Failed, m.AvgLatencyMS()))
	}
	return sb.String()
}

// sprTestHelp returns the help text for .sprtest.
// Points users to .spr subcommands as the preferred way.
func sprTestHelp() string {
	return `🔧 *SPR Test Commands* (deprecated — use .spr <sub>)

This command is a thin wrapper. Same features available via .spr:

═══ .spr DIAGNOSTICS (preferred) ═══
• .spr metrics           — cumulative stats + per-source/per-error breakdown
• .spr audit             — aggregate analytics (latency p50/p95/p99, top groups)
• .spr health            — 8-point health check
• .spr probe [group]     — deep group diagnostic
• .spr trace <msg_id>    — end-to-end message trace
• .spr keys [group]      — sender key browser
• .spr history [N]       — last N SPR sends from audit_log
• .spr bench [N]         — benchmark N SPR sends, report latency
• .spr reset-metrics     — clear cumulative metrics (audit_log preserved)

═══ .sprtest-only commands ═══
• .sprtest status [group]  — sender key counts
• .sprtest info [group]     — group info
• .sprtest rotate <group>   — force sender key rotation
• .sprtest send <group> <text>  — normal group send + stats
• .sprtest spr <group> <text>   — full SPR flow rotate→send→rotate
• .sprtest stanza <group> <text> — dry-run stanza XML preview

You can safely delete .sprtest — all diagnostics live in spr_metrics.go
and are accessible via .spr subcommands.`
}

// ─── Helper functions (test-specific, NOT shared) ────────────────

// sprTestStatus shows sender key counts in PG.
func sprTestStatus(groupJID string) string {
	db, err := pgGet()
	if err != nil {
		return fmt.Sprintf("❌ PG: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if groupJID != "" {
		gid := strings.Split(groupJID, ":")[0]
		if !strings.Contains(gid, "@") {
			gid = gid + "@g.us"
		}
		var total, mine, isolated int
		_ = pgQueryRow(ctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ?`,
			gid).Scan(&total)
		if botClient != nil && botClient.Store != nil && botClient.Store.ID != nil {
			myUser := botClient.Store.ID.User
			_ = pgQueryRow(ctx, db,
				`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ? AND sender_id LIKE ?`,
				gid, myUser+"%").Scan(&mine)
		}
		senderID, _ := sprIsolatedSenderID()
		if senderID != "" {
			_ = pgQueryRow(ctx, db,
				`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ? AND sender_id = ?`,
				gid, senderID).Scan(&isolated)
		}
		return fmt.Sprintf("📊 *Sender Keys for %s*\n\n• Total: %d\n• Mine (own sends): %d\n• Isolated (SPR): %d",
			gid, total, mine, isolated)
	}

	var grandTotal int
	_ = pgQueryRow(ctx, db,
		`SELECT COUNT(*) FROM whatsmeow_sender_keys`).Scan(&grandTotal)
	rows, err := pgQuery(ctx, db,
		`SELECT chat_id, COUNT(*) AS k
                 FROM whatsmeow_sender_keys
                 GROUP BY chat_id
                 ORDER BY k DESC
                 LIMIT 10`)
	if err != nil {
		return fmt.Sprintf("📊 Total sender keys: %d (failed to fetch top groups: %v)", grandTotal, err)
	}
	defer rows.Close()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 *Sender Keys Summary*\n\n• Total rows: %d\n\n", grandTotal))
	sb.WriteString("Top 10 groups by key count:\n")
	i := 0
	for rows.Next() {
		var chatID string
		var count int
		if err := rows.Scan(&chatID, &count); err != nil {
			continue
		}
		i++
		short := chatID
		if len(short) > 30 {
			short = short[:27] + "…"
		}
		sb.WriteString(fmt.Sprintf("  %d. `%s` — %d keys\n", i, short, count))
	}
	if i == 0 {
		sb.WriteString("  (no keys stored)")
	}
	return sb.String()
}

// sprTestInfo shows comprehensive group info.
func sprTestInfo(client *whatsmeow.Client, groupJID string) string {
	if client == nil {
		return "❌ Client nil"
	}
	gid := strings.Split(groupJID, ":")[0]
	if !strings.Contains(gid, "@") {
		gid = gid + "@g.us"
	}
	parsed, err := types.ParseJID(gid)
	if err != nil {
		return fmt.Sprintf("❌ invalid JID %q: %v", gid, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	gi, err := client.GetGroupInfo(ctx, parsed)
	if err != nil {
		return fmt.Sprintf("❌ GetGroupInfo: %v", err)
	}

	db, _ := pgGet()
	var keyCount int
	if db != nil {
		_ = pgQueryRow(ctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ?`,
			gid).Scan(&keyCount)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👥 *Group Info*\n\n"))
	sb.WriteString(fmt.Sprintf("• Name: %s\n", gi.Name))
	sb.WriteString(fmt.Sprintf("• JID: `%s`\n", gi.JID.String()))
	sb.WriteString(fmt.Sprintf("• Owner: `%s`\n", gi.OwnerJID.String()))
	sb.WriteString(fmt.Sprintf("• Participants: %d\n", len(gi.Participants)))
	sb.WriteString(fmt.Sprintf("• Sender keys in PG: %d\n", keyCount))
	if gi.GroupEphemeral.IsEphemeral {
		sb.WriteString(fmt.Sprintf("• Ephemeral: %d seconds\n", gi.GroupEphemeral.DisappearingTimer))
	}
	if !gi.GroupCreated.IsZero() {
		sb.WriteString(fmt.Sprintf("• Created: %s\n", gi.GroupCreated.Format("02-01-2006")))
	}

	sb.WriteString("\nParticipants (first 15):\n")
	maxShow := 15
	if len(gi.Participants) < maxShow {
		maxShow = len(gi.Participants)
	}
	for i := 0; i < maxShow; i++ {
		p := gi.Participants[i]
		role := "member"
		if p.IsAdmin {
			role = "admin"
		}
		if p.IsSuperAdmin {
			role = "owner"
		}
		sb.WriteString(fmt.Sprintf("  • `%s` (%s)\n", p.JID.String(), role))
	}
	if len(gi.Participants) > maxShow {
		sb.WriteString(fmt.Sprintf("  • ... %d more\n", len(gi.Participants)-maxShow))
	}

	return sb.String()
}

// sprTestRotate forces sender key rotation for a group.
func sprTestRotate(groupJID string) string {
	gid := strings.Split(groupJID, ":")[0]
	if !strings.Contains(gid, "@") {
		gid = gid + "@g.us"
	}
	parsed, err := types.ParseJID(gid)
	if err != nil {
		return fmt.Sprintf("❌ invalid JID: %v", err)
	}
	if err := sprRotateSenderKey(parsed); err != nil {
		return fmt.Sprintf("❌ rotate: %v", err)
	}
	return fmt.Sprintf("✅ Sender key rotated for %s", gid)
}

// sprTestSend sends a group message (optionally with SPR flow).
func sprTestSend(client *whatsmeow.Client, groupJID, text string, spr bool) string {
	if client == nil {
		return "❌ Client nil"
	}
	gid := strings.Split(groupJID, ":")[0]
	if !strings.Contains(gid, "@") {
		gid = gid + "@g.us"
	}
	parsed, err := types.ParseJID(gid)
	if err != nil {
		return fmt.Sprintf("❌ invalid JID: %v", err)
	}

	rowsBefore := sprCountSenderKeys(gid)

	result := SPRTestResult{
		GroupJID:            parsed,
		Message:             text,
		SentAt:              time.Now(),
		SenderKeyRowsBefore: rowsBefore,
	}

	if spr {
		if err := sprRotateSenderKey(parsed); err != nil {
			result.Error = fmt.Sprintf("rotate before: %v", err)
			return sprTestFormatResult(result)
		}
		if err := sprDeleteIsolatedSenderKey(parsed); err != nil {
			result.Error = fmt.Sprintf("delete isolated before: %v", err)
			return sprTestFormatResult(result)
		}
	}

	msgID, err := sprGenerateMsgID()
	if err != nil {
		result.Error = fmt.Sprintf("gen msg id: %v", err)
		return sprTestFormatResult(result)
	}
	result.MsgID = msgID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waMsg := &waE2E.Message{Conversation: &text}
	_, err = client.SendMessage(ctx, parsed, waMsg, whatsmeow.SendRequestExtra{
		ID: types.MessageID(msgID),
	})
	if err != nil {
		result.Error = fmt.Sprintf("send: %v", err)
		return sprTestFormatResult(result)
	}

	if spr {
		result.SenderKeyRowsAfter = sprCountSenderKeys(gid)
		if err := sprDeleteIsolatedSenderKey(parsed); err != nil {
			result.Error = fmt.Sprintf("delete isolated after: %v", err)
			return sprTestFormatResult(result)
		}
	} else {
		result.SenderKeyRowsAfter = sprCountSenderKeys(gid)
	}

	return sprTestFormatResult(result)
}

// sprTestStanzaPreview builds the stanza XML that would be sent (dry-run).
func sprTestStanzaPreview(client *whatsmeow.Client, groupJID, text string) string {
	if client == nil {
		return "❌ Client nil"
	}
	gid := strings.Split(groupJID, ":")[0]
	if !strings.Contains(gid, "@") {
		gid = gid + "@g.us"
	}
	parsed, err := types.ParseJID(gid)
	if err != nil {
		return fmt.Sprintf("❌ invalid JID: %v", err)
	}

	msgID, _ := sprGenerateMsgID()
	ownID := types.EmptyJID
	if client.Store != nil && client.Store.ID != nil {
		ownID = *client.Store.ID
	}

	stanza := waBinary.Node{
		Tag: "message",
		Attrs: waBinary.Attrs{
			"id":   msgID,
			"to":   parsed.String(),
			"type": "text",
		},
		Content: []waBinary.Node{
			{
				Tag:     "enc",
				Attrs:   waBinary.Attrs{"v": "2", "type": "skmsg"},
				Content: []byte("(encrypted skmsg bytes — generated by whatsmeow at send time)"),
			},
		},
	}

	xml, err := waBinary.Marshal(stanza)
	if err != nil {
		return fmt.Sprintf("❌ marshal stanza: %v", err)
	}

	xmlStr := string(xml)
	if len(xmlStr) > 1500 {
		xmlStr = xmlStr[:1497] + "…"
	}

	return fmt.Sprintf("📄 *Stanza Preview* (dry-run)\n\n• Msg ID: `%s`\n• From: `%s`\n• To: `%s`\n• Text: %s\n\n```xml\n%s\n```",
		msgID, ownID.String(), parsed.String(), text, xmlStr)
}

// ─── Helper utilities (test-specific, NOT shared) ───────────────

// sprCountSenderKeys counts all sender key rows for a group in PG.
func sprCountSenderKeys(groupJID string) int {
	db, err := pgGet()
	if err != nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	_ = pgQueryRow(ctx, db,
		`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ?`,
		groupJID).Scan(&count)
	return count
}

// sprGenerateMsgID generates a WhatsApp-style message ID.
// Format: uppercase hex, ~20 chars.
func sprGenerateMsgID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(b)), nil
}

// sprTestFormatResult formats a SPRTestResult as a user-friendly string.
func sprTestFormatResult(r SPRTestResult) string {
	var sb strings.Builder
	if r.Error != "" {
		sb.WriteString(fmt.Sprintf("❌ *SPR Test FAILED*\n\n• Group: `%s`\n• Error: %s",
			r.GroupJID.String(), r.Error))
		return sb.String()
	}
	sb.WriteString("✅ *SPR Test Result*\n\n")
	sb.WriteString(fmt.Sprintf("• Msg ID: `%s`\n", r.MsgID))
	sb.WriteString(fmt.Sprintf("• Group: `%s`\n", r.GroupJID.String()))
	sb.WriteString(fmt.Sprintf("• Text: %s\n", r.Message))
	sb.WriteString(fmt.Sprintf("• Sent at: %s\n", r.SentAt.Format("15:04:05")))
	sb.WriteString(fmt.Sprintf("• Sender keys BEFORE: %d\n", r.SenderKeyRowsBefore))
	sb.WriteString(fmt.Sprintf("• Sender keys AFTER:  %d\n", r.SenderKeyRowsAfter))
	delta := r.SenderKeyRowsAfter - r.SenderKeyRowsBefore
	if delta > 0 {
		sb.WriteString(fmt.Sprintf("• Δ added: +%d (whatsmeow minted %d new key rows during send)\n",
			delta, delta))
	} else if delta < 0 {
		sb.WriteString(fmt.Sprintf("• Δ removed: %d (cleanup deleted %d rows)\n", delta, -delta))
	} else {
		sb.WriteString("• Δ unchanged\n")
	}
	return sb.String()
}

// ─── Plugin registration ──────────────────────────────────────

func init() {
	RegisterPlugin(Plugin{
		Command:     "sprtest",
		Description: "SPR test & diagnostic commands (thin wrapper — use .spr <sub> for same features)",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSPRTestCommand(client, msg, args)
		},
	})
}
