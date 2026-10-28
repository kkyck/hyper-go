// ════════════════════════════════════════════════════════════
// plugins/call.go — REAL WhatsApp Call via meowcaller
// ════════════════════════════════════════════════════════════
//
// v2.10+ — meowcaller adalah SATU-SATUNYA mode .call
// (migrasi total dari whatsmeow ke hypermeow + meowcaller)
//
// API:
//   meowcaller.NewClient(wa)            — wrap hypermeow client
//   client.Call(ctx, target)            — place outbound audio call
//   client.CallWithOptions(ctx, t, {Video:true}) — place video call
//   client.GroupCallByID(ctx, groupJID) — call all group members
//   call.Play(meowcaller.MP3File(path)) — stream MP3 to peer
//   call.SendVideo(h264AccessUnit)      — stream H.264 video frame
//   call.OnReady(func(){})              — media flowing
//   call.OnEnd(func(reason){})          — call ended
//
// Audio format supported: .mp3, .wav, .opus (via meowcaller built-in decoders)
// Video: H.264 Annex-B via ffmpeg (libx264, no B-frames) → Call.SendVideo

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	meowcaller "github.com/purpshell/meowcaller"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// ─── Meowcaller global state ──────────────────────────────────────

var (
	meowcallerClient *meowcaller.Client
	meowcallerMu     sync.Mutex
	meowcallerReady  bool

	// active calls registry (call ID → *meowcaller.Call)
	activeCallsMu sync.Mutex
	activeCalls   = map[string]*meowcaller.Call{}
)

// callCheckUDP probe apakah UDP egress BENAR-BENAR bisa dipakai (butuh untuk
// media call). Caranya: kirim STUN binding request ke server STUN publik dan
// tunggu jawabannya. DNS tidak bisa dipakai sebagai probe — proxy meneruskan
// port 53 tapi membuang UDP port lain. Write-only juga tidak cukup.
//
// PENTING: UDP di VM ini INTERMITEN (kadang jalan, kadang diblokir), jadi
// TIDAK di-cache — cek fresh setiap kali sebelum place call. Probe max ~5 detik.
func callCheckUDP() bool {
	ok := false
	func() {
		defer func() { _ = recover() }()
		conn, err := net.DialTimeout("udp", "stun.l.google.com:19302", 4*time.Second)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		// STUN binding request: type=0x0001, len=0, magic=0x2112A442
		req := []byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42,
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
		if _, err := conn.Write(req); err != nil {
			return
		}
		resp := make([]byte, 512)
		n, err := conn.Read(resp)
		if err != nil || n < 20 {
			return
		}
		// Validasi: binding response (0x0101) + magic cookie cocok
		if resp[0] == 0x01 && resp[1] == 0x01 &&
			resp[4] == 0x21 && resp[5] == 0x12 && resp[6] == 0xA4 && resp[7] == 0x42 {
			ok = true
		}
	}()
	return ok
}

// callUDPBlockMsg pesan jujur saat UDP diblokir — daripada call nyangkut.
const callUDPBlockMsg = "❌ *Voice/video call tidak bisa sekarang*\n\n" +
	"Koneksi UDP dari server bot ke WhatsApp sedang diblokir/putus-putus. " +
	"Kalau dipaksa, HP kamu berdering tapi tidak ada suara/gambar (nyangkut lalu putus).\n\n" +
	"Coba lagi beberapa menit lagi — kadang jaringannya pulih sendiri."

// ─── Audio cache (avoid re-download + re-decode same audio) ──────
//
// If owner .call's the same voice note multiple times, we currently:
//   1. Download audio from WhatsApp servers (3-min timeout, network)
//   2. Write to temp file
//   3. Decode to AudioSource (CPU-heavy: MP3/WAV/Opus → PCM float32)
//
// With cache: we key by audio's FileSHA256 (WhatsApp-provided content hash).
// Same audio = same SHA = cache hit → skip steps 1-3.
// Cache stored in memory as decoded PCM bytes (16kHz mono float32).
//
// Trade-off: memory. 1MB audio = ~4MB decoded PCM. Cache max 10 entries
// = ~40MB max. Acceptable for a bot process.

var (
	audioCacheMu  sync.Mutex
	audioCache    = map[string]*cachedAudio{} // sha256hex → decoded audio
	audioCacheMax = 10
)

type cachedAudio struct {
	data      []byte // original ENCODED bytes (mp3/wav/opus) — decoded on use
	ext       string // ".mp3" / ".wav" / ".opus" — decoder selector
	seconds   uint32
	createdAt time.Time
	hits      int
}

// audioCacheKey computes cache key from audio message's FileSHA256.
// Returns empty string if hash not available (cache disabled for this msg).
func audioCacheKey(audioMsg *waE2E.AudioMessage) string {
	if audioMsg == nil {
		return ""
	}
	sha := audioMsg.GetFileSHA256()
	if len(sha) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", sha)
}

// audioCacheGet returns cached PCM if present and not expired.
func audioCacheGet(key string) (*cachedAudio, bool) {
	if key == "" {
		return nil, false
	}
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	c, ok := audioCache[key]
	if !ok {
		return nil, false
	}
	// TTL: 30 minutes (audio might change if user re-records)
	if time.Since(c.createdAt) > 30*time.Minute {
		delete(audioCache, key)
		return nil, false
	}
	c.hits++
	return c, true
}

// audioCachePut stores encoded audio bytes + ext. Evicts oldest if cache full.
func audioCachePut(key string, data []byte, ext string, seconds uint32) {
	if key == "" || len(data) == 0 {
		return
	}
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()

	// Evict oldest if at capacity
	if len(audioCache) >= audioCacheMax {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range audioCache {
			if oldestKey == "" || v.createdAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.createdAt
			}
		}
		delete(audioCache, oldestKey)
	}

	audioCache[key] = &cachedAudio{
		data:      data,
		ext:       ext,
		seconds:   seconds,
		createdAt: time.Now(),
	}
}

// audioCacheClear empties the cache (call from .callmeow clearcache).
func audioCacheClear() int {
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	n := len(audioCache)
	audioCache = map[string]*cachedAudio{}
	return n
}

// audioCacheStats returns (entries, total bytes, total hits).
func audioCacheStats() (entries, totalBytes, totalHits int) {
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	for _, c := range audioCache {
		entries++
		totalBytes += len(c.data)
		totalHits += c.hits
	}
	return
}

// safeShortKey returns first 12 chars of cache key (for logging).
// Returns "(none)" if key is empty.
func safeShortKey(key string) string {
	if key == "" {
		return "(none)"
	}
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

// humanBytes formats bytes as human-readable (KB/MB/GB).
func humanBytes(b int) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ─── Rate limiter for .call ───────────────────────────────────────
//
// Sliding window rate limiter: max N calls per window per owner.
// Uses sync.Mutex (not sync.RWMutex) because both read+write paths
// modify the bucket slice (append/filter).
//
// Memory bound: each owner's bucket is capped at callRateMax entries.
// Stale buckets (no activity > 5min) are cleaned up lazily on access
// and proactively via a background goroutine.

var (
	callRateMu      sync.Mutex
	callRateBuckets = map[string][]int64{} // owner → timestamps
)

const (
	callRateMax       = 5
	callRateWindow    = 60 * time.Second
	callRateBucketTTL = 5 * time.Minute // buckets with no activity are GC'd
)

// callRateAllow returns true if owner can still send .call within rate limit.
// Side effect: prunes expired timestamps from the owner's bucket.
func callRateAllow(ownerKey string) bool {
	callRateMu.Lock()
	defer callRateMu.Unlock()
	now := time.Now().UnixMilli()
	windowMs := int64(callRateWindow / time.Millisecond)

	// Filter to fresh timestamps only
	old := callRateBuckets[ownerKey]
	fresh := make([]int64, 0, callRateMax)
	for _, t := range old {
		if now-t < windowMs {
			fresh = append(fresh, t)
		}
	}

	if len(fresh) >= callRateMax {
		callRateBuckets[ownerKey] = fresh
		return false
	}
	fresh = append(fresh, now)
	callRateBuckets[ownerKey] = fresh
	return true
}

// callRateCleanup removes stale buckets (no activity > TTL).
// Called lazily from callRateAllow + proactively from a background ticker.
// This bounds memory: if owner only does .call once then never again,
// their bucket is GC'd after 5 minutes.
func callRateCleanup() {
	callRateMu.Lock()
	defer callRateMu.Unlock()
	now := time.Now().UnixMilli()
	ttlMs := int64(callRateBucketTTL / time.Millisecond)
	windowMs := int64(callRateWindow / time.Millisecond)

	for owner, bucket := range callRateBuckets {
		// Filter fresh timestamps
		fresh := bucket[:0]
		for _, t := range bucket {
			if now-t < windowMs {
				fresh = append(fresh, t)
			}
		}
		// If bucket is empty AND no activity in TTL, delete it
		if len(fresh) == 0 {
			// Check if last activity (if any) is older than TTL
			if len(bucket) == 0 || (now-bucket[len(bucket)-1]) > ttlMs {
				delete(callRateBuckets, owner)
				continue
			}
		}
		callRateBuckets[owner] = fresh
	}
}

// init starts a background goroutine that cleans up stale rate limiter
// buckets every minute. Fires once at package init.
func init() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			callRateCleanup()
		}
	}()

	RegisterPlugin(Plugin{
		Command:     "call",
		Description: "Telepon VoIP (meowcaller) + putar audio",
		Category:    "Call",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCallSendCommand(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "callmeow",
		Description: "Kontrol meowcaller (status/autoaccept)",
		Category:    "Call",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleCallMeowCommand(client, args, msg)
		},
	})
}

// initMeowcallerClient wraps the whatsmeow client with meowcaller.
// MUST be called BEFORE wa.Connect() so call event handlers are installed.
//
// This is called from main.go (also behind build tag) during startup.
func initMeowcallerClient(wa *whatsmeow.Client) {
	if wa == nil {
		fmt.Println("[MEOWCALLER] Skipping init: whatsmeow client is nil")
		return
	}

	// Panic recovery — don't crash bot if meowcaller init fails
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[MEOWCALLER] PANIC during init: %v\n", r)
			meowcallerClient = nil
			meowcallerReady = false
		}
	}()

	meowcallerMu.Lock()
	defer meowcallerMu.Unlock()

	if meowcallerClient != nil {
		return // already initialized
	}

	// Logger untuk diagnostik meowcaller (signaling, relay, media).
	// Output ke stdout → journalctl, prefix [MEOWCALLER].
	zlog := zerolog.New(os.Stdout).With().Timestamp().Logger().Output(
		zerolog.ConsoleWriter{Out: os.Stdout, NoColor: true, PartsOrder: []string{"message"}},
	)
	mc := meowcaller.NewClient(wa, meowcaller.WithLogger(zlog))
	if mc == nil {
		fmt.Println("[MEOWCALLER] NewClient returned nil — calls disabled")
		return
	}

	// Register incoming call handler (auto-reject by default for security)
	mc.OnIncomingCall(func(call *meowcaller.Call) {
		// Recovery inside callback — don't crash bot if call handling panics
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[MEOWCALLER] PANIC in incoming call handler: %v\n", r)
			}
		}()

		if call == nil {
			fmt.Println("[MEOWCALLER] Incoming call is nil — skipping")
			return
		}

		var peerStr string
		if p := call.Peer(); !p.IsEmpty() {
			peerStr = p.String()
		} else {
			peerStr = "(unknown)"
		}
		fmt.Printf("[MEOWCALLER] Incoming call from %s\n", peerStr)

		// Check if AI Call mode is ON — if so, auto-answer + start AI conversation
		if aicallOnIncomingCall(call) {
			return // handled by AI call system
		}

		// Default: auto-reject incoming calls
		// Owner can override by enabling auto-accept mode (.callmeow autoaccept on)
		if meowcallerAutoAccept() {
			if err := call.Answer(); err != nil {
				fmt.Printf("[MEOWCALLER] Answer failed: %v\n", err)
			}
		} else {
			if err := call.Reject(); err != nil {
				fmt.Printf("[MEOWCALLER] Reject failed: %v\n", err)
			}
		}
	})

	meowcallerClient = mc
	meowcallerReady = true
	fmt.Println("[MEOWCALLER] Client initialized — real WhatsApp calls enabled")
	// Peringatan dini: media call butuh UDP. Kalau diblokir, .call akan
	// menolak langsung dengan pesan jujur (tidak nyangkut).
	if !callCheckUDP() {
		fmt.Println("[MEOWCALLER] WARNING: UDP egress diblokir di server ini — voice/video call tidak bisa (signaling OK, media gagal).")
	}
}

// meowcallerAutoAccept returns true if incoming calls should be auto-answered.
// Default: false (reject all incoming calls).
// Toggle via: .callmeow autoaccept on|off
var (
	meowcallerAutoAcceptMu sync.RWMutex
	meowcallerAutoAcceptOn bool
)

func meowcallerAutoAccept() bool {
	meowcallerAutoAcceptMu.RLock()
	defer meowcallerAutoAcceptMu.RUnlock()
	return meowcallerAutoAcceptOn
}

// ─── .call command — REAL meowcaller implementation ──────────────

func handleCallSendCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	// Subcommand routing: .call status | .call active | .call hangup | .call help
	// These don't need audio reply, redirect to .callmeow handler.
	argsTrim := strings.TrimSpace(strings.ToLower(args))
	if argsTrim != "" {
		firstWord := strings.Fields(argsTrim)[0]
		switch firstWord {
		case "status", "stat":
			return callMeowcallerStatus()
		case "active", "list":
			return callMeowcallerActiveList()
		case "hangup", "end", "stop":
			return handleCallMeowCommand(client, args, msg)
		case "help", "?", "bantuan":
			return callMeowcallerHelp()
		case "autoaccept":
			return handleCallMeowCommand(client, args, msg)
		}
	}

	// ─── Flags: video / grup ───
	//   .call video ...  → video call (1:1)
	//   .call grup       → group call ke grup saat ini (audio)
	//   .call video grup → group video call
	isVideoCall := false
	isGroupCall := false
	callArgs := strings.TrimSpace(args)
	{
		f := strings.Fields(strings.ToLower(callArgs))
		if len(f) > 0 && (f[0] == "video" || f[0] == "v") {
			isVideoCall = true
			callArgs = strings.TrimSpace(callArgs[len(f[0]):])
			f = strings.Fields(strings.ToLower(callArgs))
		}
		if len(f) > 0 && (f[0] == "grup" || f[0] == "group") {
			isGroupCall = true
			callArgs = strings.TrimSpace(callArgs[len(f[0]):])
		}
	}

	meowcallerMu.Lock()
	mc := meowcallerClient
	ready := meowcallerReady
	meowcallerMu.Unlock()

	if !ready || mc == nil {
		return "❌ Meowcaller belum siap. Pastikan bot sudah connect ke WhatsApp.\n\nAtau cek: .callmeow status"
	}

	// Guard UDP: media call WA wajib UDP. Kalau diblokir, tolak langsung
	// dengan pesan jujur daripada call nyangkut 30 detik lalu timeout.
	if !callCheckUDP() {
		return callUDPBlockMsg
	}

	// Rate limit
	ownerKey := msg.Info.Sender.String()
	if !callRateAllow(ownerKey) {
		return fmt.Sprintf("⏳ Rate limit: maks %d .call per menit.", callRateMax)
	}

	// ─── MEDIA SOURCE: reply audio / video / kosong ───
	//   reply audio → audio itu yang diputar
	//   reply video → audionya diekstrak + framenya di-stream (untuk video call)
	//   tanpa reply → hanya diizinkan untuk video call (pola generik, tanpa audio)
	quoted := getQuotedMessage(msg)
	var audioMsg *waE2E.AudioMessage
	var videoMsg *waE2E.VideoMessage
	if quoted != nil && quoted.Message != nil {
		audioMsg = quoted.Message.GetAudioMessage()
		videoMsg = quoted.Message.GetVideoMessage()
		if audioMsg == nil && videoMsg == nil {
			if doc := quoted.Message.GetDocumentWithCaptionMessage(); doc != nil {
				audioMsg = doc.GetMessage().GetAudioMessage()
				videoMsg = doc.GetMessage().GetVideoMessage()
			}
		}
	}
	if audioMsg == nil && videoMsg == nil && !isVideoCall {
		return callMeowcallerHelp()
	}

	// Resolve target
	// meowcaller.Call(target string) accepts:
	//   - phone number (e.g. "+6281234567890" or "6281234567890")
	//   - phone JID (e.g. "6281234567890@s.whatsapp.net")
	//   - LID JID (e.g. "65138346446989@lid")
	//
	// We must pass FULL JID (with @server suffix) — not just user ID.
	// Passing bare user ID like "65138346446989" makes meowcaller treat it
	// as a phone number → usync lookup → "no LID" error.
	//
	// IMPORTANT: .call's reply target is the AUDIO SOURCE, not the call target.
	// So we check mentions + phone FIRST, and only fall back to sender for
	// ".call self" or ".call" (no args).
	var target string // 1:1 target
	var groupJID string

	if isGroupCall {
		// Group call: target = grup tempat command dikirim.
		// meowcaller resolve roster via GetGroupInfo (min 2, max 31 member remote).
		g := msg.Info.Chat.ToNonAD()
		if g.Server != types.GroupServer {
			return "❌ .call grup harus dikirim di dalam grup."
		}
		groupJID = g.String()
	} else {
		args = strings.TrimSpace(callArgs)
		argsLower := strings.ToLower(args)

		// Helper: convert JID to meowcaller-compatible string (prefer LID form)
		jidToTarget := func(j types.JID) string {
			j = j.ToNonAD()
			if j.IsEmpty() {
				return ""
			}
			// Prefer LID form (meowcaller is LID-first identity model)
			if j.Server == types.HiddenUserServer {
				return j.String()
			}
			// Phone JID — meowcaller will resolve via usync internally
			return j.String()
		}

		if args == "" || argsLower == "self" {
			// .call (no args) or .call self → call the sender (who triggers)
			sender := msg.Info.Sender.ToNonAD()
			senderAlt := msg.Info.SenderAlt.ToNonAD()

			if sender.Server == types.HiddenUserServer {
				target = sender.String()
			} else if !senderAlt.IsEmpty() && senderAlt.Server == types.HiddenUserServer {
				target = senderAlt.String()
			} else if !sender.IsEmpty() {
				target = sender.String()
			} else {
				return "❌ Tidak bisa resolve sender JID"
			}

		} else if strings.HasPrefix(args, "@") {
			// .call @tag — resolve via MentionedJIDList (NOT reply target!)
			// The reply in .call context is the audio source, not the call target.
			mentions := extractMentionedJIDsDual(msg)
			if len(mentions) == 0 {
				return "❌ Mention tidak terdeteksi. Pakai @ pilih kontak dari WhatsApp."
			}
			// Take first mention
			j, err := parseAnyJID(mentions[0])
			if err != nil || j.IsEmpty() {
				return fmt.Sprintf("❌ Mention JID tidak valid: %s (%v)", mentions[0], err)
			}
			target = jidToTarget(j)
			if target == "" {
				return "❌ Mention resolve ke JID kosong"
			}

		} else if strings.HasPrefix(args, "6") || strings.HasPrefix(args, "+") || strings.HasPrefix(args, "0") {
			// .call 628xxx or .call +628xxx or .call 08xxx — phone number
			// Strip non-digits, normalize
			phone := strings.TrimPrefix(args, "+")
			phone = strings.TrimPrefix(phone, "0")
			// If starts with country code, keep as is
			if !strings.HasPrefix(phone, "62") && !strings.HasPrefix(phone, "6") {
				// Indonesian number, prepend 62
				phone = "62" + phone
			}
			// Validate: 8-15 digits
			digitsOnly := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, phone)
			if len(digitsOnly) < 8 || len(digitsOnly) > 15 {
				return fmt.Sprintf("❌ Nomor tidak valid: %s (butuh 8-15 digit)", args)
			}
			target = digitsOnly + "@" + types.DefaultUserServer

		} else {
			// Fallback: try mentions first (for ".call" with mention in args)
			mentions := extractMentionedJIDsDual(msg)
			if len(mentions) > 0 {
				if j, err := parseAnyJID(mentions[0]); err == nil && !j.IsEmpty() {
					target = jidToTarget(j)
				}
			}
			if target == "" {
				return "❓ Target tidak valid. Pakai: *.call @tag* atau *.call 628xxx* atau *.call self*"
			}
		}

		if target == "" {
			return "❌ Target kosong"
		}
	} // end 1:1 target branch

	// ─── AUDIO DOWNLOAD + DECODE ────────────────────────────────
	// Strategy: cache by FileSHA256 to skip re-download + re-decode
	// for the same audio. Cache miss → full pipeline.
	cacheKey := audioCacheKey(audioMsg)
	var audioData []byte
	var seconds uint32
	var src meowcaller.AudioSource
	var tmpPath string
	var replyVideoPath string // temp video (reply video) — sumber frame video call + cleanup
	var preH264Path string    // hasil pre-transcode H.264 (agar call tidak encode realtime)
	var ext string

	if audioMsg == nil && videoMsg != nil {
		// ─── REPLY VIDEO: download video, ekstrak audionya ───
		fmt.Println("[MEOWCALLER] Reply is video — downloading...")
		dlCtx, dlCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		videoData, dlErr := client.Download(dlCtx, videoMsg)
		dlCancel()
		if dlErr != nil {
			return fmt.Sprintf("❌ Gagal download video: %v", dlErr)
		}
		if len(videoData) == 0 {
			return "❌ Video kosong."
		}
		vf, err := os.CreateTemp("", "wabot-call-video-*.mp4")
		if err != nil {
			return fmt.Sprintf("❌ Gagal buat temp video: %v", err)
		}
		replyVideoPath = vf.Name()
		if _, err := vf.Write(videoData); err != nil {
			vf.Close()
			_ = os.Remove(replyVideoPath)
			replyVideoPath = ""
			return fmt.Sprintf("❌ Gagal tulis temp video: %v", err)
		}
		vf.Close()
		fmt.Printf("[MEOWCALLER] Video saved: %s (%d bytes)\n", replyVideoPath, len(videoData))

		// Ekstrak audio → wav 16kHz mono (format native meowcaller)
		wavPath, err := extractAudioWAV(replyVideoPath)
		if err != nil {
			_ = os.Remove(replyVideoPath)
			replyVideoPath = ""
			return fmt.Sprintf("❌ Gagal ekstrak audio video: %v", err)
		}
		tmpPath = wavPath
		src, err = meowcaller.WAVFile(wavPath)
		if err != nil {
			_ = os.Remove(wavPath)
			_ = os.Remove(replyVideoPath)
			tmpPath, replyVideoPath = "", ""
			return fmt.Sprintf("❌ Gagal decode audio video: %v", err)
		}
		// Pre-transcode video ke H.264 SEKARANG (sebelum call) — gagal fast
		// dengan pesan jelas, dan call nanti tinggal stream tanpa encode realtime.
		// Hanya untuk video call; audio call tidak butuh frame video.
		if isVideoCall {
			fmt.Println("[MEOWCALLER] Pre-transcode video ke H.264...")
			h264Path, err := pretranscodeVideoH264(replyVideoPath)
			if err != nil {
				_ = os.Remove(wavPath)
				_ = os.Remove(replyVideoPath)
				tmpPath, replyVideoPath = "", ""
				return fmt.Sprintf("❌ Gagal pre-transcode video: %v", err)
			}
			preH264Path = h264Path
			fmt.Printf("[MEOWCALLER] Pre-transcode OK: %s\n", preH264Path)
		}
		seconds = videoMsg.GetSeconds()
	} else if audioMsg != nil {
		if cached, ok := audioCacheGet(cacheKey); ok {
			// CACHE HIT — tulis bytes asli + decode pakai decoder yang sesuai ext.
			// (Dulu bug: bytes MP3 ditulis sebagai .pcm mentah → audio rusak.)
			fmt.Printf("[MEOWCALLER] Audio cache HIT (key=%s, %d bytes, hits=%d)\n",
				safeShortKey(cacheKey), len(cached.data), cached.hits)
			audioData = cached.data
			seconds = cached.seconds
			ext = cached.ext
			if ext == "" {
				ext = ".mp3"
			}

			tmpFile, err := os.CreateTemp("", "wabot-call-cached-*"+ext)
			if err != nil {
				return fmt.Sprintf("❌ Gagal buat temp file (cache): %v", err)
			}
			tmpPath = tmpFile.Name()
			if _, err := tmpFile.Write(audioData); err != nil {
				tmpFile.Close()
				_ = os.Remove(tmpPath)
				return fmt.Sprintf("❌ Gagal tulis temp file (cache): %v", err)
			}
			tmpFile.Close()

			var decodeErr error
			switch ext {
			case ".wav":
				src, decodeErr = meowcaller.WAVFile(tmpPath)
			case ".opus":
				src, decodeErr = meowcaller.OpusFile(tmpPath)
			default:
				src, decodeErr = meowcaller.MP3File(tmpPath)
			}
			if decodeErr != nil {
				_ = os.Remove(tmpPath)
				tmpPath = ""
				return fmt.Sprintf("❌ Gagal decode audio (cache): %v", decodeErr)
			}
		} else {
			// CACHE MISS — full pipeline: download + write temp + decode
			fmt.Printf("[MEOWCALLER] Audio cache MISS (key=%s) — downloading...\n",
				safeShortKey(cacheKey))

			dlCtx, dlCancel := context.WithTimeout(context.Background(), 3*time.Minute)
			var dlErr error
			audioData, dlErr = client.Download(dlCtx, audioMsg)
			dlCancel()
			if dlErr != nil {
				return fmt.Sprintf("❌ Gagal download audio: %v", dlErr)
			}

			mime := audioMsg.GetMimetype()
			if mime == "" {
				mime = "audio/mpeg"
			}

			// Determine file extension from mime
			ext = ".mp3"
			switch {
			case strings.Contains(mime, "wav"):
				ext = ".wav"
			case strings.Contains(mime, "opus"):
				ext = ".opus"
			case strings.Contains(mime, "ogg"):
				ext = ".opus"
			case strings.Contains(mime, "mp3"), strings.Contains(mime, "mpeg"):
				ext = ".mp3"
			}

			tmpFile, err := os.CreateTemp("", "wabot-call-*"+ext)
			if err != nil {
				return fmt.Sprintf("❌ Gagal buat temp file: %v", err)
			}
			tmpPath = tmpFile.Name()
			if _, err := tmpFile.Write(audioData); err != nil {
				tmpFile.Close()
				_ = os.Remove(tmpPath)
				return fmt.Sprintf("❌ Gagal tulis temp file: %v", err)
			}
			tmpFile.Close()

			fmt.Printf("[MEOWCALLER] Audio saved: %s (%d bytes, %s)\n", tmpPath, len(audioData), ext)

			// Open audio source via meowcaller (extension-based)
			var decodeErr error
			switch strings.ToLower(filepath.Ext(tmpPath)) {
			case ".mp3":
				src, decodeErr = meowcaller.MP3File(tmpPath)
			case ".wav":
				src, decodeErr = meowcaller.WAVFile(tmpPath)
			case ".opus":
				src, decodeErr = meowcaller.OpusFile(tmpPath)
			default:
				_ = os.Remove(tmpPath)
				return fmt.Sprintf("❌ Format tidak didukung: %s (butuh .mp3/.wav/.opus)", ext)
			}
			if decodeErr != nil {
				_ = os.Remove(tmpPath)
				return fmt.Sprintf("❌ Gagal decode audio: %v", decodeErr)
			}

			seconds = audioMsg.GetSeconds()
			if seconds == 0 {
				seconds = uint32(len(audioData) / 14000)
				if seconds == 0 {
					seconds = 10
				}
			}

			// Store in cache (decode + download result).
			// We store the ORIGINAL audio bytes (not decoded PCM) — re-decoding
			// from cached bytes is faster than re-downloading from WhatsApp.
			audioCachePut(cacheKey, audioData, ext, seconds)
		}
	} // end audioMsg branch (audioMsg==nil && videoMsg==nil → tanpa audio, khusus video call)

	// src is set (either from cache or fresh decode)
	// tmpPath is set (either cached PCM temp or fresh decode temp)
	// seconds is set, ext is set

	// Place the call
	// Use a long-lived context (5 minutes max for a call).
	// We DON'T defer callCancel() here because that would cancel the call
	// the moment this function returns. The context lives until the call ends.
	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Minute)

	// ─── VIDEO ARGS (ffmpeg → H.264 Annex-B mentah) ───
	var videoFFmpegArgs []string
	if isVideoCall {
		if preH264Path != "" {
			// Video sudah di-pre-transcode ke H.264 — tinggal stream tanpa
			// encode realtime (-c:v copy, nyaris nol CPU). -re agar pacing realtime.
			videoFFmpegArgs = []string{
				"-hide_banner", "-loglevel", "error",
				"-re", "-i", preH264Path,
				"-c:v", "copy",
				"-f", "h264", "-",
			}
		} else {
			// Pola generik animasi (test pattern) kalau tidak reply video
			videoFFmpegArgs = []string{
				"-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc=size=640x480:rate=15",
				"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
				"-bf", "0", "-g", "30",
				"-f", "h264", "-",
			}
		}
	}

	modeStr := "audio"
	if isVideoCall {
		modeStr = "video"
	}
	dialTarget := target
	if isGroupCall {
		modeStr += " group"
		dialTarget = groupJID
	}
	fmt.Printf("[MEOWCALLER] Placing %s call to %s...\n", modeStr, dialTarget)

	// Panic recovery around call placement + setup
	var call *meowcaller.Call
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("panic during call: %v", r)
				fmt.Printf("[MEOWCALLER] PANIC: %v\n", r)
			}
		}()
		if isGroupCall {
			call, callErr = mc.GroupCallByIDWithOptions(callCtx, groupJID, meowcaller.GroupCallOptions{Video: isVideoCall})
		} else if isVideoCall {
			call, callErr = mc.CallWithOptions(callCtx, target, meowcaller.CallOptions{Video: true})
		} else {
			call, callErr = mc.Call(callCtx, target)
		}
	}()
	if callErr != nil {
		callCancel()
		if src != nil {
			_ = src.Close()
		}
		_ = os.Remove(tmpPath)
		_ = os.Remove(replyVideoPath)
		_ = os.Remove(preH264Path)
		hint := "Pastikan target online dan bukan bot sendiri."
		if isGroupCall {
			hint = "Pastikan grup punya min 2 member remote (selain bot), max 31."
		}
		return fmt.Sprintf("❌ Gagal place call: %v\n\n%s", callErr, hint)
	}
	if call == nil {
		callCancel()
		if src != nil {
			_ = src.Close()
		}
		_ = os.Remove(tmpPath)
		_ = os.Remove(replyVideoPath)
		_ = os.Remove(preH264Path)
		return "❌ meowcaller.Call returned nil call (no error)"
	}

	callID := call.ID()
	if callID == "" {
		callID = fmt.Sprintf("unknown-%d", time.Now().UnixNano())
	}

	// Register call in active calls map
	activeCallsMu.Lock()
	activeCalls[callID] = call
	activeCallsMu.Unlock()

	// Cleanup on call end (with recovery — don't panic if cleanup fails)
	// This is where src.Close(), temp file removal, and context cancel happen.
	// OnEnd fires when:
	//   - target hangs up
	//   - audio finishes (if OnFinish calls Hangup)
	//   - .callmeow hangup <id>
	//   - call times out (5 min)
	call.OnEnd(func(reason string) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[MEOWCALLER] PANIC in OnEnd: %v\n", r)
			}
		}()
		fmt.Printf("[MEOWCALLER] Call %s ended: %s — cleaning up resources\n", callID, reason)

		// Close audio source first (stops any pending reads)
		if src != nil {
			_ = src.Close()
		}
		// Cancel call context (juga menghentikan video stream ffmpeg)
		callCancel()
		// Remove temp files
		_ = os.Remove(tmpPath)
		_ = os.Remove(replyVideoPath)
		_ = os.Remove(preH264Path)
		// Remove from active calls registry
		activeCallsMu.Lock()
		delete(activeCalls, callID)
		activeCallsMu.Unlock()
	})

	// Stream audio + video when media is ready (with recovery — Play can panic on bad source)
	// OnReady fires when: relay bound, first frames exchanged.
	// This may be AFTER the function returns — that's OK because src + call
	// are kept alive by closures + activeCalls registry.
	call.OnReady(func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[MEOWCALLER] PANIC in OnReady/Play: %v\n", r)
			}
		}()
		fmt.Printf("[MEOWCALLER] Media ready (call %s)...\n", callID)
		if src != nil {
			fmt.Printf("[MEOWCALLER] Streaming audio to peer (call %s)...\n", callID)
			// call.Play creates a Player, subscribes it, starts src.
			// Player pulls frames from src on 60ms cadence until io.EOF.
			// When src reaches EOF, Player fires OnFinish (we don't hook it).
			// The call continues until peer hangs up or we call Hangup.
			player := call.Play(src)
			if player == nil {
				fmt.Printf("[MEOWCALLER] WARNING: call.Play returned nil player (audio may not stream)\n")
			} else {
				// Auto-hangup when audio finishes (so the call doesn't stay open in silence)
				player.OnFinish(func() {
					defer func() {
						if r := recover(); r != nil {
							fmt.Printf("[MEOWCALLER] PANIC in OnFinish: %v\n", r)
						}
					}()
					fmt.Printf("[MEOWCALLER] Audio finished for call %s — hanging up\n", callID)
					_ = call.Hangup()
				})
			}
		} else {
			fmt.Printf("[MEOWCALLER] No audio source (video-only call %s)\n", callID)
		}
		if isVideoCall {
			fmt.Printf("[MEOWCALLER] Starting H.264 video stream (call %s)...\n", callID)
			if err := startVideoStream(callCtx, call, videoFFmpegArgs, 15); err != nil {
				fmt.Printf("[MEOWCALLER] Video stream failed to start: %v\n", err)
			}
		}
	})

	// Silent on success — no notification message to owner.
	// Call is already placed; target will receive incoming call ringing.
	return ""
}

// buildTextMessage creates a waE2E text message for sending notifications.
func buildTextMessage(text string) *waE2E.Message {
	return &waE2E.Message{
		Conversation: proto.String(text),
	}
}

// ─── Video call helpers (ffmpeg → H.264 Annex-B → Call.SendVideo) ───
//
// meowcaller tidak encode pixel (no pure-Go H.264 encoder) — video harus
// dikirim sebagai H.264 Annex-B access units via Call.SendVideo.
// Kita pakai ffmpeg (libx264, tanpa B-frame) lalu parse stdout-nya
// menjadi access units (1 frame) dan kirim dengan pacing sesuai fps.

// pretranscodeVideoH264 men-transcode video ke H.264 Annex-B (file) SEKALI di
// awal, SEBELUM call dipasang. Tujuannya: gagal fast kalau ffmpeg/transcode
// bermasalah (pesan error jelas ke user), dan saat call berjalan tidak ada
// lagi encoding realtime yang berat di CPU — tinggal stream file dengan
// -c:v copy (nyaris nol CPU).
// Return path file .h264 temp.
func pretranscodeVideoH264(videoPath string) (string, error) {
	out, err := os.CreateTemp("", "wabot-call-h264-*.h264")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	out.Close()
	// Timeout 10 menit — tanpa ini ffmpeg hang = .call hang selamanya.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", videoPath,
		"-vf", "scale=640:480:force_original_aspect_ratio=decrease,pad=640:480:(ow-iw)/2:(oh-ih)/2,fps=15",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-bf", "0", "-g", "30",
		"-f", "h264", outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("ffmpeg pre-transcode: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}
	// Pastikan file hasil tidak kosong
	if st, err := os.Stat(outPath); err != nil || st.Size() == 0 {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("ffmpeg pre-transcode menghasilkan file kosong")
	}
	return outPath, nil
}

// extractAudioWAV mengekstrak audio dari video ke wav 16kHz mono
// (format native meowcaller). Return path file wav temp.
func extractAudioWAV(videoPath string) (string, error) {
	out, err := os.CreateTemp("", "wabot-call-aud-*.wav")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	out.Close()
	// Timeout 5 menit — tanpa ini ffmpeg hang = .call hang selamanya.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", videoPath,
		"-vn", "-ar", "16000", "-ac", "1",
		"-f", "wav", outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("ffmpeg: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return outPath, nil
}

// startVideoStream menjalankan ffmpeg dengan args yang diberikan, mem-parse
// stdout H.264 Annex-B menjadi access units, dan mengirimkannya ke call.
// Berhenti saat ctx dibatalkan (call end) atau SendVideo error.
// ffmpeg di-kill otomatis saat stream selesai.
func startVideoStream(ctx context.Context, call *meowcaller.Call, ffmpegArgs []string, fps float64) error {
	if call == nil || len(ffmpegArgs) == 0 {
		return fmt.Errorf("video stream: call/args kosong")
	}
	cmd := exec.Command("ffmpeg", ffmpegArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("video stream stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("video stream ffmpeg start: %w", err)
	}
	fmt.Printf("[MEOWCALLER] ffmpeg video started (pid=%d)\n", cmd.Process.Pid)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[MEOWCALLER] PANIC in video stream: %v\n", r)
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}()
		if err := pushAnnexBFrames(ctx, stdout, call, fps); err != nil {
			if ctx.Err() == nil {
				fmt.Printf("[MEOWCALLER] video stream ended: %v (ffmpeg stderr: %s)\n",
					err, strings.TrimSpace(stderr.String()))
			}
		} else {
			fmt.Println("[MEOWCALLER] video stream finished cleanly")
		}
	}()
	return nil
}

// findAnnexBStartCodes mencari posisi semua start code (00 00 01 / 00 00 00 01)
// di buffer b. Return indeks awal tiap start code.
func findAnnexBStartCodes(b []byte) []int {
	var pos []int
	i := 0
	for i+3 <= len(b) {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				pos = append(pos, i)
				i += 3
				continue
			}
			if i+4 <= len(b) && b[i+2] == 0 && b[i+3] == 1 {
				pos = append(pos, i)
				i += 4
				continue
			}
		}
		i++
	}
	return pos
}

// h264BitReader membaca bit per bit untuk parse Exp-Golomb di slice header.
type h264BitReader struct {
	data []byte
	bit  int
}

func (b *h264BitReader) readBit() (int, bool) {
	if b.bit/8 >= len(b.data) {
		return 0, false
	}
	v := (b.data[b.bit/8] >> (7 - (b.bit % 8))) & 1
	b.bit++
	return int(v), true
}

// readUE membaca unsigned Exp-Golomb code.
func (b *h264BitReader) readUE() (int, bool) {
	zeros := 0
	for {
		bit, ok := b.readBit()
		if !ok {
			return 0, false
		}
		if bit == 1 {
			break
		}
		zeros++
		if zeros > 32 {
			return 0, false
		}
	}
	val := 1
	for i := 0; i < zeros; i++ {
		bit, ok := b.readBit()
		if !ok {
			return 0, false
		}
		val = (val << 1) | bit
	}
	return val - 1, true
}

// sliceFirstMB membaca first_mb_in_slice dari slice header H.264.
// sliceData = isi NAL tanpa start code dan tanpa NAL header byte.
// Return -1 jika parse gagal.
func sliceFirstMB(sliceData []byte) int {
	if len(sliceData) < 1 {
		return -1
	}
	// hapus emulation prevention bytes (00 00 03 → 00 00)
	rbsp := make([]byte, 0, len(sliceData))
	for i := 0; i < len(sliceData); i++ {
		if i+2 < len(sliceData) && sliceData[i] == 0 && sliceData[i+1] == 0 && sliceData[i+2] == 3 {
			rbsp = append(rbsp, 0, 0)
			i += 2
		} else {
			rbsp = append(rbsp, sliceData[i])
		}
	}
	br := &h264BitReader{data: rbsp}
	v, ok := br.readUE() // first_mb_in_slice adalah field pertama slice header
	if !ok {
		return -1
	}
	return v
}

// pushAnnexBFrames membaca raw H.264 Annex-B dari r, mengelompokkan NAL units
// menjadi access units (1 frame), dan mengirim via call.SendVideo dengan pacing fps.
// Asumsi encoder tanpa B-frame (-bf 0): batas frame = NAL VCL (type 1-5) baru
// setelah unit saat ini sudah berisi VCL.
func pushAnnexBFrames(ctx context.Context, r io.Reader, call *meowcaller.Call, fps float64) error {
	if fps <= 0 {
		fps = 15
	}
	ticker := time.NewTicker(time.Duration(float64(time.Second) / fps))
	defer ticker.Stop()

	var cur []byte // access unit berjalan (dengan start code)
	var curHasVCL bool
	frames := 0

	// send mengirim access unit saat ini dengan pacing.
	send := func() error {
		if len(cur) == 0 {
			return nil
		}
		au := cur
		cur = nil
		curHasVCL = false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if err := call.SendVideo(au); err != nil {
			return fmt.Errorf("sendvideo frame %d: %w", frames+1, err)
		}
		frames++
		return nil
	}

	// feed memasukkan satu NAL (termasuk start code) ke framer.
	// feed memasukkan satu NAL (termasuk start code) ke framer.
	// Batas frame dideteksi via first_mb_in_slice==0 di slice header
	// (akurat walau encoder pakai multi-slice per frame).
	feed := func(nal []byte) error {
		if len(nal) < 5 {
			return nil
		}
		// indeks header byte: 3 untuk 00 00 01, 4 untuk 00 00 00 01
		hi := 3
		if !(nal[0] == 0 && nal[1] == 0 && nal[2] == 1) {
			hi = 4
		}
		if hi >= len(nal) {
			return nil
		}
		typ := nal[hi] & 0x1F
		isVCL := typ >= 1 && typ <= 5
		if isVCL {
			firstMB := sliceFirstMB(nal[hi+1:])
			// firstMB==0 → slice pertama dari frame baru.
			// firstMB==-1 (parse gagal) → anggap lanjutan (aman: tidak pecah frame).
			if firstMB == 0 && curHasVCL {
				if err := send(); err != nil {
					return err
				}
			}
			cur = append(cur, nal...)
			curHasVCL = true
		} else {
			// non-VCL (SPS/PPS/SEI) — tempel ke unit berjalan
			cur = append(cur, nal...)
		}
		// sanity: satu frame jangan lebih dari 8MB
		if len(cur) > 8<<20 {
			return fmt.Errorf("access unit > 8MB (stream korup?)")
		}
		return nil
	}

	buf := make([]byte, 64*1024)
	var tail []byte // sisa chunk sebelumnya (selalu diawali start code)
	for {
		// cek ctx tiap iterasi
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			data := append(tail, buf[:n]...)
			tail = nil
			positions := findAnnexBStartCodes(data)
			if len(positions) == 0 {
				// tidak ada start code — seluruh data milik NAL berjalan
				tail = data
				if len(tail) > 8<<20 {
					return fmt.Errorf("tanpa start code dalam 8MB")
				}
			} else {
				// prefix sebelum start code pertama = sampah (hanya di read pertama)
				for k := 0; k+1 < len(positions); k++ {
					if err := feed(data[positions[k]:positions[k+1]]); err != nil {
						return err
					}
				}
				// NAL terakhir belum lengkap → simpan sebagai tail
				tail = append([]byte(nil), data[positions[len(positions)-1]:]...)
			}
		}
		if rerr != nil {
			// EOF / error: feed sisa tail sebagai NAL terakhir lalu flush
			if len(tail) > 0 {
				_ = feed(tail)
			}
			_ = send()
			if rerr == io.EOF {
				fmt.Printf("[MEOWCALLER] video: EOF setelah %d frames\n", frames)
				return nil
			}
			return rerr
		}
	}
}

// ─── .callmeow command — meowcaller-specific controls ────────────

func handleCallMeowCommand(client *whatsmeow.Client, args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	parts := strings.Fields(strings.TrimSpace(strings.ToLower(args)))
	if len(parts) == 0 {
		return callMeowcallerStatus()
	}

	switch parts[0] {
	case "autoaccept":
		if len(parts) < 2 {
			return "❓ .callmeow autoaccept on|off"
		}
		meowcallerAutoAcceptMu.Lock()
		meowcallerAutoAcceptOn = parts[1] == "on"
		meowcallerAutoAcceptMu.Unlock()
		state := "OFF (reject all incoming)"
		if meowcallerAutoAcceptOn {
			state = "ON (auto-answer incoming)"
		}
		return fmt.Sprintf("✅ Auto-accept %s", state)

	case "status":
		return callMeowcallerStatus()

	case "active":
		return callMeowcallerActiveList()

	case "cekgrup":
		return handleCallMeowCekGrup(client, msg)

	case "cache":
		if len(parts) < 2 {
			// show cache stats
			entries, totalBytes, totalHits := audioCacheStats()
			return fmt.Sprintf(`📦 *Audio Cache Stats*

• Entries:    %d / %d
• Total size: %s
• Total hits: %d

Commands:
  .callmeow cache stats   — lihat statistik ini
  .callmeow cache clear   — hapus semua cache`, entries, audioCacheMax, humanBytes(totalBytes), totalHits)
		}
		switch parts[1] {
		case "stats", "status":
			entries, totalBytes, totalHits := audioCacheStats()
			return fmt.Sprintf("📦 *Cache*: %d entries, %s, %d hits", entries, humanBytes(totalBytes), totalHits)
		case "clear", "purge", "reset":
			n := audioCacheClear()
			return fmt.Sprintf("✅ Cache dibersihkan: %d entries dihapus", n)
		default:
			return "❓ .callmeow cache stats|clear"
		}

	case "hangup":
		if len(parts) < 2 {
			return "❓ .callmeow hangup <call-id>  (atau .callmeow hangup all)"
		}
		if parts[1] == "all" {
			activeCallsMu.Lock()
			count := len(activeCalls)
			for _, call := range activeCalls {
				call.Hangup()
			}
			activeCalls = map[string]*meowcaller.Call{}
			activeCallsMu.Unlock()
			return fmt.Sprintf("✅ Hangup %d call(s)", count)
		}
		activeCallsMu.Lock()
		call, ok := activeCalls[parts[1]]
		activeCallsMu.Unlock()
		if !ok {
			return "❌ Call ID tidak ditemukan. Lihat .callmeow active"
		}
		call.Hangup()
		return fmt.Sprintf("✅ Hangup call %s", parts[1])

	default:
		return callMeowcallerHelp()
	}
}

// handleCallMeowCekGrup — diagnostic: cek apakah grup saat ini bisa di-group-call.
// Menampilkan jumlah partisipan, berapa yang jadi target remote, dan masalahnya.
func handleCallMeowCekGrup(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "❌ Client tidak siap"
	}
	g := msg.Info.Chat.ToNonAD()
	if g.Server != types.GroupServer {
		return "❌ .callmeow cekgrup harus dikirim di dalam grup."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := client.GetGroupInfo(ctx, g)
	if err != nil {
		return fmt.Sprintf("❌ GetGroupInfo gagal: %v", err)
	}
	if info == nil {
		return "❌ GetGroupInfo return nil"
	}
	// Hitung self JIDs (bot sendiri)
	selfJIDs := map[string]bool{}
	for _, sj := range []types.JID{client.Store.GetJID().ToNonAD(), client.Store.GetLID().ToNonAD()} {
		if !sj.IsEmpty() {
			selfJIDs[sj.String()] = true
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "👥 *Cek Grup Call*\n\n• Grup: %s\n• Nama: %s\n• Total partisipan: %d\n\n", g.String(), info.Name, len(info.Participants))
	remote := 0
	for _, p := range info.Participants {
		jid := p.JID.ToNonAD().String()
		lid := p.LID.ToNonAD().String()
		phone := p.PhoneNumber.ToNonAD().String()
		isSelf := selfJIDs[jid] || (lid != "" && selfJIDs[lid]) || (phone != "" && selfJIDs[phone])
		mark := "✓"
		if isSelf {
			mark = "(bot)"
		} else {
			remote++
		}
		display := jid
		if display == "" {
			display = lid
		}
		if display == "" {
			display = phone
		}
		fmt.Fprintf(&sb, "%s %s\n", mark, display)
	}
	fmt.Fprintf(&sb, "\n• Remote member: %d\n", remote)
	if remote < 2 {
		fmt.Fprintf(&sb, "\n❌ BUTUH min 2 member remote untuk group call.")
	} else if remote > 31 {
		fmt.Fprintf(&sb, "\n❌ MAKS 31 member remote untuk group call.")
	} else {
		fmt.Fprintf(&sb, "\n✅ Grup ini BISA di-group-call.")
	}
	return sb.String()
}

// callMeowcallerActiveList returns formatted list of active calls.
// Used by .call active and .callmeow active.
func callMeowcallerActiveList() string {
	activeCallsMu.Lock()
	count := len(activeCalls)
	ids := make([]string, 0, count)
	for id := range activeCalls {
		ids = append(ids, id)
	}
	activeCallsMu.Unlock()
	if count == 0 {
		return "📭 Tidak ada call aktif"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📞 *%d call aktif*\n\n", count))
	for _, id := range ids {
		sb.WriteString(fmt.Sprintf("• %s\n", id))
	}
	return sb.String()
}

func callMeowcallerStatus() string {
	meowcallerMu.Lock()
	ready := meowcallerReady
	mc := meowcallerClient != nil
	meowcallerMu.Unlock()

	status := "❌ Tidak aktif (build tanpa tag meowcaller)"
	if mc && ready {
		status = "✅ Aktif — real WhatsApp calls enabled"
	}

	autoAccept := "OFF (reject incoming)"
	meowcallerAutoAcceptMu.RLock()
	if meowcallerAutoAcceptOn {
		autoAccept = "ON (auto-answer)"
	}
	meowcallerAutoAcceptMu.RUnlock()

	activeCallsMu.Lock()
	activeCount := len(activeCalls)
	activeCallsMu.Unlock()

	return fmt.Sprintf(`📞 *Meowcaller Status*

• Mode:        %s
• Auto-accept: %s
• Active calls: %d

Commands:
  .call (reply audio) — place real WhatsApp call + play audio
  .callmeow status     — lihat status ini
  .callmeow autoaccept on|off
  .callmeow active     — list active calls
  .callmeow hangup <id|all>

Build info:
  Tag: meowcaller (active)
  Library: github.com/purpshell/meowcaller
  WhatsApp: github.com/polymorfa/hypermeow (fork of whatsmeow)`, status, autoAccept, activeCount)
}

func callMeowcallerHelp() string {
	return `📞 *REAL WhatsApp Call (meowcaller)*

*Audio call:*
• *.call* (reply audio) — Telepon yg trigger + putar lagu
• *.call @tag* (reply audio) — Telepon @tag
• *.call 628xxx* (reply audio) — Telepon nomor
• *.call self* (reply audio) — Test telepon diri sendiri

*Video call* 📹
• *.call video* (reply audio) — Video call + pola animasi + putar lagu
• *.call video* (reply video) — Video call, stream video-nya + audionya
• *.call video @tag* — Video call ke @tag
• *.call v 628xxx* — (v = singkatan video)

*Group call* 👥
• *.call grup* (reply audio, di dalam grup) — Telepon semua member grup
• *.call video grup* (di dalam grup) — Video call grup
  (min 2, max 31 member remote)

Audio support: .mp3, .wav, .opus
Video: reply video apa saja (di-transcode otomatis) atau pola generik

⚠️ Ini REAL WhatsApp VoIP call — target akan ditelpon beneran.
   Berbeda dari voice note (auto-play PT).

Controls:
  .callmeow status      — status meowcaller
  .callmeow autoaccept on|off — toggle incoming call handling
  .callmeow active      — list active calls
  .callmeow hangup all  — hangup semua call

Rate limit: 5 .call per menit (anti-spam)`
}

// keep imports used
var _ = types.DefaultUserServer
