package main

// spr_metrics.go — SPR ecosystem observability + diagnostics (shared).
// ════════════════════════════════════════════════════════════
//
// Strengthening layer for the WHOLE SPR ecosystem. All SPR sends
// (from .spr, .spr2, .sbpr, .sprtest) get auto-tracked here.
//
// Migrated here from spr_test.go so:
//   • Other SPR plugins (spr.go, spr2.go, sbpr.go) can use these
//     functions without depending on spr_test.go
//   • spr_test.go can be deleted by user without breaking anything
//   • .spr command gets new diagnostic subcommands
//
// Persistence:
//   - kv_store(spr_metrics)       → cumulative counters + per-source/
//     per-error-class/per-group breakdowns + FirstSendTS + last send
//   - audit_log(action='spr_send') → per-send history (one row per send)
//     JSONB details: target, latency_ms, status, msg_id, source,
//     error_class, error
//
// Public API:
//   sprClassifyError(err)        → "network"|"crypto"|"pg"|"signal"|"context"|"unknown"
//   sprMetricsRecord(entry)      → update cumulative + log audit_log (async)
//   sprMetricsSnapshot()         → read-only state lookup
//   sprSendTracked(client, groupJID, participants, message, source)
//                                 → drop-in wrapper for sendSPREngineAHide
//                                   that records metrics (opt-in)
//   sprShowMetrics()             → cumulative stats report
//   sprHealthCheck(client)       → 8-point health check
//   sprBenchmark(client, msg, n) → send N SPR + report latency
//   sprHistoryShow(limit)        → audit_log history
//   sprProbeGroup(client, groupJID) → deep group diagnostic
//   sprAuditShow()                → aggregate analytics from audit_log
//   sprTraceMsg(msgID)           → end-to-end message trace
//   sprKeysBrowser(groupJID)     → sender key browser
//   sprResetMetrics()            → clear cumulative metrics (audit_log preserved)

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

// sprMetricsKVKey is the kv_store key for cumulative SPR metrics state.
const sprMetricsKVKey = "spr_metrics"

// sprNewestKeySQL returns the statement that reads the newest sender-key
// timestamp for a group.
//
// PostgreSQL can fold the "no rows" case into the aggregate; SQLite reports
// an empty column type for MAX(), so the newest row is selected directly
// there and the zero time.Time stands in for "no keys yet".
func sprNewestKeySQL() string {
	if storageIsSQLite() {
		return `SELECT created_at FROM whatsmeow_sender_keys WHERE chat_id = ? ORDER BY created_at DESC LIMIT 1`
	}
	return `SELECT COALESCE(MAX(created_at), 'epoch'::timestamptz) FROM whatsmeow_sender_keys WHERE chat_id = ?`
}

// sprPercentileSQL returns a statement computing the given percentile of the
// recorded send latency.
//
// PostgreSQL has PERCENTILE_CONT; SQLite has no equivalent aggregate, so the
// value is derived from the same rows with a window function.
func sprPercentileSQL(p float64) string {
	if storageIsSQLite() {
		return fmt.Sprintf(`SELECT COALESCE((SELECT v FROM (
                          SELECT CAST(details->>'latency_ms' AS REAL) AS v,
                                 ROW_NUMBER() OVER (ORDER BY CAST(details->>'latency_ms' AS REAL)) AS rn,
                                 COUNT(*) OVER () AS cnt
                          FROM audit_log
                          WHERE action='spr_send' AND details->>'status' = 'ok'
                            AND (details->>'latency_ms') IS NOT NULL)
                        WHERE rn = MAX(1, CAST(CEIL(cnt * %g) AS INTEGER))), 0)`, p)
	}
	return fmt.Sprintf(`SELECT COALESCE(PERCENTILE_CONT(%g) WITHIN GROUP (ORDER BY (details->>'latency_ms')::float8), 0)
                 FROM audit_log WHERE action='spr_send' AND details->>'status' = 'ok' AND (details->>'latency_ms') IS NOT NULL`, p)
}

// sprMetricsState is the persisted cumulative metrics snapshot.
// Backward compatible — old kv_store rows without new fields still load.
type sprMetricsState struct {
	Total         int            `json:"total"`
	Success       int            `json:"success"`
	Failed        int            `json:"failed"`
	SumLatencyMS  int64          `json:"sum_latency_ms"`
	MinLatencyMS  int64          `json:"min_latency_ms"`
	MaxLatencyMS  int64          `json:"max_latency_ms"`
	LastSendTS    int64          `json:"last_send_ts"`
	LastSendMsgID string         `json:"last_send_msg_id"`
	PerGroup      map[string]int `json:"per_group"`                 // group_jid → count
	PerSource     map[string]int `json:"per_source,omitempty"`      // source tag → count
	PerErrorClass map[string]int `json:"per_error_class,omitempty"` // error class → count
	FirstSendTS   int64          `json:"first_send_ts,omitempty"`   // first send ever
}

// sprMetricsEntry is one recorded SPR send (in-memory transient).
type sprMetricsEntry struct {
	MsgID      string
	GroupJID   string
	Target     string
	LatencyMS  int64
	Status     string // "ok" or "fail"
	Error      string // populated when status="fail"
	Source     string // "spr", "spr2", "sbpr", "spr_engine", "sprtest_default", ...
	ErrorClass string // "network", "crypto", "pg", "signal", "context", "unknown"
}

// AvgLatencyMS returns the average latency in milliseconds.
func (m *sprMetricsState) AvgLatencyMS() int64 {
	if m.Success == 0 {
		return 0
	}
	return m.SumLatencyMS / int64(m.Success)
}

var sprMetricsMu sync.Mutex

// auditLogMaxRows is the cap on audit_log rows for SPR sends.
// When the count exceeds this, oldest rows are deleted in background.
const auditLogMaxRows = 5000

// auditLogCleanupInterval is how often the cleanup goroutine runs.
const auditLogCleanupInterval = 1 * time.Hour

// sprAuditLogCleanupStarted ensures the cleanup goroutine is only started once.
var sprAuditLogCleanupStarted bool

// sprAuditLogStartCleanup starts a background goroutine that periodically
// trims audit_log to the last auditLogMaxRows rows. Idempotent — calling
// multiple times is safe; only the first call starts the goroutine.
//
// This prevents audit_log from growing unbounded over time as every
// SPR send creates a row.
func sprAuditLogStartCleanup() {
	sprMetricsMu.Lock()
	if sprAuditLogCleanupStarted {
		sprMetricsMu.Unlock()
		return
	}
	sprAuditLogCleanupStarted = true
	sprMetricsMu.Unlock()
	go func() {
		ticker := time.NewTicker(auditLogCleanupInterval)
		defer ticker.Stop()
		// Run once at startup, then on every tick
		sprAuditLogTrim()
		for range ticker.C {
			sprAuditLogTrim()
		}
	}()
}

// sprAuditLogTrim deletes oldest audit_log rows beyond auditLogMaxRows.
// Best-effort — silently fails if PG unavailable.
func sprAuditLogTrim() {
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Delete all rows except the newest auditLogMaxRows.
	// Uses subquery to find the cutoff id, then deletes everything older.
	_, _ = pgExec(ctx, db,
		`DELETE FROM audit_log
                 WHERE id NOT IN (
                   SELECT id FROM audit_log ORDER BY id DESC LIMIT ?
                 )`, auditLogMaxRows)
}

// init triggers the cleanup goroutine on package load (once per process).
// Wrapped in sprAuditLogStartCleanup for idempotency.
func init() {
	sprAuditLogStartCleanup()
}

// sprClassifyError categorizes an SPR send error into a known class.
func sprClassifyError(errStr string) string {
	if errStr == "" {
		return "unknown"
	}
	low := strings.ToLower(errStr)
	switch {
	case strings.Contains(low, "websocket") ||
		strings.Contains(low, "connection") ||
		strings.Contains(low, "timeout") ||
		strings.Contains(low, "eof") ||
		strings.Contains(low, "dial") ||
		strings.Contains(low, "network") ||
		strings.Contains(low, "broken pipe") ||
		strings.Contains(low, "reset"):
		return "network"
	case strings.Contains(low, "encrypt") ||
		strings.Contains(low, "decrypt") ||
		strings.Contains(low, "cipher") ||
		strings.Contains(low, "aes") ||
		strings.Contains(low, "hmac") ||
		strings.Contains(low, "marshal") ||
		strings.Contains(low, "unmarshal"):
		return "crypto"
	case strings.Contains(low, "pg") ||
		strings.Contains(low, "postgres") ||
		strings.Contains(low, "sql:") ||
		strings.Contains(low, "query") ||
		strings.Contains(low, "scan"):
		return "pg"
	case strings.Contains(low, "sender key") ||
		strings.Contains(low, "prekey") ||
		strings.Contains(low, "pre-key") ||
		strings.Contains(low, "identity") ||
		strings.Contains(low, "signal") ||
		strings.Contains(low, "session") ||
		strings.Contains(low, "assertsessions"):
		return "signal"
	case strings.Contains(low, "context canceled") ||
		strings.Contains(low, "deadline exceeded") ||
		strings.Contains(low, "context deadline"):
		return "context"
	}
	return "unknown"
}

// sprMetricsRecord updates cumulative metrics + logs per-send to audit_log.
// Best-effort — failures are logged but never panic.
//
// Note: sendSPREngineAHide (in spr.go) auto-calls this with source="spr_engine"
// after every send. So all SPR sends (.spr, .spr2, .sbpr, .sprtest) get
// auto-tracked. Per-source granularity defaults to "spr_engine" for all.
func sprMetricsRecord(e sprMetricsEntry) {
	sprMetricsMu.Lock()
	defer sprMetricsMu.Unlock()

	var state sprMetricsState
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kvStoreGet(ctx, sprMetricsKVKey, &state)
	if state.PerGroup == nil {
		state.PerGroup = map[string]int{}
	}
	if state.PerSource == nil {
		state.PerSource = map[string]int{}
	}
	if state.PerErrorClass == nil {
		state.PerErrorClass = map[string]int{}
	}
	if state.FirstSendTS == 0 {
		state.FirstSendTS = time.Now().Unix()
	}

	state.Total++
	if e.Source != "" {
		state.PerSource[e.Source]++
	}
	if e.Status == "ok" {
		state.Success++
		state.SumLatencyMS += e.LatencyMS
		if state.MinLatencyMS == 0 || e.LatencyMS < state.MinLatencyMS {
			state.MinLatencyMS = e.LatencyMS
		}
		if e.LatencyMS > state.MaxLatencyMS {
			state.MaxLatencyMS = e.LatencyMS
		}
	} else {
		state.Failed++
		if e.ErrorClass == "" {
			e.ErrorClass = sprClassifyError(e.Error)
		}
		if e.ErrorClass != "" {
			state.PerErrorClass[e.ErrorClass]++
		}
	}
	state.LastSendTS = time.Now().Unix()
	state.LastSendMsgID = e.MsgID
	state.PerGroup[e.GroupJID]++

	_ = kvStoreSet(ctx, sprMetricsKVKey, &state)

	// Per-send entry to audit_log (background goroutine to not block)
	go func(entry sprMetricsEntry) {
		bgctx, bgcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgcancel()
		db, err := pgGet()
		if err != nil {
			return
		}
		details := map[string]any{
			"target":      entry.Target,
			"latency_ms":  entry.LatencyMS,
			"status":      entry.Status,
			"msg_id":      entry.MsgID,
			"source":      entry.Source,
			"error_class": entry.ErrorClass,
		}
		if entry.Error != "" {
			details["error"] = entry.Error
		}
		detailsJSON := sprJSONMarshalSafe(details)
		_, _ = pgExec(bgctx, db,
			`INSERT INTO audit_log (jid, action, details) VALUES (?, ?, ?::jsonb)`,
			entry.GroupJID, "spr_send", detailsJSON)
	}(e)
}

// sprMetricsSnapshot returns the current metrics state (read-only).
func sprMetricsSnapshot() *sprMetricsState {
	sprMetricsMu.Lock()
	defer sprMetricsMu.Unlock()
	var state sprMetricsState
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !kvStoreGet(ctx, sprMetricsKVKey, &state) {
		return nil
	}
	if state.PerGroup == nil {
		state.PerGroup = map[string]int{}
	}
	if state.PerSource == nil {
		state.PerSource = map[string]int{}
	}
	if state.PerErrorClass == nil {
		state.PerErrorClass = map[string]int{}
	}
	return &state
}

// ─── Diagnostic renderers (called from .spr subcommands) ────────

// sprShowMetrics renders the cumulative metrics as a report.
func sprShowMetrics() string {
	m := sprMetricsSnapshot()
	if m == nil {
		return "❌ Tidak bisa load metrics dari PG"
	}
	if m.Total == 0 {
		return "📊 *SPR Metrics*\n\nBelum ada send tercatat.\n\nKirim `.spr` (kirim SPR ke target) atau `.spr bench N` untuk mulai."
	}
	successRate := float64(0)
	if m.Total > 0 {
		successRate = float64(m.Success) * 100 / float64(m.Total)
	}
	var sb strings.Builder
	sb.WriteString("📊 *SPR Metrics*\n\n")
	sb.WriteString(fmt.Sprintf("• Total sends: %d\n", m.Total))
	sb.WriteString(fmt.Sprintf("• Success: %d (%.1f%%)\n", m.Success, successRate))
	sb.WriteString(fmt.Sprintf("• Failed: %d\n", m.Failed))
	sb.WriteString(fmt.Sprintf("• Avg latency: %dms\n", m.AvgLatencyMS()))
	if m.MinLatencyMS > 0 {
		sb.WriteString(fmt.Sprintf("• Min/Max latency: %dms / %dms\n", m.MinLatencyMS, m.MaxLatencyMS))
	}
	if m.LastSendTS > 0 {
		ts := time.Unix(m.LastSendTS, 0).Format("02-01 15:04:05")
		sb.WriteString(fmt.Sprintf("• Last send: %s\n", ts))
		sb.WriteString(fmt.Sprintf("  Msg ID: `%s`\n", m.LastSendMsgID))
	}
	if m.FirstSendTS > 0 {
		uptime := time.Since(time.Unix(m.FirstSendTS, 0)).Round(time.Hour)
		sb.WriteString(fmt.Sprintf("• Tracking since: %s (%s ago)\n",
			time.Unix(m.FirstSendTS, 0).Format("02-01-2006"), uptime))
	}
	// Top groups
	if len(m.PerGroup) > 0 {
		type groupCount struct {
			jid   string
			count int
		}
		var groups []groupCount
		for jid, c := range m.PerGroup {
			groups = append(groups, groupCount{jid, c})
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].count > groups[j].count })
		sb.WriteString(fmt.Sprintf("\n📍 Per-group (%d unique groups):\n", len(groups)))
		max := 5
		if len(groups) < max {
			max = len(groups)
		}
		for i := 0; i < max; i++ {
			g := groups[i]
			short := g.jid
			if len(short) > 30 {
				short = short[:27] + "…"
			}
			sb.WriteString(fmt.Sprintf("  • `%s` — %d sends\n", short, g.count))
		}
		if len(groups) > max {
			sb.WriteString(fmt.Sprintf("  • … +%d other groups\n", len(groups)-max))
		}
	}
	// Per-source breakdown
	if len(m.PerSource) > 0 {
		sb.WriteString("\n🔍 Per-source:\n")
		var keys []string
		for k := range m.PerSource {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("  • %s: %d\n", k, m.PerSource[k]))
		}
	}
	// Per-error-class breakdown
	if len(m.PerErrorClass) > 0 {
		sb.WriteString("\n❌ Per-error-class:\n")
		var keys []string
		for k := range m.PerErrorClass {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("  • %s: %d\n", k, m.PerErrorClass[k]))
		}
	}
	return sb.String()
}

// sprHealthCheck runs a comprehensive health check of all SPR components.
func sprHealthCheck(client *whatsmeow.Client) string {
	var sb strings.Builder
	sb.WriteString("🏥 *SPR Health Check*\n\n")
	checks := 0
	passed := 0

	// 1. PG connection
	checks++
	if db, err := pgGet(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err == nil {
			sb.WriteString("✅ PG connection: OK\n")
			passed++
		} else {
			sb.WriteString(fmt.Sprintf("❌ PG ping: %v\n", err))
		}
	} else {
		sb.WriteString(fmt.Sprintf("❌ PG connection: %v\n", err))
	}

	// 2. Bot identity
	checks++
	if client != nil && client.Store != nil && client.Store.ID != nil {
		botJID := client.Store.ID.String()
		sb.WriteString(fmt.Sprintf("✅ Bot identity: `%s`\n", botJID))
		passed++
		if di := client.DangerousInternals(); di != nil {
			if botLID := di.GetOwnLID(); !botLID.IsEmpty() {
				sb.WriteString(fmt.Sprintf("   LID: `%s`\n", botLID.String()))
			}
		}
	} else {
		sb.WriteString("❌ Bot identity: belum login\n")
	}

	// 3. SPR engine availability
	checks++
	if _, err := sprIsolatedSenderID(); err == nil {
		sb.WriteString("✅ SPR engine: spr_senderkey helpers loaded\n")
		passed++
	} else {
		sb.WriteString(fmt.Sprintf("❌ SPR engine: %v\n", err))
	}

	// 4. Owner count
	checks++
	owners := ownerJIDs()
	if len(owners) > 0 {
		sb.WriteString(fmt.Sprintf("✅ Owners: %d registered\n", len(owners)))
		passed++
	} else {
		sb.WriteString("❌ Owners: belum ada (OWNER_JID env)\n")
	}

	// 5. Cloak config
	checks++
	loadSPRCloak()
	sb.WriteString(fmt.Sprintf("✅ SPR cloak config loaded (kv_store)\n"))
	passed++

	// 6. Sender key counts
	checks++
	db, _ := pgGet()
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var totalKeys int
		_ = pgQueryRow(ctx, db, `SELECT COUNT(*) FROM whatsmeow_sender_keys`).Scan(&totalKeys)
		sb.WriteString(fmt.Sprintf("✅ Sender keys in PG: %d rows\n", totalKeys))
		passed++
	}

	// 7. Metrics state
	checks++
	if m := sprMetricsSnapshot(); m != nil {
		sb.WriteString(fmt.Sprintf("✅ SPR metrics: %d sends recorded\n", m.Total))
		passed++
	}

	// 8. Archived messages count
	checks++
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var archiveCount int
		_ = pgQueryRow(ctx, db, `SELECT COUNT(*) FROM message_archive`).Scan(&archiveCount)
		sb.WriteString(fmt.Sprintf("✅ Archived messages: %d\n", archiveCount))
		passed++
	}

	sb.WriteString(fmt.Sprintf("\n*Summary:* %d/%d checks passed\n", passed, checks))
	return sb.String()
}

// sprBenchmark sends N SPR messages with target=[sender] + reports latency.
// Cap at 10 sends to prevent abuse.
func sprBenchmark(client *whatsmeow.Client, msg *events.Message, n int) string {
	if !msg.Info.IsGroup {
		return "❌ SPR hanya bekerja di grup"
	}
	if client == nil {
		return "❌ Client nil"
	}
	target := msg.Info.Sender.ToNonAD()
	if target.IsEmpty() || target.Server == "" {
		target = msg.Info.SenderAlt.ToNonAD()
	}
	if target.IsEmpty() {
		return "❌ Tidak bisa resolve sender JID"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🏁 *SPR Benchmark*\n\n• N=%d sends\n• Target=`%s` (mode=include)\n• Chat=`%s`\n\n",
		n, target.String(), msg.Info.Chat.String()))
	sb.WriteString("Running... (each send ~200-500ms)\n\n")

	text := getenvDefault("BOT_NAME", "Bot")
	var latencies []int64
	var okCount, failCount int
	var firstMsgID string

	for i := 0; i < n; i++ {
		sendText := fmt.Sprintf("%s (%d/%d)", text, i+1, n)

		participants, err := buildSPRParticipants(client, msg.Info.Chat, []types.JID{target})
		if err != nil || len(participants) == 0 {
			failCount++
			// Don't double-record: sendSPREngineAHide wasn't called yet,
			// so no auto-record happened. Record this as a bench-specific failure.
			sprMetricsRecord(sprMetricsEntry{
				GroupJID: msg.Info.Chat.String(),
				Target:   target.String(),
				Status:   "fail",
				Source:   "spr_bench",
				Error:    fmt.Sprintf("buildSPRParticipants: %v", err),
			})
			continue
		}
		waMsg := &waE2E.Message{Conversation: &sendText}
		started := time.Now()
		msgID, err := sendSPREngineAHide(client, msg.Info.Chat, participants, waMsg)
		elapsed := time.Since(started)
		if err != nil {
			failCount++
			// sendSPREngineAHide already auto-recorded this failure
			// (with source="spr_engine"). Don't double-record here.
			_ = msgID
			continue
		}
		okCount++
		latencies = append(latencies, elapsed.Milliseconds())
		if firstMsgID == "" {
			firstMsgID = msgID
		}
		// sendSPREngineAHide already auto-recorded this success
		// (with source="spr_engine"). Don't double-record here.
	}

	sb.WriteString("📊 *Results*\n\n")
	sb.WriteString(fmt.Sprintf("• Success: %d/%d\n", okCount, n))
	sb.WriteString(fmt.Sprintf("• Failed: %d\n", failCount))
	if len(latencies) > 0 {
		var min, max, sum int64
		min = latencies[0]
		for _, l := range latencies {
			sum += l
			if l < min {
				min = l
			}
			if l > max {
				max = l
			}
		}
		avg := sum / int64(len(latencies))
		sb.WriteString(fmt.Sprintf("• Latency min/avg/max: %dms / %dms / %dms\n", min, avg, max))
	}
	if firstMsgID != "" {
		sb.WriteString(fmt.Sprintf("\n• First msg ID: `%s`\n", firstMsgID))
	}
	sb.WriteString("\n_Stats also persisted to `.spr metrics` for cumulative view._")
	return sb.String()
}

// sprHistoryShow shows the last N SPR sends from PG audit_log.
func sprHistoryShow(limit int) string {
	db, err := pgGet()
	if err != nil {
		return fmt.Sprintf("❌ PG: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := pgQuery(ctx, db,
		`SELECT ts, jid, COALESCE(details::text,'{}') FROM audit_log
                 WHERE action = 'spr_send'
                 ORDER BY ts DESC, id DESC
                 LIMIT ?`, limit)
	if err != nil {
		return fmt.Sprintf("❌ query: %v", err)
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📜 *SPR Send History* (last %d)\n\n", limit))
	count := 0
	for rows.Next() {
		var ts time.Time
		var jid, detailsJSON string
		if err := rows.Scan(&ts, &jid, &detailsJSON); err != nil {
			continue
		}
		count++
		var d map[string]any
		_ = json.Unmarshal([]byte(detailsJSON), &d)
		target, _ := d["target"].(string)
		latency, _ := d["latency_ms"].(float64)
		status, _ := d["status"].(string)
		msgID, _ := d["msg_id"].(string)
		source, _ := d["source"].(string)
		icon := "✅"
		if status != "ok" {
			icon = "❌"
		}
		tsStr := ts.Format("02-01 15:04:05")
		shortGroup := jid
		if len(shortGroup) > 20 {
			shortGroup = shortGroup[:17] + "…"
		}
		shortTarget := target
		if len(shortTarget) > 20 {
			shortTarget = shortTarget[:17] + "…"
		}
		shortMsgID := msgID
		if len(shortMsgID) > 16 {
			shortMsgID = shortMsgID[:13] + "…"
		}
		sb.WriteString(fmt.Sprintf("%s [%s] %s → %s (%dms, %s)\n   msg=`%s`\n",
			icon, tsStr, shortGroup, shortTarget, int64(latency), source, shortMsgID))
	}
	if count == 0 {
		sb.WriteString("Belum ada SPR send tercatat.\n\nKirim `.spr` atau `.spr bench N` untuk mulai.")
	}
	return sb.String()
}

// sprProbeGroup runs a deep diagnostic for a group.
func sprProbeGroup(client *whatsmeow.Client, groupJID string) string {
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sb strings.Builder
	sb.WriteString("🔬 *SPR Group Probe*\n\n")

	// 1. Group info
	gi, err := client.GetGroupInfo(ctx, parsed)
	if err != nil {
		sb.WriteString(fmt.Sprintf("❌ GetGroupInfo: %v\n", err))
	} else {
		sb.WriteString(fmt.Sprintf("• Name: %s\n", gi.Name))
		sb.WriteString(fmt.Sprintf("• JID: `%s`\n", gi.JID.String()))
		sb.WriteString(fmt.Sprintf("• Owner: `%s`\n", gi.OwnerJID.String()))
		sb.WriteString(fmt.Sprintf("• Participants: %d\n", len(gi.Participants)))
		if gi.GroupEphemeral.IsEphemeral {
			sb.WriteString(fmt.Sprintf("• Ephemeral: %ds\n", gi.GroupEphemeral.DisappearingTimer))
		} else {
			sb.WriteString("• Ephemeral: OFF\n")
		}
		if !gi.GroupCreated.IsZero() {
			sb.WriteString(fmt.Sprintf("• Created: %s\n", gi.GroupCreated.Format("02-01-2006")))
		}
	}

	// 2. Sender keys in PG for this group
	db, _ := pgGet()
	if db != nil {
		qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer qcancel()
		var total, mine, isolated int
		_ = pgQueryRow(qctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ?`, gid).Scan(&total)
		if botClient != nil && botClient.Store != nil && botClient.Store.ID != nil {
			myUser := botClient.Store.ID.User
			_ = pgQueryRow(qctx, db,
				`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ? AND sender_id LIKE ?`,
				gid, myUser+"%").Scan(&mine)
		}
		if senderID, _ := sprIsolatedSenderID(); senderID != "" {
			_ = pgQueryRow(qctx, db,
				`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ? AND sender_id = ?`,
				gid, senderID).Scan(&isolated)
		}
		var newestTS time.Time
		_ = pgQueryRow(qctx, db,
			sprNewestKeySQL(),
			gid).Scan(&newestTS)
		var ageStr string
		if newestTS.IsZero() || newestTS.Equal(time.Unix(0, 0).UTC()) {
			ageStr = "(no keys yet)"
		} else {
			ageStr = fmt.Sprintf("%s ago (%s)", time.Since(newestTS).Round(time.Second), newestTS.Format("02-01 15:04"))
		}
		sb.WriteString(fmt.Sprintf("\n🔑 *Sender keys in PG:*\n• Total: %d\n• Mine: %d\n• Isolated (SPR): %d\n• Newest key age: %s\n",
			total, mine, isolated, ageStr))
	}

	// 3. Last SPR send to this group (from audit_log)
	if db != nil {
		qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer qcancel()
		var lastTS time.Time
		var lastDetails string
		_ = pgQueryRow(qctx, db,
			`SELECT ts, COALESCE(details::text,'{}') FROM audit_log WHERE action='spr_send' AND jid=? ORDER BY ts DESC LIMIT 1`,
			gid).Scan(&lastTS, &lastDetails)
		if lastTS.IsZero() {
			sb.WriteString("\n📜 Last SPR send: (none recorded)\n")
		} else {
			var d map[string]any
			_ = json.Unmarshal([]byte(lastDetails), &d)
			status, _ := d["status"].(string)
			latency, _ := d["latency_ms"].(float64)
			target, _ := d["target"].(string)
			source, _ := d["source"].(string)
			icon := "✅"
			if status != "ok" {
				icon = "❌"
			}
			sb.WriteString(fmt.Sprintf("\n📜 Last SPR send: %s\n  • When: %s\n  • Status: %s (%dms)\n  • Target: `%s`\n  • Source: %s\n",
				icon, lastTS.Format("02-01-2006 15:04:05"), status, int64(latency), target, source))
		}
	}

	// 4. Cloak config
	loadSPRCloak()
	if sprCloakEnabled() {
		sb.WriteString("\n🥷 Cloak: ENABLED (anti-detection ON)\n")
	} else {
		sb.WriteString("\n🥷 Cloak: DISABLED\n")
	}

	// 5. Recommendations
	var recs []string
	if db != nil {
		qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer qcancel()
		var staleCount int
		_ = pgQueryRow(qctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ? AND created_at < NOW() - INTERVAL '7 days'`,
			gid).Scan(&staleCount)
		if staleCount > 0 {
			recs = append(recs, fmt.Sprintf("• %d sender keys older than 7 days — consider rotating (.sprtest rotate %s)", staleCount, gid))
		}
	}
	if !sprCloakEnabled() {
		recs = append(recs, "• Cloak OFF — enable .sprcloak on for anti-detection in stealth groups")
	}
	if len(recs) > 0 {
		sb.WriteString("\n💡 *Recommendations:*\n")
		for _, r := range recs {
			sb.WriteString(r + "\n")
		}
	}

	return sb.String()
}

// sprAuditShow shows aggregate analytics from audit_log for SPR sends.
func sprAuditShow() string {
	db, err := pgGet()
	if err != nil {
		return fmt.Sprintf("❌ PG: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	sb.WriteString("📈 *SPR Audit Analytics*\n\n")

	var total, total24h, okCount, okCount24h int
	_ = pgQueryRow(ctx, db,
		`SELECT COUNT(*) FROM audit_log WHERE action='spr_send'`).Scan(&total)
	_ = pgQueryRow(ctx, db,
		`SELECT COUNT(*) FROM audit_log WHERE action='spr_send' AND ts >= NOW() - INTERVAL '24 hours'`).Scan(&total24h)
	_ = pgQueryRow(ctx, db,
		`SELECT COUNT(*) FROM audit_log WHERE action='spr_send' AND details->>'status' = 'ok'`).Scan(&okCount)
	_ = pgQueryRow(ctx, db,
		`SELECT COUNT(*) FROM audit_log WHERE action='spr_send' AND details->>'status' = 'ok' AND ts >= NOW() - INTERVAL '24 hours'`).Scan(&okCount24h)

	successRate := float64(0)
	if total > 0 {
		successRate = float64(okCount) * 100 / float64(total)
	}
	successRate24h := float64(0)
	if total24h > 0 {
		successRate24h = float64(okCount24h) * 100 / float64(total24h)
	}
	sb.WriteString(fmt.Sprintf("📊 *Volume:*\n• Total sends: %d (all-time)\n• Last 24h: %d sends\n• Success rate: %.1f%% (all-time), %.1f%% (24h)\n\n",
		total, total24h, successRate, successRate24h))

	// Error class breakdown
	rows, err := pgQuery(ctx, db,
		`SELECT COALESCE(details->>'error_class','unknown') AS cls, COUNT(*) AS c
                 FROM audit_log
                 WHERE action='spr_send' AND details->>'status' = 'fail'
                 GROUP BY cls ORDER BY c DESC`)
	if err == nil {
		defer rows.Close()
		var hasErrors bool
		for rows.Next() {
			if !hasErrors {
				sb.WriteString("❌ *Error breakdown:*\n")
				hasErrors = true
			}
			var cls string
			var c int
			_ = rows.Scan(&cls, &c)
			pct := float64(0)
			if total > 0 {
				pct = float64(c) * 100 / float64(total)
			}
			sb.WriteString(fmt.Sprintf("• %s: %d (%.1f%%)\n", cls, c, pct))
		}
		if !hasErrors {
			sb.WriteString("✅ No failed sends recorded\n")
		}
	}

	// Top 5 groups by SPR send count
	rows2, err := pgQuery(ctx, db,
		`SELECT jid, COUNT(*) AS c FROM audit_log WHERE action='spr_send' GROUP BY jid ORDER BY c DESC LIMIT 5`)
	if err == nil {
		defer rows2.Close()
		count := 0
		for rows2.Next() {
			if count == 0 {
				sb.WriteString("\n📍 *Top groups by send count:*\n")
			}
			var jid string
			var c int
			_ = rows2.Scan(&jid, &c)
			count++
			short := jid
			if len(short) > 30 {
				short = short[:27] + "…"
			}
			sb.WriteString(fmt.Sprintf("• `%s` — %d sends\n", short, c))
		}
	}

	// Top 5 failing groups
	rows3, err := pgQuery(ctx, db,
		`SELECT jid, COUNT(*) AS c FROM audit_log
                 WHERE action='spr_send' AND details->>'status' = 'fail'
                 GROUP BY jid ORDER BY c DESC LIMIT 5`)
	if err == nil {
		defer rows3.Close()
		count := 0
		for rows3.Next() {
			if count == 0 {
				sb.WriteString("\n❌ *Top failing groups:*\n")
			}
			var jid string
			var c int
			_ = rows3.Scan(&jid, &c)
			count++
			short := jid
			if len(short) > 30 {
				short = short[:27] + "…"
			}
			sb.WriteString(fmt.Sprintf("• `%s` — %d failures\n", short, c))
		}
	}

	// Latency stats
	var avgLatency, p50Latency, p95Latency, p99Latency, minLatency, maxLatency float64
	_ = pgQueryRow(ctx, db,
		`SELECT COALESCE(AVG((details->>'latency_ms')::float8), 0) FROM audit_log
                 WHERE action='spr_send' AND details->>'status' = 'ok' AND (details->>'latency_ms') IS NOT NULL`).Scan(&avgLatency)
	_ = pgQueryRow(ctx, db, sprPercentileSQL(0.5)).Scan(&p50Latency)
	_ = pgQueryRow(ctx, db, sprPercentileSQL(0.95)).Scan(&p95Latency)
	_ = pgQueryRow(ctx, db, sprPercentileSQL(0.99)).Scan(&p99Latency)
	_ = pgQueryRow(ctx, db,
		`SELECT COALESCE(MIN((details->>'latency_ms')::float8), 0) FROM audit_log
                 WHERE action='spr_send' AND details->>'status' = 'ok' AND (details->>'latency_ms') IS NOT NULL`).Scan(&minLatency)
	_ = pgQueryRow(ctx, db,
		`SELECT COALESCE(MAX((details->>'latency_ms')::float8), 0) FROM audit_log
                 WHERE action='spr_send' AND details->>'status' = 'ok' AND (details->>'latency_ms') IS NOT NULL`).Scan(&maxLatency)
	if avgLatency > 0 {
		sb.WriteString(fmt.Sprintf("\n⏱️ *Latency (ok sends only):*\n• Min: %.0fms\n• Avg: %.0fms\n• p50: %.0fms\n• p95: %.0fms\n• p99: %.0fms\n• Max: %.0fms\n",
			minLatency, avgLatency, p50Latency, p95Latency, p99Latency, maxLatency))
	}

	// Per-source breakdown
	rows4, err := pgQuery(ctx, db,
		`SELECT COALESCE(details->>'source','unknown') AS src, COUNT(*) AS c
                 FROM audit_log WHERE action='spr_send' GROUP BY src ORDER BY c DESC`)
	if err == nil {
		defer rows4.Close()
		count := 0
		for rows4.Next() {
			if count == 0 {
				sb.WriteString("\n🔍 *Per-source:*\n")
			}
			var src string
			var c int
			_ = rows4.Scan(&src, &c)
			count++
			sb.WriteString(fmt.Sprintf("• %s: %d\n", src, c))
		}
	}

	if total == 0 {
		sb.WriteString("\nBelum ada SPR send tercatat di audit_log.\nKirim `.spr` untuk mulai mencatat.")
	}

	return sb.String()
}

// sprTraceMsg shows end-to-end trace for one SPR-sent message.
func sprTraceMsg(msgID string) string {
	if msgID == "" {
		return "❌ Pakai: .spr trace <msg_id>"
	}
	db, err := pgGet()
	if err != nil {
		return fmt.Sprintf("❌ PG: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔎 *SPR Message Trace*\n\n• Msg ID: `%s`\n\n", msgID))

	var ts time.Time
	var jid, detailsJSON string
	err = pgQueryRow(ctx, db,
		`SELECT ts, jid, COALESCE(details::text,'{}') FROM audit_log
                 WHERE action='spr_send' AND details->>'msg_id' = ?
                 ORDER BY ts DESC LIMIT 1`, msgID).Scan(&ts, &jid, &detailsJSON)
	if err == nil {
		var d map[string]any
		_ = json.Unmarshal([]byte(detailsJSON), &d)
		sb.WriteString("📤 *Send event:*\n")
		sb.WriteString(fmt.Sprintf("• When: %s\n", ts.Format("02-01-2006 15:04:05")))
		sb.WriteString(fmt.Sprintf("• Group: `%s`\n", jid))
		if t, ok := d["target"].(string); ok {
			sb.WriteString(fmt.Sprintf("• Target: `%s`\n", t))
		}
		if s, ok := d["source"].(string); ok {
			sb.WriteString(fmt.Sprintf("• Source: %s\n", s))
		}
		if s, ok := d["status"].(string); ok {
			sb.WriteString(fmt.Sprintf("• Status: %s\n", s))
		}
		if l, ok := d["latency_ms"].(float64); ok {
			sb.WriteString(fmt.Sprintf("• Latency: %.0fms\n", l))
		}
		if ec, ok := d["error_class"].(string); ok {
			sb.WriteString(fmt.Sprintf("• Error class: %s\n", ec))
		}
		if e, ok := d["error"].(string); ok && e != "" {
			sb.WriteString(fmt.Sprintf("• Error: %s\n", e))
		}
	} else {
		sb.WriteString("📭 No send event found in audit_log for this msg_id\n\n")
	}

	// Find archived envelope
	var archivedTS time.Time
	var envelopeJSON string
	err = pgQueryRow(ctx, db,
		`SELECT created_at, COALESCE(envelope::text,'{}') FROM message_archive
                 WHERE msg_id = ? ORDER BY id DESC LIMIT 1`, msgID).Scan(&archivedTS, &envelopeJSON)
	if err == nil && envelopeJSON != "" && envelopeJSON != "{}" {
		sb.WriteString(fmt.Sprintf("\n📦 *Archived envelope:*\n• Archived at: %s\n", archivedTS.Format("02-01-2006 15:04:05")))
		var env map[string]any
		if json.Unmarshal([]byte(envelopeJSON), &env) == nil {
			if v, ok := env["msg_id"].(string); ok {
				sb.WriteString(fmt.Sprintf("• Envelope msg_id: `%s`\n", v))
			}
			if v, ok := env["sender"].(string); ok {
				sb.WriteString(fmt.Sprintf("• Sender: `%s`\n", v))
			}
			if v, ok := env["message_type"].(string); ok {
				sb.WriteString(fmt.Sprintf("• Type: %s\n", v))
			}
			if v, ok := env["participant"].(string); ok && v != "" {
				sb.WriteString(fmt.Sprintf("• Participant: `%s`\n", v))
			}
			if v, ok := env["addressing_mode"].(string); ok && v != "" {
				sb.WriteString(fmt.Sprintf("• Addressing: %s\n", v))
			}
			if v, ok := env["text"].(string); ok && v != "" {
				t := v
				if len(t) > 80 {
					t = t[:77] + "…"
				}
				sb.WriteString(fmt.Sprintf("• Text: %s\n", t))
			}
			if v, ok := env["broadcast"].(bool); ok && v {
				sb.WriteString("• Broadcast: yes\n")
			}
			if na, ok := env["node_attrs"].(map[string]any); ok && len(na) > 0 {
				sb.WriteString(fmt.Sprintf("• Node attrs: %d keys\n", len(na)))
				var keys []string
				for k := range na {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				preview := strings.Join(keys, ", ")
				if len(preview) > 80 {
					preview = preview[:77] + "…"
				}
				sb.WriteString(fmt.Sprintf("  (%s)\n", preview))
			}
		}
	} else {
		sb.WriteString("\n📭 Message not in archive (may have been purged or sent before archive v2)\n")
	}

	return sb.String()
}

// sprKeysBrowser browses sender keys in PG, optionally filtered by group.
func sprKeysBrowser(groupJID string) string {
	db, err := pgGet()
	if err != nil {
		return fmt.Sprintf("❌ PG: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	if groupJID != "" {
		gid := strings.Split(groupJID, ":")[0]
		if !strings.Contains(gid, "@") {
			gid = gid + "@g.us"
		}
		sb.WriteString(fmt.Sprintf("🔑 *Sender Keys for %s*\n\n", gid))

		var total int
		_ = pgQueryRow(ctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE chat_id = ?`, gid).Scan(&total)
		sb.WriteString(fmt.Sprintf("• Total rows: %d\n", total))

		rows, err := pgQuery(ctx, db,
			`SELECT sender_id, COUNT(*) AS c, MAX(created_at) AS newest
                         FROM whatsmeow_sender_keys WHERE chat_id = ?
                         GROUP BY sender_id ORDER BY c DESC LIMIT 10`, gid)
		if err == nil {
			defer rows.Close()
			count := 0
			for rows.Next() {
				if count == 0 {
					sb.WriteString("\n📊 *Top senders (by key count):*\n")
				}
				var sid string
				var c int
				var newest time.Time
				_ = rows.Scan(&sid, &c, &newest)
				count++
				short := sid
				if len(short) > 30 {
					short = short[:27] + "…"
				}
				ageStr := "n/a"
				if !newest.IsZero() {
					ageStr = time.Since(newest).Round(time.Second).String() + " ago"
				}
				sb.WriteString(fmt.Sprintf("• `%s` — %d keys (newest: %s)\n", short, c, ageStr))
			}
		}
	} else {
		sb.WriteString("🔑 *Sender Keys (global)*\n\n")
		var grandTotal int
		_ = pgQueryRow(ctx, db, `SELECT COUNT(*) FROM whatsmeow_sender_keys`).Scan(&grandTotal)
		sb.WriteString(fmt.Sprintf("• Total rows: %d\n", grandTotal))

		rows, err := pgQuery(ctx, db,
			`SELECT chat_id, COUNT(*) AS c, MAX(created_at) AS newest
                         FROM whatsmeow_sender_keys
                         GROUP BY chat_id ORDER BY c DESC LIMIT 10`)
		if err == nil {
			defer rows.Close()
			count := 0
			for rows.Next() {
				if count == 0 {
					sb.WriteString("\n📍 *Top groups:*\n")
				}
				var cid string
				var c int
				var newest time.Time
				_ = rows.Scan(&cid, &c, &newest)
				count++
				short := cid
				if len(short) > 30 {
					short = short[:27] + "…"
				}
				ageStr := "n/a"
				if !newest.IsZero() {
					ageStr = time.Since(newest).Round(time.Hour).String() + " ago"
				}
				sb.WriteString(fmt.Sprintf("• `%s` — %d keys (newest: %s)\n", short, c, ageStr))
			}
		}

		var freshCount, oldCount, staleCount int
		_ = pgQueryRow(ctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE created_at >= NOW() - INTERVAL '1 day'`).Scan(&freshCount)
		_ = pgQueryRow(ctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE created_at < NOW() - INTERVAL '1 day' AND created_at >= NOW() - INTERVAL '7 days'`).Scan(&oldCount)
		_ = pgQueryRow(ctx, db,
			`SELECT COUNT(*) FROM whatsmeow_sender_keys WHERE created_at < NOW() - INTERVAL '7 days'`).Scan(&staleCount)
		sb.WriteString(fmt.Sprintf("\n⏰ *Age distribution:*\n• <1 day: %d\n• 1-7 days: %d\n• >7 days (stale): %d\n",
			freshCount, oldCount, staleCount))
	}
	return sb.String()
}

// sprResetMetrics clears the cumulative SPR metrics.
// Does NOT touch audit_log (per-send history preserved).
func sprResetMetrics() string {
	sprMetricsMu.Lock()
	defer sprMetricsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	empty := sprMetricsState{
		PerSource:     map[string]int{},
		PerErrorClass: map[string]int{},
		PerGroup:      map[string]int{},
	}
	if err := kvStoreSet(ctx, sprMetricsKVKey, &empty); err != nil {
		return fmt.Sprintf("❌ reset: %v", err)
	}
	return "🗑️ SPR metrics direset. (audit_log per-send history tetap utuh — kirim `.spr audit` untuk lihat)"
}

// ─── JSON helpers (panic-proof) ─────────────────────────────────

// sprJSONMarshalSafe marshals v to JSON string, returning "{}" on error.
func sprJSONMarshalSafe(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	s := string(b)
	if !json.Valid([]byte(s)) {
		return "{}"
	}
	return s
}

// ─── Subcommand dispatcher for .spr (called from spr.go's handleSPRCommand) ──

// sprHandleDiagnosticSubcommand checks if args starts with a known
// diagnostic subcommand keyword and dispatches accordingly.
//
// Returns (output, true) if matched and consumed; returns ("", false)
// if not matched (caller should proceed with normal SPR send).
//
// This is called from spr.go's handleSPRCommand BEFORE the existing
// target-parsing logic, so existing .spr behavior is unchanged for
// non-diagnostic invocations.
func sprHandleDiagnosticSubcommand(client *whatsmeow.Client, msg *events.Message, args string) (string, bool) {
	parts := strings.Fields(args)
	if len(parts) == 0 {
		return "", false
	}
	sub := strings.ToLower(parts[0])
	switch sub {
	case "metrics":
		return sprShowMetrics(), true
	case "audit":
		return sprAuditShow(), true
	case "health":
		return sprHealthCheck(client), true
	case "probe":
		groupJID := ""
		if len(parts) >= 2 {
			groupJID = parts[1]
		} else if msg != nil {
			groupJID = msg.Info.Chat.String()
		}
		return sprProbeGroup(client, groupJID), true
	case "trace":
		if len(parts) < 2 {
			return "❌ Pakai: .spr trace <msg_id>", true
		}
		return sprTraceMsg(parts[1]), true
	case "keys":
		groupJID := ""
		if len(parts) >= 2 {
			groupJID = parts[1]
		}
		return sprKeysBrowser(groupJID), true
	case "reset-metrics", "resetmetrics":
		return sprResetMetrics(), true
	case "history":
		limit := 10
		if len(parts) >= 2 {
			fmt.Sscanf(parts[1], "%d", &limit)
			if limit < 1 {
				limit = 10
			}
			if limit > 50 {
				limit = 50
			}
		}
		return sprHistoryShow(limit), true
	case "bench":
		n := 1
		if len(parts) >= 2 {
			fmt.Sscanf(parts[1], "%d", &n)
			if n < 1 {
				n = 1
			}
			if n > 10 {
				n = 10
			}
		}
		return sprBenchmark(client, msg, n), true
	}
	return "", false
}
