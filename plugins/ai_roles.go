package main

// ai_roles.go — Role-based AI access system
// ════════════════════════════════════════════════════════════
//
// .ai command accessible like .s (sticker) — reply image + .ai → analyze
// Different user roles get different capabilities:
//
// ┌─────────────┬───────┬────────┬──────────┬───────┬──────┬───────────┐
// │ Role        │ Chat  │ Image  │ GrupCmds │ Shell │ Eval │ RateLimit │
// ├─────────────┼───────┼────────┼──────────┼───────┼──────┼───────────┤
// │ Owner       │   ✅  │  ✅    │   ✅     │  ✅   │  ✅  │ unlimited │
// │ Admin Grup  │   ✅  │  ✅    │   ✅     │  ❌   │  ❌  │ 20/min    │
// │ Trusted     │   ✅  │  ✅    │   ❌     │  ❌   │  ❌  │ 10/min    │
// │ User        │   ✅  │  ❌    │   ❌     │  ❌   │  ❌  │ 5/min     │
// │ Banned      │   ❌  │  ❌    │   ❌     │  ❌   │  ❌  │ 0         │
// └─────────────┴───────┴────────┴──────────┴───────┴──────┴───────────┘
//
// Role detection:
//   - Owner:     isOwnerMsg(msg) → OWNER_JID match
//   - Admin:     isGroupAdmin(client, msg) → WA group admin
//   - Trusted:   isTrustedMsg(msg) → .trust @tag list
//   - User:      default (everyone else)
//   - Banned:    .aiban @tag list (new)
//
// Image analysis:
//   Reply image + .ai <question> → AI analyze image
//   Owner/Admin/Trusted: Pollinations vision (free, no key)
//   User: text-only (image ignored, AI told "user replied with image")
//
// Commands:
//   .ai <text>              — chat AI (all roles)
//   .ai <text> (reply img)  — analyze image (owner/admin/trusted)
//   .ai kick @tag           — group tools (owner/admin)
//   .ai shell <cmd>         — shell (owner only)
//   .ai eval <code>         — Go eval (owner only)
//   .ai ban @tag            — ban user from AI (owner only)
//   .ai unban @tag          — unban (owner only)
//   .ai banlist             — list banned users
//   .ai role                — show your current role + capabilities
//   .ai help                — help

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Role definitions ─────────────────────────────────────────────

type AIRole int

const (
	AIRoleBanned AIRole = iota
	AIRoleUser
	AIRoleTrusted
	AIRoleAdmin
	AIRoleOwner
)

func (r AIRole) String() string {
	switch r {
	case AIRoleBanned:
		return "Banned"
	case AIRoleUser:
		return "User"
	case AIRoleTrusted:
		return "Trusted"
	case AIRoleAdmin:
		return "Admin Grup"
	case AIRoleOwner:
		return "Owner"
	default:
		return "Unknown"
	}
}

// AIRoleConfig defines what a role can do.
type AIRoleConfig struct {
	CanChat          bool
	CanAnalyzeImage  bool
	CanUseGroupTools bool // kick, promote, demote, tagall, hidetag, setname, setdesc
	CanUseShell      bool
	CanUseEval       bool
	CanUseReadFile   bool
	CanUseGetFile    bool
	CanUseGitBackup  bool
	CanBanUsers      bool // .ai ban/unban
	RateLimitPerMin  int  // 0 = unlimited
}

var aiRoleConfigs = map[AIRole]AIRoleConfig{
	AIRoleBanned: {
		// No access at all
		RateLimitPerMin: 0,
	},
	AIRoleUser: {
		CanChat:         true,
		CanAnalyzeImage: false, // text-only for regular users
		RateLimitPerMin: 5,
	},
	AIRoleTrusted: {
		CanChat:         true,
		CanAnalyzeImage: true,
		RateLimitPerMin: 10,
	},
	AIRoleAdmin: {
		CanChat:          true,
		CanAnalyzeImage:  true,
		CanUseGroupTools: true,
		RateLimitPerMin:  20,
	},
	AIRoleOwner: {
		CanChat:          true,
		CanAnalyzeImage:  true,
		CanUseGroupTools: true,
		CanUseShell:      true,
		CanUseEval:       true,
		CanUseReadFile:   true,
		CanUseGetFile:    true,
		CanUseGitBackup:  true,
		CanBanUsers:      true,
		RateLimitPerMin:  0, // unlimited
	},
}

// ─── Role detection ───────────────────────────────────────────────

// aiDetectRole determines the user's role based on message context.
// Priority: Banned > Owner > Admin > Trusted > User
func aiDetectRole(client *whatsmeow.Client, msg *events.Message) AIRole {
	senderJID := msg.Info.Sender.String()

	// Check banned first (even owner can't be banned, but check anyway)
	if aiIsBanned(senderJID) {
		return AIRoleBanned
	}

	// Owner (highest priority)
	if isOwnerMsg(msg) {
		return AIRoleOwner
	}

	// Admin grup (only in group context)
	if msg.Info.IsGroup && isGroupAdmin(client, msg) {
		return AIRoleAdmin
	}

	// Trusted user
	if isTrustedMsg(msg) {
		return AIRoleTrusted
	}

	// Default: regular user
	return AIRoleUser
}

// ─── Banned users registry ────────────────────────────────────────
//
// 2026-10-27: migrated from data/aiban.json to PG kv_store (key="aiban").
// The banned list is a simple map[string]bool — kv_store is a perfect fit.

var (
	aiBanMu     sync.RWMutex
	aiBanList   = map[string]bool{} // senderJID → banned
	aiBanLoaded bool
)

const aiBanKVKey = "aiban"

func loadAIBanList() {
	aiBanMu.Lock()
	defer aiBanMu.Unlock()
	if aiBanLoaded {
		return
	}
	aiBanLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// kvStoreGet unmarshals into the map directly.
	kvStoreGet(ctx, aiBanKVKey, &aiBanList)
	if aiBanList == nil {
		aiBanList = map[string]bool{}
	}
}

func saveAIBanList() {
	aiBanMu.RLock()
	// Copy under lock to release fast.
	snapshot := make(map[string]bool, len(aiBanList))
	for k, v := range aiBanList {
		snapshot[k] = v
	}
	aiBanMu.RUnlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = kvStoreSet(ctx, aiBanKVKey, &snapshot)
	}()
}

func aiIsBanned(senderJID string) bool {
	loadAIBanList()
	aiBanMu.RLock()
	defer aiBanMu.RUnlock()
	return aiBanList[senderJID]
}

func aiBanUser(senderJID string) {
	aiBanMu.Lock()
	aiBanList[senderJID] = true
	aiBanMu.Unlock()
	go saveAIBanList()
}

func aiUnbanUser(senderJID string) bool {
	aiBanMu.Lock()
	existed := aiBanList[senderJID]
	delete(aiBanList, senderJID)
	aiBanMu.Unlock()
	if existed {
		go saveAIBanList()
	}
	return existed
}

func aiBanListCount() int {
	aiBanMu.RLock()
	defer aiBanMu.RUnlock()
	return len(aiBanList)
}

// ─── Rate limiter per role ────────────────────────────────────────

var (
	aiRateMu      sync.Mutex
	aiRateBuckets = map[string][]int64{} // senderJID → timestamps
)

func aiRateAllow(senderJID string, role AIRole) bool {
	cfg := aiRoleConfigs[role]
	if cfg.RateLimitPerMin == 0 {
		return true // unlimited
	}

	aiRateMu.Lock()
	defer aiRateMu.Unlock()

	now := time.Now().UnixMilli()
	windowMs := int64(60 * 1000) // 1 minute

	// Filter fresh timestamps
	old := aiRateBuckets[senderJID]
	fresh := make([]int64, 0, cfg.RateLimitPerMin)
	for _, t := range old {
		if now-t < windowMs {
			fresh = append(fresh, t)
		}
	}

	if len(fresh) >= cfg.RateLimitPerMin {
		aiRateBuckets[senderJID] = fresh
		return false
	}

	fresh = append(fresh, now)
	aiRateBuckets[senderJID] = fresh
	return true
}

// ─── Image analysis via Pollinations Vision (free, no API key) ────

// aiAnalyzeImage sends an image + question to Pollinations vision API.
// Returns AI text response describing the image.
//
// Pollinations supports OpenAI-compatible API with vision models.
// Free tier, no API key required (anonymous access).
func aiAnalyzeImage(imageData []byte, mimeType, question string) (string, error) {
	if len(imageData) == 0 {
		return "", fmt.Errorf("empty image data")
	}

	// Convert image to base64 data URL
	b64 := base64.StdEncoding.EncodeToString(imageData)
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, b64)

	// Build OpenAI-compatible request
	reqBody := map[string]interface{}{
		"model": "openai",
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": question},
					{"type": "image_url", "image_url": map[string]string{"url": dataURL}},
				},
			},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST",
		pollinationsBaseURL()+"/openai", bytes.NewReader(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hyper-go/1.0")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vision request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("vision HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if result.Error != "" {
		return "", fmt.Errorf("API error: %s", result.Error)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no response from vision API")
	}

	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

// ─── Download image from replied message ──────────────────────────

// aiDownloadImage downloads image from quoted message using client.
// Returns (imageData, mimeType, error). Error if no image or download fails.
func aiDownloadImage(client *whatsmeow.Client, quoted *events.Message) ([]byte, string, error) {
	if quoted == nil || quoted.Message == nil {
		return nil, "", fmt.Errorf("no quoted message")
	}

	var imgMsg *waE2E.ImageMessage
	if img := quoted.Message.GetImageMessage(); img != nil {
		imgMsg = img
	}
	if imgMsg == nil {
		if doc := quoted.Message.GetDocumentWithCaptionMessage(); doc != nil {
			imgMsg = doc.GetMessage().GetImageMessage()
		}
	}
	if imgMsg == nil {
		return nil, "", fmt.Errorf("no image in quoted message")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	data, err := client.Download(ctx, imgMsg)
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}

	mime := imgMsg.GetMimetype()
	if mime == "" {
		mime = "image/jpeg"
	}

	return data, mime, nil
}

// ─── Role info display ────────────────────────────────────────────

func aiRoleInfo(role AIRole) string {
	cfg := aiRoleConfigs[role]
	var caps []string

	if cfg.CanChat {
		caps = append(caps, "💬 Chat")
	}
	if cfg.CanAnalyzeImage {
		caps = append(caps, "🖼️ Image Analysis")
	}
	if cfg.CanUseGroupTools {
		caps = append(caps, "👥 Group Tools (kick/promote/tagall)")
	}
	if cfg.CanUseShell {
		caps = append(caps, "🖥️ Shell")
	}
	if cfg.CanUseEval {
		caps = append(caps, "⚙️ Go Eval")
	}
	if cfg.CanUseReadFile {
		caps = append(caps, "📄 Read File")
	}
	if cfg.CanUseGetFile {
		caps = append(caps, "📁 Get File")
	}
	if cfg.CanUseGitBackup {
		caps = append(caps, "📦 Git Backup")
	}
	if cfg.CanBanUsers {
		caps = append(caps, "🚫 Ban Users")
	}

	rateStr := "unlimited"
	if cfg.RateLimitPerMin > 0 {
		rateStr = fmt.Sprintf("%d/min", cfg.RateLimitPerMin)
	}

	capsStr := "(none)"
	if len(caps) > 0 {
		capsStr = strings.Join(caps, "\n  ")
	}

	return fmt.Sprintf(`🎭 *Role: %s*

Capabilities:
  %s

Rate limit: %s`, role.String(), capsStr, rateStr)
}
