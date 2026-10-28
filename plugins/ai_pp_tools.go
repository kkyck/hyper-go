package main

// ai_pp_tools.go — Profile Picture tools + extra download tools for AI agent
// ════════════════════════════════════════════════════════════
//
// v2.84 — Adds profile-picture fetching tools + download-tool wrappers
// so the AI can autonomously fetch PP and run downloads.
//
// TOOL CATEGORIES:
//   1. PROFILE PICTURE TOOLS (PP)
//     - get_pp_wa:        WhatsApp profile picture (per-user or per-group)
//     - get_pp_github:    GitHub avatar (by username)
//     - get_pp_instagram: Instagram profile picture (by username, public only)
//     - get_pp_tiktok:    TikTok profile picture (by username, public only)
//     - get_pp_email:     Gravatar avatar (by email hash)
//     - get_pp_discord:   Discord user avatar (by user ID — requires bot token, optional)
//
//   2. EXTRA DOWNLOAD TOOLS (wrappers around existing handlers)
//     - download_youtube2: .yt2 dual-engine extractor (Innertube + Web SSR)
//     - download_youtube_video: alias for download_youtube2 with video subcommand
//     - download_youtube_audio: alias for download_youtube2 with audio subcommand
//     - download_facebook:  Facebook video downloader
//     - download_threads:    Threads post downloader
//     - download_spotify:    Spotify track downloader
//     - download_soundcloud: SoundCloud track downloader
//     - download_yt2_thumb:  YouTube thumbnail (maxresdefault)
//
// All PP tools return a direct image URL (or download + send to chat).
// Behavior: AI calls tool → tool fetches PP URL → sends image to chat → returns status text.

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

// ─── HTTP client (shared for PP fetches) ───────────────────────────

var aiPPHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
}

// ─── 1. WHATSAPP PROFILE PICTURE ──────────────────────────────────

// aiGetPPWA fetches the WhatsApp profile picture of a target user/group
// and sends it to the current chat.
// Args: empty = sender's own PP, "group" = current group PP,
//
//	phone/@tag/username = target user's PP
func aiGetPPWA(client *whatsmeow.Client, msg *events.Message, args string) string {
	if client == nil {
		return "Error: client nil"
	}

	var targetJID types.JID
	var label string

	args = strings.TrimSpace(args)

	if args == "" || args == "self" {
		// Sender's own PP
		targetJID = msg.Info.Sender
		label = "sender"
	} else if args == "group" || args == "chat" {
		// Current group/chat PP
		targetJID = msg.Info.Chat
		label = "current chat"
	} else {
		// Resolve target (phone or @tag)
		j, lbl, err := aiResolveTarget(client, msg, args)
		if err != nil {
			return "Error resolving target: " + err.Error()
		}
		targetJID = j
		label = lbl
	}

	// Try PN form first (more reliable for PP fetch)
	effectiveJID := targetJID
	if ph := phoneOf(client, targetJID); ph != "" {
		effectiveJID = types.NewJID(ph, types.DefaultUserServer)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ppInfo, err := client.GetProfilePictureInfo(ctx, effectiveJID, &whatsmeow.GetProfilePictureParams{})
	if err != nil || ppInfo == nil || ppInfo.URL == "" {
		// Fallback to original JID form
		if effectiveJID != targetJID {
			ppInfo, err = client.GetProfilePictureInfo(ctx, targetJID, &whatsmeow.GetProfilePictureParams{})
		}
		if err != nil || ppInfo == nil || ppInfo.URL == "" {
			return fmt.Sprintf("PP %s tidak tersedia (privacy hidden atau belum set): %v", label, err)
		}
	}

	// Send the image to chat
	caption := fmt.Sprintf("🖼️ PP WhatsApp: %s", label)
	if err := sendImageFromURL(client, msg.Info.Chat, ppInfo.URL, caption); err != nil {
		// Fallback: just return the URL
		return fmt.Sprintf("PP URL (gagal kirim gambar): %s", ppInfo.URL)
	}
	return fmt.Sprintf("✅ PP %s sudah dikirim (URL: %s)", label, ppInfo.URL)
}

// ─── 2. GITHUB AVATAR ─────────────────────────────────────────────

// aiGetPPGitHub fetches a GitHub user's avatar and sends it to chat.
// Args: GitHub username (e.g. "torvalds")
func aiGetPPGitHub(client *whatsmeow.Client, msg *events.Message, args string) string {
	username := strings.TrimSpace(args)
	if username == "" {
		return "Error: username GitHub kosong. Contoh: get_pp_github torvalds"
	}
	// Strip leading @ if present
	username = strings.TrimPrefix(username, "@")
	// GitHub avatar URL pattern: https://avatars.githubusercontent.com/u/<ID>
	// But we can use the simpler: https://github.com/<username>.png?size=512
	avatarURL := fmt.Sprintf("https://github.com/%s.png?size=512", username)

	caption := fmt.Sprintf("🖼️ GitHub avatar: @%s", username)
	if err := sendImageFromURL(client, msg.Info.Chat, avatarURL, caption); err != nil {
		return fmt.Sprintf("GitHub avatar URL: %s (gagal kirim: %v)", avatarURL, err)
	}
	return fmt.Sprintf("✅ GitHub avatar @%s dikirim", username)
}

// ─── 3. INSTAGRAM PROFILE PICTURE ──────────────────────────────────

// aiGetPPInstagram fetches an Instagram user's profile picture (public only).
// Uses the public Instagram page + regex extraction.
// Args: Instagram username (without @)
func aiGetPPInstagram(client *whatsmeow.Client, msg *events.Message, args string) string {
	username := strings.TrimSpace(args)
	if username == "" {
		return "Error: username Instagram kosong. Contoh: get_pp_instagram natgeo"
	}
	username = strings.TrimPrefix(username, "@")

	// Try multiple strategies for IG PP URL:
	// 1. Direct: https://www.instagram.com/<username>/?__a=1 (often blocked)
	// 2. HTML scrape: fetch profile page, extract og:image meta tag
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pageURL := fmt.Sprintf("https://www.instagram.com/%s/", username)
	req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := aiPPHTTPClient.Do(req)
	if err != nil {
		return fmt.Sprintf("Error fetch IG page: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Sprintf("IG HTTP %d untuk @%s", resp.StatusCode, username)
	}

	// Read up to 1MB of HTML
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return fmt.Sprintf("Error read IG: %v", err)
	}

	// Extract og:image meta tag (most reliable for IG PP)
	ogImageRegex := regexp.MustCompile(`<meta\s+property="og:image"\s+content="([^"]+)"`)
	match := ogImageRegex.FindSubmatch(body)
	if len(match) < 2 {
		// Fallback: try to find profile_pic_url in embedded JSON
		profPicRegex := regexp.MustCompile(`"profile_pic_url":\s*"([^"]+)"`)
		match = profPicRegex.FindSubmatch(body)
		if len(match) < 2 {
			return fmt.Sprintf("PP @%s tidak ditemukan (akun private atau tidak ada)", username)
		}
	}

	ppURL := string(match[1])
	// Unescape HTML entities
	ppURL = strings.ReplaceAll(ppURL, "&amp;", "&")
	ppURL = strings.ReplaceAll(ppURL, "\\/", "/")

	caption := fmt.Sprintf("🖼️ Instagram PP: @%s", username)
	if err := sendImageFromURL(client, msg.Info.Chat, ppURL, caption); err != nil {
		return fmt.Sprintf("IG PP URL: %s (gagal kirim: %v)", ppURL, err)
	}
	return fmt.Sprintf("✅ IG PP @%s dikirim", username)
}

// ─── 4. TIKTOK PROFILE PICTURE ────────────────────────────────────

// aiGetPPTikTok fetches a TikTok user's profile picture.
// Uses the public TikTok page + og:image meta tag.
// Args: TikTok username (without @)
func aiGetPPTikTok(client *whatsmeow.Client, msg *events.Message, args string) string {
	username := strings.TrimSpace(args)
	if username == "" {
		return "Error: username TikTok kosong. Contoh: get_pp_tiktok charlidamelio"
	}
	username = strings.TrimPrefix(username, "@")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pageURL := fmt.Sprintf("https://www.tiktok.com/@%s", username)
	req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := aiPPHTTPClient.Do(req)
	if err != nil {
		return fmt.Sprintf("Error fetch TikTok page: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Sprintf("TikTok HTTP %d untuk @%s", resp.StatusCode, username)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return fmt.Sprintf("Error read TikTok: %v", err)
	}

	// TikTok uses og:image meta tag for PP
	ogImageRegex := regexp.MustCompile(`<meta\s+(?:property|name)="og:image"\s+content="([^"]+)"`)
	match := ogImageRegex.FindSubmatch(body)
	if len(match) < 2 {
		// Fallback: try twitter:image
		twRegex := regexp.MustCompile(`<meta\s+name="twitter:image"\s+content="([^"]+)"`)
		match = twRegex.FindSubmatch(body)
		if len(match) < 2 {
			return fmt.Sprintf("PP TikTok @%s tidak ditemukan", username)
		}
	}

	ppURL := string(match[1])
	ppURL = strings.ReplaceAll(ppURL, "&amp;", "&")
	ppURL = strings.ReplaceAll(ppURL, "\\/", "/")

	caption := fmt.Sprintf("🖼️ TikTok PP: @%s", username)
	if err := sendImageFromURL(client, msg.Info.Chat, ppURL, caption); err != nil {
		return fmt.Sprintf("TikTok PP URL: %s (gagal kirim: %v)", ppURL, err)
	}
	return fmt.Sprintf("✅ TikTok PP @%s dikirim", username)
}

// ─── 5. GRAVATAR (EMAIL AVATAR) ───────────────────────────────────

// aiGetPPEmail fetches a Gravatar avatar by email hash.
// Args: email address
func aiGetPPEmail(client *whatsmeow.Client, msg *events.Message, args string) string {
	email := strings.TrimSpace(args)
	if email == "" {
		return "Error: email kosong. Contoh: get_pp_email user@example.com"
	}
	email = strings.ToLower(email)

	// MD5 hash the email (Gravatar requirement)
	hasher := md5.Sum([]byte(email))
	emailHash := hex.EncodeToString(hasher[:])

	// Gravatar URL: https://www.gravatar.com/avatar/<hash>?s=512
	avatarURL := fmt.Sprintf("https://www.gravatar.com/avatar/%s?s=512&d=404", emailHash)

	// Verify it exists (d=404 returns 404 if no avatar)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "HEAD", avatarURL, nil)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	resp, err := aiPPHTTPClient.Do(req)
	if err != nil {
		return fmt.Sprintf("Gravatar untuk %s tidak ditemukan (email belum terdaftar di Gravatar)", email)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Sprintf("Gravatar untuk %s tidak ditemukan (email belum terdaftar di Gravatar)", email)
	}

	caption := fmt.Sprintf("🖼️ Gravatar: %s", email)
	if err := sendImageFromURL(client, msg.Info.Chat, avatarURL, caption); err != nil {
		return fmt.Sprintf("Gravatar URL: %s (gagal kirim: %v)", avatarURL, err)
	}
	return fmt.Sprintf("✅ Gravatar %s dikirim", email)
}

// ─── 6. DISCORD AVATAR (optional, needs bot token) ───────────────

// aiGetPPDiscord fetches a Discord user's avatar via the public API.
// Args: Discord user ID (numeric, e.g. "123456789012345678")
func aiGetPPDiscord(client *whatsmeow.Client, msg *events.Message, args string) string {
	userID := strings.TrimSpace(args)
	if userID == "" {
		return "Error: Discord user ID kosong. Contoh: get_pp_discord 123456789012345678"
	}
	// Strip @ or <@ prefix
	userID = strings.TrimPrefix(userID, "@")
	userID = strings.TrimPrefix(userID, "<@")
	userID = strings.TrimSuffix(userID, ">")

	// Validate numeric
	for _, c := range userID {
		if c < '0' || c > '9' {
			return "Error: Discord user ID harus angka"
		}
	}

	// Discord public API: https://discord.com/api/v9/users/<ID>
	// Note: without bot token, only some fields are returned.
	// For a fully public avatar fetch, we use the CDN directly.
	// Default avatar URL: https://cdn.discordapp.com/embed/avatars/<discriminator%5>.png
	// For custom avatar, we need the user's avatar hash from API (requires bot token).
	//
	// Workaround: try direct CDN URL with default avatar (works for users without custom avatar).
	// For custom avatar, attempt fetch with no auth (may fail for private users).

	// Try the public Discord API (returns avatar hash + discriminator)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apiURL := fmt.Sprintf("https://discord.com/api/v9/users/%s", userID)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	req.Header.Set("User-Agent", "DiscordBot (https://example.com, 1.0)")
	resp, err := aiPPHTTPClient.Do(req)
	if err != nil {
		return fmt.Sprintf("Error fetch Discord API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Sprintf("Discord API HTTP %d untuk user ID %s", resp.StatusCode, userID)
	}

	var userInfo struct {
		Username      string `json:"username"`
		Avatar        string `json:"avatar"`
		Discriminator string `json:"discriminator"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return fmt.Sprintf("Error parse Discord: %v", err)
	}

	if userInfo.Avatar == "" {
		// Default avatar based on discriminator
		var discrim int
		fmt.Sscanf(userInfo.Discriminator, "%d", &discrim)
		avatarURL := fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", discrim%5)
		caption := fmt.Sprintf("🖼️ Discord default avatar: %s#%s", userInfo.Username, userInfo.Discriminator)
		if err := sendImageFromURL(client, msg.Info.Chat, avatarURL, caption); err != nil {
			return fmt.Sprintf("URL: %s (gagal kirim: %v)", avatarURL, err)
		}
		return fmt.Sprintf("✅ Discord default avatar %s#%s dikirim", userInfo.Username, userInfo.Discriminator)
	}

	// Custom avatar
	ext := ".png"
	if strings.HasPrefix(userInfo.Avatar, "a_") {
		ext = ".gif"
	}
	avatarURL := fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s%s?size=512", userID, userInfo.Avatar, ext)
	caption := fmt.Sprintf("🖼️ Discord avatar: %s#%s", userInfo.Username, userInfo.Discriminator)
	if err := sendImageFromURL(client, msg.Info.Chat, avatarURL, caption); err != nil {
		return fmt.Sprintf("URL: %s (gagal kirim: %v)", avatarURL, err)
	}
	return fmt.Sprintf("✅ Discord avatar %s#%s dikirim", userInfo.Username, userInfo.Discriminator)
}

// ─── EXTRA DOWNLOAD TOOLS (wrappers) ─────────────────────────────

// aiDownloadYT2 — wrapper for handleYT2Command (dual-engine extractor)
// Args: "audio <url>" | "video <url>" | "thumb <url>" | "info <url>" | "<url>"
func aiDownloadYT2(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL kosong. Contoh: download_yt2 audio https://youtu.be/XXX"
	}
	return handleYT2Command(client, msg, args)
}

// aiDownloadYT2Audio — convenience alias
// Args: YouTube URL or video ID
func aiDownloadYT2Audio(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL kosong"
	}
	return handleYT2Command(client, msg, "audio "+args)
}

// aiDownloadYT2Video — convenience alias
// Args: YouTube URL or video ID
func aiDownloadYT2Video(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL kosong"
	}
	return handleYT2Command(client, msg, "video "+args)
}

// aiDownloadYT2Thumb — fetch YouTube thumbnail
// Args: YouTube URL or video ID
func aiDownloadYT2Thumb(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL kosong"
	}
	return handleYT2Command(client, msg, "thumb "+args)
}

// aiDownloadFacebook — Facebook video downloader wrapper
func aiDownloadFacebook(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL Facebook kosong"
	}
	return handleFacebookDL(client, msg, args)
}

// aiDownloadThreads — Threads post downloader wrapper
func aiDownloadThreads(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL Threads kosong"
	}
	return handleThreadsDL(client, msg, args)
}

// aiDownloadSpotify — Spotify track downloader wrapper
func aiDownloadSpotify(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL Spotify kosong"
	}
	return handleSpotifyDL(client, msg, args)
}

// aiDownloadSoundCloud — SoundCloud track downloader wrapper
func aiDownloadSoundCloud(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: URL SoundCloud kosong"
	}
	return handleSoundCloudDL(client, msg, args)
}
