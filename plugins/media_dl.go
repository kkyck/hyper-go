package main

// ─── Media Downloader (github.com/Yz776/media-downloader) ─────────────
// Engine yt-dlp: TikTok/YouTube/Spotify/Instagram/1000+ situs.
// Fitur: video kualitas tertinggi, audio MP3 + lirik sinkron + cover,
// lirik saja, info metadata, MP4 "Spotify Now Playing" 7 tema.
// Script di-vendor di tools/media_downloader.py (private repo, diambil via API).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

const mdScriptPath = "tools/media_downloader.py"
const npYTMusicScriptPath = "tools/np_ytmusic.py"

// mdSem membatasi download paralel agar VM tidak kewalahan.
var mdSem = make(chan struct{}, 2)

var mdDepsMu sync.Mutex
var mdDepsOK bool
var mdPendingReg = NewRegistry[string, types.JID]() // jobID -> chat

// mdRevokeMsg menghapus sebuah pesan milik bot (untuk .mdhapus).
func mdRevokeMsg(client *whatsmeow.Client, chat types.JID, msgID string) bool {
	if msgID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := client.RevokeMessage(ctx, chat, types.MessageID(msgID))
	return err == nil
}

// ensureMediaDlDeps memastikan yt-dlp/curl_cffi/pillow terinstall DAN
// CA egress proxy terdaftar di certifi (untuk CurlCFFI fallback).
// Dibuat self-healing karena /usr tidak persisten saat VM diganti.
// Berbeda dari sync.Once: bila gagal, dicoba lagi di pemanggilan berikut.
func ensureMediaDlDeps() {
	// Pure Go: tidak ada dependency Python lagi
	mdDepsMu.Lock()
	defer mdDepsMu.Unlock()
	mdDepsOK = true
}

// mdSendText mengirim teks balasan dari goroutine async.
func mdSendText(client *whatsmeow.Client, chat types.JID, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = client.SendMessage(ctx, chat, &waE2E.Message{
		Conversation: proto.String(text),
	})
}

// mdPickFile mencari file hasil di outdir berdasarkan ekstensi prioritas.
func mdPickFile(outdir string, exts []string) string {
	entries, err := os.ReadDir(outdir)
	if err != nil {
		return ""
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		for _, ext := range exts {
			if strings.HasSuffix(lower, ext) {
				fi, _ := e.Info()
				cands = append(cands, cand{filepath.Join(outdir, e.Name()), fi.ModTime()})
				break
			}
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	return cands[0].path
}

func mdAsync(client *whatsmeow.Client, msg *events.Message, fn func(outdir string) string) string {
	select {
	case mdSem <- struct{}{}:
	default:
		return "⏳ Downloader sibuk (2 slot penuh), coba lagi sebentar."
	}
	chat := msg.Info.Chat
	jobID := fmt.Sprintf("mdjob-%d", time.Now().UnixMilli())
	mdPendingReg.Set(jobID, chat)

	go func() {
		defer func() { <-mdSem }()
		defer mdPendingReg.Delete(jobID)

		outdir, err := os.MkdirTemp("/tmp", "md_dl_*")
		if err != nil {
			mdSendText(client, chat, "❌ Gagal buat temp dir")
			return
		}
		defer os.RemoveAll(outdir)
		result := fn(outdir)
		if result != "" {
			mdSendText(client, chat, result)
		}
	}()
	return ""
}

// ─── .mdhapus — hapus notif ⏳ downloader yang gantung ────────────────
func handleMDHapus(client *whatsmeow.Client, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	revoked := 0

	// 1. yang masih terlacak di memori (snapshot = copy, aman dibaca)
	snap := mdPendingReg.Snapshot()
	pending := make([]struct {
		id   string
		chat types.JID
	}, 0, len(snap))
	for id, chat := range snap {
		pending = append(pending, struct {
			id   string
			chat types.JID
		}{id, chat})
	}
	for _, p := range pending {
		if mdRevokeMsg(client, p.chat, p.id) {
			revoked++
		}
	}

	// 2. sikat sisa di crmdb: blob berisi ⏳ + frasa khas notif bot (24 jam terakhir).
	// Tidak filter sender karena sender pesan keluar tercatat tidak konsisten.
	// NOTE: SQLite LIKE tidak cocok untuk byte UTF-8 di kolom BLOB — filter di Go.
	mdHourglassPhrases := [][]byte{
		[]byte("⏳ Download"), []byte("⏳ Render"), []byte("⏳ Ambil"),
		[]byte("⏳ Downloader"),
	}
	{
		cutoff := time.Now().Add(-24 * time.Hour).Unix()
		db, err := pgGet()
		if err == nil {
			mdCtx, mdCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer mdCancel()
			rows, err := pgQuery(mdCtx, db,
				"SELECT id, chat, data FROM messages WHERE timestamp > ? ORDER BY rowid DESC LIMIT 500",
				cutoff)
			if err == nil {
				for rows.Next() {
					var id, chatStr string
					var data []byte
					if rows.Scan(&id, &chatStr, &data) != nil {
						continue
					}
					matched := false
					for _, ph := range mdHourglassPhrases {
						if bytes.Contains(data, ph) {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
					chatJID, err := types.ParseJID(chatStr)
					if err != nil {
						continue
					}
					rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
					_, rerr := client.RevokeMessage(rctx, chatJID, types.MessageID(id))
					rcancel()
					if rerr == nil {
						revoked++
					}
				}
				rows.Close()
			}
			// NOTE: db is the shared pool — never close it here.
		}
	}

	if revoked == 0 {
		return "Tidak ada notif ⏳ gantung."
	}
	return fmt.Sprintf("🧹 %d notif ⏳ dihapus.", revoked)
}

// ─── .md — video ──────────────────────────────────────────────────────
func handleMDVideo(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return "❓ *Cara pakai:* .md <url/judul>\nContoh: .md https://www.tiktok.com/@x/video/123"
	}
	query := args
	if query == "" {
		return "❌ URL/judul kosong"
	}
	return mdAsync(client, msg, func(outdir string) string {
		// Pure Go: TikTok via tiktokExtractGo, YouTube via yt2 Innertube
		urlStr := extractFirstURL(query)
		var data []byte
		var caption string
		var err error

		if urlStr != "" && strings.Contains(urlStr, "tiktok.com") {
			var info *tiktokInfo
			info, err = tiktokExtractGo(urlStr)
			if err == nil {
				caption = "🎵 *TikTok*"
				if info.Title != "" {
					caption = "🎵 *" + info.Title + "*"
				}
				if info.Author != "" {
					caption += "\n👤 " + info.Author
				}
				data, err = tiktokDownloadGo(info.VideoURL, 50*1024*1024)
			}
		} else {
			// YouTube: search atau langsung
			videoURL := urlStr
			if videoURL == "" {
				videoURL, _, _, err = ytmp3SearchYouTube(query)
			}
			if err == nil {
				videoID := extractYouTubeID(videoURL)
				var playerResp *yt2InnertubePlayerResponse
				playerResp, _, err = yt2RaceEngines(videoID)
				if err == nil {
					caption = "🎬 *" + playerResp.VideoDetails.Title + "*"
					// Cari video stream terbaik
					var videoURL string
					for _, f := range playerResp.StreamingData.Formats {
						if strings.HasPrefix(f.MimeType, "video/mp4") && f.URL != "" {
							videoURL = f.URL
							break
						}
					}
					if videoURL == "" {
						err = fmt.Errorf("video stream tidak ditemukan")
					} else {
						data, err = tiktokDownloadGo(videoURL, 100*1024*1024)
					}
				}
			}
		}
		if err != nil {
			return "❌ Download gagal: " + err.Error()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := sprSendVideo(client, ctx, msg.Info.Chat, data, "video/mp4", caption); err != nil {
			return "❌ Kirim gagal: " + err.Error()
		}
		return ""
	})
}

// ─── .mda — audio MP3 + lirik + cover ─────────────────────────────────
func handleMDAudio(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return "❓ *Cara pakai:* .mda <url/judul lagu>\nContoh: .mda golden hour jvke"
	}
	return mdAsync(client, msg, func(outdir string) string {
		// Pure Go: search YouTube + download audio via Innertube
		query := args
		urlStr := extractFirstURL(args)
		if urlStr == "" {
			// Search dulu
			videoURL, _, _, err := ytmp3SearchYouTube(query)
			if err != nil {
				return "❌ Search gagal: " + err.Error()
			}
			urlStr = videoURL
		}
		buf, title, _, err := ytmp3DownloadAudio(urlStr)
		if err != nil {
			return "❌ Download gagal: " + err.Error()
		}
		// Kirim sebagai audio
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := sprSendAudio(client, ctx, msg.Info.Chat, buf, "audio/mpeg", title+".mp3"); err != nil {
			return "❌ Kirim gagal: " + err.Error()
		}
		return ""
	})
}

// ─── .mdlirik — lirik saja ────────────────────────────────────────────
func handleMDLyrics(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return "❓ *Cara pakai:* .mdlirik <url/judul lagu>"
	}
	return mdAsync(client, msg, func(outdir string) string {
		// Pure Go: LRCLIB API langsung (tanpa Python)
		txt, err := lrclibSearchGo(args)
		if err != nil {
			return "❌ Gagal: " + err.Error()
		}
		if len(txt) > 3500 {
			txt = txt[:3500] + "\n..."
		}
		mdSendText(client, msg.Info.Chat, "📝 *Lirik*\n\n"+txt)
		return ""
	})
}

// ─── .mdinfo — metadata + tabel format ────────────────────────────────
func handleMDInfo(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	args = strings.TrimSpace(args)
	if args == "" {
		return "❓ *Cara pakai:* .mdinfo <url>"
	}
	return mdAsync(client, msg, func(outdir string) string {
		// Pure Go: info via Innertube
		urlStr := extractFirstURL(args)
		if urlStr == "" {
			return "❌ Butuh URL YouTube"
		}
		videoID := extractYouTubeID(urlStr)
		playerResp, _, err := yt2RaceEngines(videoID)
		if err != nil {
			return "❌ Gagal: " + err.Error()
		}
		txt := "📹 *" + playerResp.VideoDetails.Title + "*\n\n"
		txt += "• Channel: " + playerResp.VideoDetails.Author + "\n"
		txt += "• Views: " + playerResp.VideoDetails.ViewCount + "\n"
		txt += "• Link: https://youtu.be/" + videoID
		mdSendText(client, msg.Info.Chat, txt)
		return ""
	})
}

// ─── .np — Spotify Now Playing MP4 ──────────────────────────────────────
var mdThemes = []string{"aurora"}

func handleMDUI(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	query := strings.TrimSpace(args)
	if query == "" {
		return "❓ *Cara pakai:* .np <url/judul>"
	}
	// Pure Go: search YouTube + download audio + kirim
	// (Video render dengan lirik butuh Python, diganti versi audio sederhana)
	return mdAsync(client, msg, func(outdir string) string {
		urlStr := extractFirstURL(query)
		if urlStr == "" {
			videoURL, _, _, err := ytmp3SearchYouTube(query)
			if err != nil {
				return "❌ Search gagal: " + err.Error()
			}
			urlStr = videoURL
		}
		buf, title, _, err := ytmp3DownloadAudio(urlStr)
		if err != nil {
			return "❌ Download gagal: " + err.Error()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		caption := "♪ Now Playing ♪ • " + title
		if err := sprSendAudio(client, ctx, msg.Info.Chat, buf, "audio/mpeg", caption); err != nil {
			return "❌ Kirim gagal: " + err.Error()
		}
		return ""
	})
}

// ─── .mdtema — daftar tema ────────────────────────────────────────────
func handleMDThemes() string {
	return "🎨 *Tema Spotify Now Playing*\n\n" +
		"• aurora — PREMIUM, aurora bergerak + lirik glow\n" +
		"• classic — Spotify mobile gelap (default)\n" +
		"• crimson — hitam + aksen merah\n" +
		"• sunset — gradien senja\n" +
		"• ocean — gradien laut\n" +
		"• neon — malam + lirik cyan\n" +
		"• mono — minimalis putih-hitam\n" +
		"• glass — frosted glass terang\n\n" +
		"Cara pakai: .np <url/judul> [tema]"
}

// handleYTCookie — .ytcookie (reply ke file cookies.txt)
// Simpan cookies YouTube dari browser user (via extension "Get cookies.txt").
// Dengan cookies akun asli, yt-dlp lolos bot-check/403 YouTube.
// Cara: buka youtube.com (login) di Chrome -> Get cookies.txt -> Export ->
// kirim file ke bot dengan reply .ytcookie
func handleYTCookie(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if strings.TrimSpace(args) == "clear" {
		os.Remove("data/yt_cookies.txt")
		return "🗑️ Cookies YouTube dihapus."
	}
	if _, err := os.Stat("data/yt_cookies.txt"); err == nil && strings.TrimSpace(args) == "" {
		return "🍪 *Cookies YouTube*\n\n• Status: tersimpan\n• Hapus: .ytcookie clear"
	}
	quoted := getQuotedMessage(msg)
	if quoted == nil {
		return "❓ *Cara pakai:* reply file cookies.txt dengan .ytcookie\n\nCara dapat file:\n• Chrome/Edge: install extension \"Get cookies.txt LOCALLY\"\n• Buka youtube.com (pastikan login)\n• Klik extension → Export → download cookies.txt\n• Kirim file ke sini, reply dengan .ytcookie"
	}
	doc := quoted.Message.GetDocumentMessage()
	if doc == nil {
		return "❌ Reply ke file dokumen (cookies.txt), bukan pesan teks."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := client.Download(ctx, doc)
	if err != nil {
		return fmt.Sprintf("❌ Gagal download file: %v", err)
	}
	text := string(data)
	// Validasi: harus format Netscape + ada cookie youtube
	if !strings.Contains(text, "youtube.com") && !strings.Contains(text, ".youtube.com") {
		return "❌ File bukan cookies YouTube (tidak ada cookie youtube.com). Export dari youtube.com yang sudah login."
	}
	if !strings.Contains(text, "# Netscape HTTP Cookie File") && !strings.Contains(text, ".youtube.com") {
		return "❌ Format file tidak dikenal. Pakai extension \"Get cookies.txt\"."
	}
	os.MkdirAll("data", 0o755)
	if err := os.WriteFile("data/yt_cookies.txt", data, 0o600); err != nil {
		return fmt.Sprintf("❌ Gagal simpan: %v", err)
	}
	lines := strings.Count(text, "\n")
	return fmt.Sprintf("✅ Cookies YouTube tersimpan (%d baris). .md sekarang pakai akun kamu — bot-check YouTube jauh berkurang.", lines)
}

// handleSpotifyToken — .spotifytoken <access_token> (owner only)
// Simpan token Spotify manual (karena egress proxy memblokir accounts.spotify.com).
// Token didapat dari developer.spotify.com/console atau curl dari jaringan lain.
// Berlaku ~1 jam; ulangi bila search gagal.
func handleSpotifyToken(msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	tok := strings.TrimSpace(args)
	if tok == "" {
		// cek status
		if _, err := os.Stat("data/spotify_token.txt"); err == nil {
			return "🎵 *Token Spotify manual:* tersimpan\n\n• Ganti: .spotifytoken <token-baru>\n• Hapus: .spotifytoken clear"
		}
		return "❓ *Cara pakai:* .spotifytoken <access_token>\nPaste token dari developer.spotify.com/console"
	}
	if strings.ToLower(tok) == "clear" {
		os.Remove("data/spotify_token.txt")
		return "🗑️ Token Spotify manual dihapus."
	}
	// validasi ringan: JWT Spotify biasanya panjang
	if len(tok) < 50 {
		return "❌ Token terlalu pendek, pastikan copy penuh."
	}
	os.MkdirAll("data", 0o755)
	if err := os.WriteFile("data/spotify_token.txt", []byte(tok), 0o600); err != nil {
		return fmt.Sprintf("❌ Gagal simpan: %v", err)
	}
	return "✅ Token Spotify tersimpan. `.md <judul>` sekarang cari via katalog Spotify dulu."
}

// mdShortErr mengambil baris error terakhir yang relevan dari output script.
var mdAnsiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func mdShortErr(out string, err error) string {
	out = mdAnsiRe.ReplaceAllString(out, "")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// ambil hingga 3 baris terakhir yang tidak kosong
	var tail []string
	for i := len(lines) - 1; i >= 0 && len(tail) < 3; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			tail = append([]string{s}, tail...)
		}
	}
	s := strings.Join(tail, " / ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" && err != nil {
		return mdAnsiRe.ReplaceAllString(err.Error(), "")
	}
	return s
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "md",
		Aliases:     []string{"mdvideo"},
		Description: "md",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMDVideo(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "mda",
		Aliases:     []string{"mdmp3", "mdaudio"},
		Description: "mda",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMDAudio(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "mdlirik",
		Aliases:     []string{"mdlyrics"},
		Description: "mdlirik",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMDLyrics(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "mdinfo",
		Description: "mdinfo",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMDInfo(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "np",
		Aliases:     []string{"nowplaying", "mdnp"},
		Description: "np",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMDUI(client, msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "mdhapus",
		Description: "mdhapus",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleMDHapus(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "spotifytoken",
		Description: "spotifytoken",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleSpotifyToken(msg, args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "ytcookie",
		Description: "ytcookie",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleYTCookie(client, msg, args)
		},
	})
}
