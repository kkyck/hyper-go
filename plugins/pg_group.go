package main

// pg_group.go — PostgreSQL helpers for per-group per-plugin settings.
// ════════════════════════════════════════════════════════════
//
// Generic key-value-per-group-per-plugin API used by 7+ plugins
// that previously stored per-group toggle state in JSON files.
//
// Replaces:  autoai.json, autodel_groups.json, crm_groups.json,
//            selfspr_groups.json, spr_cloak.json, stealth_primary.json,
//            aiban.json.
//
// Schema: see group_settings table in pg.go pgInitAll.
//
// Public API:
//   pgGroupGetBool(ctx, plugin, groupJID, default) bool
//   pgGroupSetBool(ctx, plugin, groupJID, val) error
//   pgGroupGetConfig(ctx, plugin, groupJID) (map[string]any, error)
//   pgGroupSetConfig(ctx, plugin, groupJID, config) error
//   pgGroupListEnabled(ctx, plugin) ([]string, error)
//   pgGroupDelete(ctx, plugin, groupJID) error
//
// Each call is wrapped with a 5s timeout. On PG failure, helpers
// return the default value (GetBool) or an empty result — they
// NEVER panic, so plugin callers can use them inline safely.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// pgGroupGetBool returns the enabled flag for a plugin in a group.
// If no row exists, returns def (the caller's chosen default).
// On PG error, returns def and logs (does not panic).
func pgGroupGetBool(ctx context.Context, plugin, groupJID string, def bool) bool {
	db, err := pgGet()
	if err != nil {
		return def
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var enabled bool
	err = pgQueryRow(qctx, db,
		`SELECT enabled FROM group_settings WHERE group_jid = ? AND plugin = ?`,
		groupJID, plugin).Scan(&enabled)
	if err != nil {
		return def // not found or error → use default
	}
	return enabled
}

// pgGroupSetBool upserts the enabled flag for a plugin in a group.
// Creates a row if not exists, updates if exists.
func pgGroupSetBool(ctx context.Context, plugin, groupJID string, val bool) error {
	db, err := pgGet()
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = pgExec(qctx, db,
		`INSERT INTO group_settings (group_jid, plugin, enabled, config)
                 VALUES (?, ?, ?, '{}'::jsonb)
                 ON CONFLICT (group_jid, plugin) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()`,
		groupJID, plugin, val)
	if err != nil {
		return fmt.Errorf("pgGroupSetBool: %w", err)
	}
	return nil
}

// pgGroupGetConfig returns the JSONB config map for a plugin in a group.
// Returns empty map if no row. Config is the optional extra data beyond
// the boolean toggle.
func pgGroupGetConfig(ctx context.Context, plugin, groupJID string) (map[string]any, error) {
	db, err := pgGet()
	if err != nil {
		return map[string]any{}, err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var configJSON string
	err = pgQueryRow(qctx, db,
		`SELECT COALESCE(config::text, '{}') FROM group_settings WHERE group_jid = ? AND plugin = ?`,
		groupJID, plugin).Scan(&configJSON)
	if err != nil {
		return map[string]any{}, nil // not found = empty config
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(configJSON), &out); err != nil {
		return map[string]any{}, nil
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

// pgGroupSetConfig upserts the JSONB config for a plugin in a group.
// Preserves the existing enabled flag (config is independent of toggle).
func pgGroupSetConfig(ctx context.Context, plugin, groupJID string, config map[string]any) error {
	db, err := pgGet()
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	jsonBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	jsonStr := string(jsonBytes)
	if !json.Valid([]byte(jsonStr)) {
		jsonStr = "{}"
	}
	_, err = pgExec(qctx, db,
		`INSERT INTO group_settings (group_jid, plugin, enabled, config)
                 VALUES (?, ?, false, ?::jsonb)
                 ON CONFLICT (group_jid, plugin) DO UPDATE SET config = EXCLUDED.config, updated_at = NOW()`,
		groupJID, plugin, jsonStr)
	if err != nil {
		return fmt.Errorf("pgGroupSetConfig: %w", err)
	}
	return nil
}

// pgGroupSetBoth upserts both enabled + config in one call.
// Use this when you need to update both at once (atomic).
func pgGroupSetBoth(ctx context.Context, plugin, groupJID string, enabled bool, config map[string]any) error {
	db, err := pgGet()
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	jsonBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	jsonStr := string(jsonBytes)
	if !json.Valid([]byte(jsonStr)) {
		jsonStr = "{}"
	}
	_, err = pgExec(qctx, db,
		`INSERT INTO group_settings (group_jid, plugin, enabled, config)
                 VALUES (?, ?, ?, ?::jsonb)
                 ON CONFLICT (group_jid, plugin) DO UPDATE
                   SET enabled = EXCLUDED.enabled, config = EXCLUDED.config, updated_at = NOW()`,
		groupJID, plugin, enabled, jsonStr)
	if err != nil {
		return fmt.Errorf("pgGroupSetBoth: %w", err)
	}
	return nil
}

// pgGroupListEnabled returns the list of group JIDs where the plugin is enabled.
// Useful for status commands that show "where is this feature on?"
func pgGroupListEnabled(ctx context.Context, plugin string) ([]string, error) {
	db, err := pgGet()
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := pgQuery(qctx, db,
		`SELECT group_jid FROM group_settings WHERE plugin = ? AND enabled = true ORDER BY group_jid`,
		plugin)
	if err != nil {
		return nil, fmt.Errorf("pgGroupListEnabled: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			continue
		}
		out = append(out, jid)
	}
	return out, nil
}

// pgGroupDelete removes the row for a plugin in a group.
// Useful for "reset to default" commands.
func pgGroupDelete(ctx context.Context, plugin, groupJID string) error {
	db, err := pgGet()
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = pgExec(qctx, db,
		`DELETE FROM group_settings WHERE group_jid = ? AND plugin = ?`,
		groupJID, plugin)
	return err
}

// ─── One-shot migration helper ──────────────────────────────────

// pgGroupMigrateFromMap is a one-shot migration helper for plugins
// migrating from in-memory map[string]bool to PG.
//   - existing[key] = the value to migrate
//   - valueIsDisabled = true means the map tracks "disabled" groups
//     (default = ON). The stored boolean is INVERTED before write.
//     If false, the map's bool value is stored as-is.
//
// Idempotent: only writes rows for keys not yet in PG. Safe to call
// multiple times (e.g., on every restart).
func pgGroupMigrateFromMap(plugin string, existing map[string]bool, valueIsDisabled bool) error {
	if len(existing) == 0 {
		return nil
	}
	db, err := pgGet()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for k, v := range existing {
		// Skip if row already exists (don't overwrite user's later changes)
		var exists bool
		_ = pgQueryRow(ctx, db,
			`SELECT EXISTS (SELECT 1 FROM group_settings WHERE group_jid = ? AND plugin = ?)`,
			k, plugin).Scan(&exists)
		if exists {
			continue
		}
		// Convert: if map value tracks "disabled", invert for storage.
		// Stored "enabled" = NOT(map value) when valueIsDisabled=true.
		stored := v
		if valueIsDisabled {
			stored = !v
		}
		if err := pgGroupSetBool(ctx, plugin, k, stored); err != nil {
			return err
		}
	}
	return nil
}

// ─── kv_store helpers (for global config / non-per-group data) ──────
//
// kv_store is a simple TEXT→JSONB table. Used by plugins that have
// a single global config blob (not per-group): spr_cloak, ai_feedback,
// ai_call global settings, bot_training, bot_patterns, etc.

// kvStoreGet returns the JSONB value for a key, unmarshaled into v.
// v must be a pointer to a struct/map/slice. Returns false if no row
// or unmarshal failed — caller uses defaults in that case.
func kvStoreGet(ctx context.Context, key string, v any) bool {
	db, err := pgGet()
	if err != nil {
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var jsonStr string
	err = pgQueryRow(qctx, db,
		`SELECT value::text FROM kv_store WHERE key = ?`,
		key).Scan(&jsonStr)
	if err != nil || jsonStr == "" || jsonStr == "null" {
		return false
	}
	if err := json.Unmarshal([]byte(jsonStr), v); err != nil {
		return false
	}
	return true
}

// kvStoreSet marshals v as JSONB and stores under key. UPSERT semantics.
func kvStoreSet(ctx context.Context, key string, v any) error {
	db, err := pgGet()
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	jsonBytes, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	jsonStr := string(jsonBytes)
	if !json.Valid([]byte(jsonStr)) {
		jsonStr = "{}"
	}
	_, err = pgExec(qctx, db,
		`INSERT INTO kv_store (key, value, updated_at) VALUES (?, ?::jsonb, EXTRACT(EPOCH FROM NOW())::BIGINT)
                 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		key, jsonStr)
	if err != nil {
		return fmt.Errorf("kvStoreSet: %w", err)
	}
	return nil
}

// kvStoreDelete removes a key from kv_store.
func kvStoreDelete(ctx context.Context, key string) error {
	db, err := pgGet()
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = pgExec(qctx, db, `DELETE FROM kv_store WHERE key = ?`, key)
	return err
}

// ─── pgMapJSONB — generic per-group state map backed by PG ────────
//
// For plugins that keep a per-group state struct in memory and need
// to persist each group's state individually. The map provides:
//   - Load on first access (lazy)
//   - Get/Set/Delete with in-memory cache (fast read)
//   - Write-through to PG on every Set/Delete (durable)
//
// Schema: group_settings (group_jid, plugin, enabled, config JSONB).
// The map's "enabled" boolean is derived from a caller-supplied predicate
// (so each plugin can define what "enabled" means for its state struct).

// pgMapEntry is one row in the per-group state map.
// State is the plugin-specific struct (caller-defined).
// We persist it as JSONB under the `config` column.
type pgMapEntry[T any] struct {
	Enabled bool `json:"enabled"`
	State   T    `json:"state"`
}

// pgMapJSONB is a generic per-group state map backed by PG.
// The plugin defines the state type T; the map handles PG load + write-through.
type pgMapJSONB[T any] struct {
	plugin  string
	mu      sync.RWMutex
	entries map[string]*pgMapEntry[T]
	loaded  bool
	// enabledFn extracts the boolean "enabled" flag from state T.
	// If nil, defaults to true (always enabled when row exists).
	enabledFn func(T) bool
}

// newPgMapJSONB creates a new per-group state map for the given plugin.
// enabledFn is used to extract the "enabled" boolean from the state struct.
func newPgMapJSONB[T any](plugin string, enabledFn func(T) bool) *pgMapJSONB[T] {
	return &pgMapJSONB[T]{
		plugin:    plugin,
		entries:   map[string]*pgMapEntry[T]{},
		enabledFn: enabledFn,
	}
}

// load pulls all rows for this plugin from PG into the in-memory map.
// Idempotent — only runs once (subsequent calls are no-ops).
func (m *pgMapJSONB[T]) load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded {
		return
	}
	m.loaded = true
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pgQuery(ctx, db,
		`SELECT group_jid, enabled, COALESCE(config::text,'{}') FROM group_settings WHERE plugin = ?`,
		m.plugin)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var jid, configJSON string
		var enabled bool
		if err := rows.Scan(&jid, &enabled, &configJSON); err != nil {
			continue
		}
		// The state is stored as a JSONB object inside `config` under key "state".
		// Try to unmarshal as {state: T} first; if that fails, unmarshal directly as T.
		var wrapper struct {
			State T `json:"state"`
		}
		var entry pgMapEntry[T]
		entry.Enabled = enabled
		if err := json.Unmarshal([]byte(configJSON), &wrapper); err == nil {
			entry.State = wrapper.State
		} else if err := json.Unmarshal([]byte(configJSON), &entry.State); err == nil {
			// state is the entire config JSON
		}
		m.entries[jid] = &entry
	}
}

// get returns the state pointer for a group, or nil if not set.
func (m *pgMapJSONB[T]) get(groupJID string) *T {
	m.load()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.entries[groupJID]; ok {
		return &e.State
	}
	return nil
}

// getEntry returns the full entry (enabled + state) for a group.
func (m *pgMapJSONB[T]) getEntry(groupJID string) (enabled bool, state *T) {
	m.load()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.entries[groupJID]; ok {
		return e.Enabled, &e.State
	}
	return false, nil
}

// set stores the state for a group + writes through to PG.
// The enabled flag is computed from enabledFn(state) (if set).
func (m *pgMapJSONB[T]) set(groupJID string, state T) {
	m.load()
	m.mu.Lock()
	defer m.mu.Unlock()
	enabled := true
	if m.enabledFn != nil {
		enabled = m.enabledFn(state)
	}
	entry := &pgMapEntry[T]{Enabled: enabled, State: state}
	m.entries[groupJID] = entry
	go m.persist(groupJID, entry)
}

// setEnabled sets the enabled flag for a group (state unchanged).
func (m *pgMapJSONB[T]) setEnabled(groupJID string, enabled bool) {
	m.load()
	m.mu.Lock()
	defer m.mu.Unlock()
	var st T
	if e, ok := m.entries[groupJID]; ok {
		st = e.State
	}
	entry := &pgMapEntry[T]{Enabled: enabled, State: st}
	m.entries[groupJID] = entry
	go m.persist(groupJID, entry)
}

// delete removes the entry for a group (in-memory + PG).
func (m *pgMapJSONB[T]) delete(groupJID string) {
	m.load()
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, groupJID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pgGroupDelete(ctx, m.plugin, groupJID)
	}()
}

// list returns all group JIDs in the map (regardless of enabled).
func (m *pgMapJSONB[T]) list() []string {
	m.load()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.entries))
	for k := range m.entries {
		out = append(out, k)
	}
	return out
}

// listEnabled returns group JIDs where enabled=true.
func (m *pgMapJSONB[T]) listEnabled() []string {
	m.load()
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for k, e := range m.entries {
		if e.Enabled {
			out = append(out, k)
		}
	}
	return out
}

// persist writes one group's state to PG (write-through, async).
func (m *pgMapJSONB[T]) persist(groupJID string, entry *pgMapEntry[T]) {
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Wrap state under "state" key for forward compat (other plugins' fields)
	wrapper := map[string]any{"state": entry.State}
	jsonBytes, err := json.Marshal(wrapper)
	if err != nil {
		return
	}
	jsonStr := string(jsonBytes)
	if !json.Valid([]byte(jsonStr)) {
		jsonStr = "{}"
	}
	_, _ = pgExec(ctx, db,
		`INSERT INTO group_settings (group_jid, plugin, enabled, config)
                 VALUES (?, ?, ?, ?::jsonb)
                 ON CONFLICT (group_jid, plugin) DO UPDATE
                   SET enabled = EXCLUDED.enabled, config = EXCLUDED.config, updated_at = NOW()`,
		groupJID, m.plugin, entry.Enabled, jsonStr)
}
