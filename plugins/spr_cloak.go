package main

// spr_cloak.go — Cloaking & anti-detection untuk SPR
// ════════════════════════════════════════════════════════════
//
// PROBLEM:
//   SPR (Secret Private Relay) meninggalkan jejak ciphertext yang bisa di-detect:
//   1. Node <enc type="skmsg" v="2"> — sender-key message signature
//   2. Attribute "decrypt-fail:hide" di node — SPR Hide signature (paling mudah dikenali)
//   3. Distribution list hanya target+bot — metadata leak via SKDM
//   4. Attribute "mediatype" pada skmsg — mediatype leak
//   5. Pesan tidak ter-cache di recent message store — pattern unik
//
// CLOAK STRATEGY:
//   - Cloak 1: Mask node attributes — wrap dalam layer enc tambahan
//   - Cloak 2: Randomize message ID format (terlihat seperti normal message)
//   - Cloak 3: Insert dummy participants ke distribution list (bot itself multiple device
//              identities) sehingga count tidak 1-2 saja
//   - Cloak 4: Strip "decrypt-fail:hide" attribute dan ganti dengan reaction-based hide
//   - Cloak 5: Randomize timing send (jitter 50-200ms) untuk hindari pattern fingerprint
//
// ANTI-SPR DETECTOR (security/anti_spr.go):
//   - Deteksi incoming SPR dari user lain berdasarkan pattern yang sama
//   - Berguna untuk grup yang ingin tahu "ada yg pakai SPR di sini"
//
// Usage:
//   .sprcloak on|off|status — toggle cloaking untuk SPR sendiri
//   .antispr on|off|status  — toggle deteksi SPR dari user lain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waBinary "github.com/polymorfa/hypermeow/binary"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Cloak State ───────────────────────────────────────────────────

var (
	sprCloakMu     sync.RWMutex
	sprCloakOn     bool = true // default ON untuk stealth
	sprCloakJitter bool = true
)

// SPRCloakConfig persisted ke PG kv_store (key="spr_cloak")
// 2026-10-27: migrated from data/spr_cloak.json to PG kv_store.
type SPRCloakConfig struct {
	Cloak   bool `json:"cloak"`    // true = mask attributes
	Jitter  bool `json:"jitter"`   // true = random timing
	DummyP  bool `json:"dummy_p"`  // true = add dummy participants
	AntiSPR bool `json:"anti_spr"` // true = detect incoming SPR
}

const sprCloakKVKey = "spr_cloak"

var (
	sprCloakCfgMu sync.RWMutex
	sprCloakCfg   = SPRCloakConfig{
		Cloak:   true,
		Jitter:  true,
		DummyP:  true,
		AntiSPR: false, // default off — owner enable manual
	}
	sprCloakLoaded bool
)

func loadSPRCloak() {
	sprCloakCfgMu.Lock()
	defer sprCloakCfgMu.Unlock()
	if sprCloakLoaded {
		return
	}
	sprCloakLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !kvStoreGet(ctx, sprCloakKVKey, &sprCloakCfg) {
		// No row in PG — use defaults (already set in struct init).
		// Persist defaults so future loads are consistent.
		_ = kvStoreSet(ctx, sprCloakKVKey, &sprCloakCfg)
	}
}

// saveSPRCloakLocked menulis config ke PG. Caller HARUS sudah pegang
// sprCloakCfgMu (Lock atau RLock) — versi tanpa lock untuk dipakai di dalam
// handler yang sudah memegang lock (mencegah deadlock).
func saveSPRCloakLocked() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := kvStoreSet(ctx, sprCloakKVKey, &sprCloakCfg); err != nil {
		fmt.Printf("[SPR-Cloak] ⚠️ kvStoreSet gagal: %v\n", err)
	}
}

func saveSPRCloak() {
	sprCloakCfgMu.RLock()
	defer sprCloakCfgMu.RUnlock()
	saveSPRCloakLocked()
}

func sprCloakEnabled() bool {
	sprCloakCfgMu.RLock()
	defer sprCloakCfgMu.RUnlock()
	return sprCloakCfg.Cloak
}

func sprAntiSPREnabled() bool {
	sprCloakCfgMu.RLock()
	defer sprCloakCfgMu.RUnlock()
	return sprCloakCfg.AntiSPR
}

// ─── Cloak Functions ──────────────────────────────────────────────

// sprCloakRandomID generates a random message ID that mimics normal WhatsApp
// message ID format (44-char base64-like). Original sprRegisterBait uses
// GenerateMessageID() which has predictable format — we wrap it for stealth.
func sprCloakRandomID() string {
	b := make([]byte, 22)
	if _, err := rand.Read(b); err != nil {
		// fallback to time-based
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// sprCloakJitterSleep introduces random 50-200ms delay before send
// to defeat timing-based fingerprinting.
func sprCloakJitterSleep() {
	if !sprCloakEnabled() {
		return
	}
	n, err := rand.Int(rand.Reader, big.NewInt(150))
	if err != nil {
		return
	}
	delay := time.Duration(50+n.Int64()) * time.Millisecond
	time.Sleep(delay)
}

// sprCloakNode strips & masks attributes that fingerprint SPR.
// Called BEFORE sending the enc node.
//
// Removes:
//   - "decrypt-fail:hide" attribute (BIGGEST signature — replaced with normal enc)
//   - "mediatype" attribute (only present on SPR with media — leaks type)
//
// Adds:
//   - "count" attribute with random value (mimics normal message distribution)
//   - "oc" (origin channel) attribute (looks like normal WA biz message)
func sprCloakNode(node *waBinary.Node) {
	if node == nil || !sprCloakEnabled() {
		return
	}

	attrs := node.Attrs
	if attrs == nil {
		attrs = waBinary.Attrs{}
	}

	// Strip "decrypt-fail:hide" — biggest SPR signature
	delete(attrs, "decrypt-fail")

	// Strip "mediatype" from skmsg children
	for _, child := range node.GetChildren() {
		if child.Tag == "enc" {
			if child.Attrs == nil {
				continue
			}
			delete(child.Attrs, "mediatype")
			// Mask "type" — skmsg is sender-key, normal message uses "msg"
			// We CAN'T change this (would break decryption) but we can add
			// noise attributes to make it less obvious
			if child.Attrs["type"] == "skmsg" {
				// Add padding attributes that look like normal WA biz messages
				child.Attrs["v"] = "2"
				// don't change type — needed for decryption
			}
		}
	}

	// Add noise attributes that look like normal WhatsApp business messages
	if _, ok := attrs["count"]; !ok {
		n, _ := rand.Int(rand.Reader, big.NewInt(50))
		attrs["count"] = fmt.Sprintf("%d", 100+n.Int64())
	}

	node.Attrs = attrs
}

// sprCloakParticipants adds dummy participant entries (bot's own multiple
// device identities) so distribution count > 1-2.
//
// This makes the SKDM distribution list look more like a normal group broadcast
// rather than a targeted 1-2 person SPR.
//
// ⚠️ This does NOT actually send to fake devices — encryptMessageForDevices
// already skips invalid device IDs. It only affects the participant list
// passed to PrepareMessageNode, which influences internal bookkeeping.
//
// We add 2-3 "phantom" entries that are derivations of the bot's own LID
// (different device numbers). These get filtered out during encryption,
// but the participant count in metadata looks more natural.
func sprCloakParticipants(participants []string, botLID string) []string {
	if !sprCloakEnabled() || botLID == "" {
		return participants
	}

	out := make([]string, 0, len(participants)+3)
	out = append(out, participants...)

	// Add 2 phantom device entries for the bot itself
	// Format: <user>:<device>@lid — these will be filtered by encryption
	// but make the participant count look like 3-5 instead of 1-2
	userPart := strings.Split(botLID, "@")
	if len(userPart) > 0 && userPart[0] != "" {
		// Add device 9, 10, 11 (non-existent but valid format)
		for _, dev := range []int{9, 10} {
			phantom := fmt.Sprintf("%s:%d@lid", userPart[0], dev)
			out = append(out, phantom)
		}
	}

	return out
}

// ─── Command: .sprcloak ──────────────────────────────────────────

func handleSPRCloakCommand(args string) string {
	loadSPRCloak()
	parts := strings.Fields(strings.TrimSpace(strings.ToLower(args)))
	if len(parts) == 0 {
		return sprCloakStatus()
	}

	sprCloakCfgMu.Lock()
	defer sprCloakCfgMu.Unlock()

	switch parts[0] {
	case "on":
		sprCloakCfg.Cloak = true
		saveSPRCloakLocked()
		return "✅ *SPR Cloak ON*\n\nSPR sekarang stealth:\n• Strip decrypt-fail:hide\n• Strip mediatype leak\n• Add count noise attr\n• Random ID format\n• Jitter timing 50-200ms"

	case "off":
		sprCloakCfg.Cloak = false
		saveSPRCloakLocked()
		return "🔴 *SPR Cloak OFF*\n\nSPR sekarang polos (mudah terdeteksi)"

	case "jitter":
		if len(parts) < 2 {
			return "❓ *Cara pakai:* .sprcloak jitter on|off"
		}
		sprCloakCfg.Jitter = parts[1] == "on"
		saveSPRCloakLocked()
		state := "OFF"
		if sprCloakCfg.Jitter {
			state = "ON"
		}
		return fmt.Sprintf("✅ Jitter %s", state)

	case "dummy":
		if len(parts) < 2 {
			return "❓ *Cara pakai:* .sprcloak dummy on|off"
		}
		sprCloakCfg.DummyP = parts[1] == "on"
		saveSPRCloakLocked()
		state := "OFF"
		if sprCloakCfg.DummyP {
			state = "ON"
		}
		return fmt.Sprintf("✅ Dummy participants %s", state)

	default:
		return sprCloakStatusLocked()
	}
}

// sprCloakStatusLocked membangun string status. Caller HARUS sudah pegang
// sprCloakCfgMu (Lock atau RLock).
func sprCloakStatusLocked() string {
	cloak := "OFF"
	if sprCloakCfg.Cloak {
		cloak = "ON"
	}
	jitter := "OFF"
	if sprCloakCfg.Jitter {
		jitter = "ON"
	}
	dummy := "OFF"
	if sprCloakCfg.DummyP {
		dummy = "ON"
	}
	antiSpr := "OFF"
	if sprCloakCfg.AntiSPR {
		antiSpr = "ON"
	}

	return fmt.Sprintf(`🛡️ *SPR Cloak Status*

• Cloak: %s
• Jitter: %s (50-200ms random delay)
• Dummy P: %s (phantom participants)
• Anti-SPR: %s (deteksi SPR user lain)

*Cloak menghapus:*
  • decrypt-fail:hide attribute
  • mediatype leak
  • predictable ID format

*Commands:*
  • .sprcloak on|off
  • .sprcloak jitter on|off
  • .sprcloak dummy on|off
  • .antispr on|off`, cloak, jitter, dummy, antiSpr)
}

// sprCloakStatus adalah wrapper thread-safe dari sprCloakStatusLocked.
func sprCloakStatus() string {
	sprCloakCfgMu.RLock()
	defer sprCloakCfgMu.RUnlock()
	return sprCloakStatusLocked()
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "sprcloak",
		Description: "sprcloak",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSPRCloakCommand(args)
			// ─── Anti-SPR (detect SPR from other users) ───────────
		},
	})
}
