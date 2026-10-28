package main

// shortlink.go — URL Shortener via TinyURL (gratis, tanpa key)
//
// Port dari shortlink-manager.ts (Baileys) ke Go/whatsmeow.
//

// Commands:
//   .shorten <url>     → Shorten a URL
//   .shorten list       → List your shortened URLs
//
// Owner only.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── Types ────────────────────────────────────────────────────────

type ShortLink struct {
	Original  string `json:"original"`
	Short     string `json:"short"`
	Code      string `json:"code"`
	CreatedAt int64  `json:"createdAt"`
}

type ShortLinksDB struct {
	mu    sync.Mutex
	links map[string][]ShortLink // key = normalized user ID
}

var shortLinksDB = &ShortLinksDB{links: map[string][]ShortLink{}}

// ─── Helpers ─────────────────────────────────────────────────────

func normalizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// shortlinkViaTinyURL memendekkan URL via TinyURL (gratis, tanpa API key).
func shortlinkViaTinyURL(longURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET",
		shortlinkBaseURL()+"/api-create.php?url="+url.QueryEscape(longURL), nil)
	req.Header.Set("User-Agent", "hyper-go/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	short := strings.TrimSpace(string(body))
	if resp.StatusCode != 200 || !strings.HasPrefix(short, "http") {
		return "", fmt.Errorf("TinyURL gagal: %s", short)
	}
	return short, nil
}

// ─── Command handler ─────────────────────────────────────────────

func handleShortenCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	input := strings.TrimSpace(args)
	senderNorm := normalizeID(msg.Info.Sender.String())

	// .shorten list
	lower := strings.ToLower(input)
	if lower == "list" || lower == "ls" {
		shortLinksDB.mu.Lock()
		links := shortLinksDB.links[senderNorm]
		shortLinksDB.mu.Unlock()

		if len(links) == 0 {
			return "ℹ️ Kamu belum punya short link."
		}

		var b strings.Builder
		b.WriteString(fmt.Sprintf("🔗 *Short Links* (%d)\n\n", len(links)))
		max := 5
		if len(links) < max {
			max = len(links)
		}
		for i := 0; i < max; i++ {
			l := links[i]
			date := time.Unix(l.CreatedAt/1000, 0).Format("2006-01-02")
			b.WriteString(fmt.Sprintf("%d. `%s`\n   %s\n   _%s_\n\n", i+1, l.Code, l.Short, date))
		}
		if len(links) > 5 {
			b.WriteString(fmt.Sprintf("_+%d lagi_", len(links)-5))
		}
		return b.String()
	}

	// .shorten <url>
	if input == "" || !strings.HasPrefix(input, "http") {
		return `🔗 *Shorten URL*

• .shorten <url> — pendekkan URL
• .shorten list — lihat short link kamu

Contoh: .shorten https://example.com/very/long/url`
	}

	// Shorten via TinyURL langsung (tanpa kangwifi)
	shortURL, err := shortlinkViaTinyURL(input)
	if err != nil {
		return fmt.Sprintf("❌ Gagal: %v", err)
	}

	// Extract code
	code2 := shortURL
	if idx := strings.LastIndex(shortURL, "/"); idx >= 0 {
		code2 = shortURL[idx+1:]
	}
	if code2 == "" {
		code2 = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	// Save to DB
	shortLinksDB.mu.Lock()
	shortLinksDB.links[senderNorm] = append(shortLinksDB.links[senderNorm], ShortLink{
		Original:  input,
		Short:     shortURL,
		Code:      code2,
		CreatedAt: time.Now().UnixMilli(),
	})
	shortLinksDB.mu.Unlock()

	return fmt.Sprintf("🔗 *URL Shortened!*\n\n| Field | Value |\n|---|---|\n| Original | %s |\n| Short | %s |",
		truncateStr(input, 60), shortURL)
}
