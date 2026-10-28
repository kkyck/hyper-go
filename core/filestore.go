package main

/**
 * 📁 filestore.go — Generic JSON file-based storage
 * ════════════════════════════════════════════════════════════
 * Replaces MongoDB collections with simple JSON files in ./data/ directory.
 *
 * Files:
 *   ./data/group_guard.json   → { "<groupJID>": GuardDoc }
 *   ./data/afk.json          → { "<jid>": AFKEntry }
 *   ./data/sticker-store.json→ { "<sender>": StickerUserStore }
 *   ./data/selfmode.json     → SelfModeState
 *
 * Concurrency: per-file mutex; atomic write (tmp file + rename).
 */

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// dataDir is the base directory for all persistent JSON files.
const dataDir = "./data"

var (
	fsMu   sync.Mutex
	fsLock = make(map[string]*sync.Mutex)
)

func init() {
	_ = os.MkdirAll(dataDir, 0o755)
}

// fileMu returns a per-file mutex so concurrent calls on the same file are serialized,
// but different files can be written in parallel.
func fileMu(path string) *sync.Mutex {
	fsMu.Lock()
	defer fsMu.Unlock()
	if m, ok := fsLock[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	fsLock[path] = m
	return m
}

// loadJSON loads a JSON file into the given pointer. If file missing or empty,
// `def` is left untouched (caller should initialize default).
func loadJSON(path string, dst any) error {
	mu := fileMu(path)
	mu.Lock()
	defer mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// saveJSON writes data as pretty JSON atomically (tmp file + rename).
func saveJSON(path string, data any) error {
	mu := fileMu(path)
	mu.Lock()
	defer mu.Unlock()

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ─── Guard file ───────────────────────────────────────────────────

const guardFile = dataDir + "/group_guard.json"

var guardStore = map[string]*GuardDoc{}
var guardStoreMu sync.RWMutex

func loadGuardsFromDisk() {
	guardStoreMu.Lock()
	defer guardStoreMu.Unlock()
	_ = loadJSON(guardFile, &guardStore)
	if guardStore == nil {
		guardStore = map[string]*GuardDoc{}
	}
}

func persistGuards() error {
	guardStoreMu.RLock()
	defer guardStoreMu.RUnlock()
	return saveJSON(guardFile, guardStore)
}

func getGroupGuard(groupJID string) *GuardDoc {
	guardStoreMu.Lock()
	defer guardStoreMu.Unlock()
	doc, ok := guardStore[groupJID]
	if !ok {
		doc = &GuardDoc{
			GroupJID: groupJID,
			Settings: defaultGuardSettings(),
		}
		guardStore[groupJID] = doc
	}
	return doc
}

// ─── AFK file ─────────────────────────────────────────────────────

const afkFile = dataDir + "/afk.json"

var afkStore = map[string]*AFKEntry{}
var afkStoreMu sync.RWMutex

func loadAFKsFromDisk() {
	afkStoreMu.Lock()
	defer afkStoreMu.Unlock()
	_ = loadJSON(afkFile, &afkStore)
	if afkStore == nil {
		afkStore = map[string]*AFKEntry{}
	}
}

func persistAFKs() error {
	afkStoreMu.RLock()
	defer afkStoreMu.RUnlock()
	return saveJSON(afkFile, afkStore)
}

// getAFK returns the AFK entry for a jid, or nil if not AFK.
func getAFK(jid string) *AFKEntry {
	afkStoreMu.RLock()
	defer afkStoreMu.RUnlock()
	return afkStore[jid]
}

func setAFK(e *AFKEntry) {
	afkStoreMu.Lock()
	defer afkStoreMu.Unlock()
	afkStore[e.JID] = e
}

func deleteAFKEntry(jid string) {
	afkStoreMu.Lock()
	defer afkStoreMu.Unlock()
	delete(afkStore, jid)
}

func allAFKs() []*AFKEntry {
	afkStoreMu.RLock()
	defer afkStoreMu.RUnlock()
	out := make([]*AFKEntry, 0, len(afkStore))
	for _, e := range afkStore {
		out = append(out, e)
	}
	return out
}

// ─── All-purpose JSON-store helpers used by Kangwifi cache & misc ──

// SimpleKV is a flat JSON object store under ./data/kv.json for misc persistence.
type SimpleKV struct {
	mu   sync.RWMutex
	data map[string]any
	path string
}

var globalKV *SimpleKV

func initGlobalKV() {
	kv := &SimpleKV{
		data: map[string]any{},
		path: dataDir + "/kv.json",
	}
	_ = loadJSON(kv.path, &kv.data)
	if kv.data == nil {
		kv.data = map[string]any{}
	}
	globalKV = kv
}

func (kv *SimpleKV) Get(key string) any {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return kv.data[key]
}

func (kv *SimpleKV) Set(key string, val any) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.data[key] = val
	_ = saveJSON(kv.path, kv.data)
}

func (kv *SimpleKV) Delete(key string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.data, key)
	_ = saveJSON(kv.path, kv.data)
}

func (kv *SimpleKV) Keys() []string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	keys := make([]string, 0, len(kv.data))
	for k := range kv.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// helper for printing
func ppJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

var _ = fmt.Sprintf // keep fmt import used
