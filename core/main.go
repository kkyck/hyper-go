package main

/**
 * 🤖 hyper-go — Entry point
 * ════════════════════════════════════════════════════════════
 * Stack: hypermeow + PostgreSQL or SQLite (pure Go) + Web UI QR
 *
 * Modul:
 *   main.go              → koneksi, event, command router (prefix .)
 *   storage.go           → backend selection (postgres / sqlite / auto)
 *   session_sqlite.go    → session store (backend aktif)
 *   filestore.go         → JSON file store untuk level/guard/afk/sticker/kv
 *   selfmode.go          → self mode (default ON), owner check via OWNER_JID
 *   stickers.go          → sticker maker
 *   dl_kangwifi.go       → client download worker
 *   admincmd.go          → tagall, kick, promote, eval, shell, broadcast
 *   autopresence.go      → auto-read, auto-typing
 *   guard.go             → antilink, antispam, welcome (filestore)
 *   afk.go               → AFK (filestore)
 *   rss.go / rusage_unix.go → runtime stats (.stats, .proses, .rss)
 *   webui.go             → QR di browser
 *   endpoints.go         → endpoint pihak ketiga (semua dari environment)
 *   extras.go            → quote, fakta, groupinfo, hidetag
 *   menu.go              → comprehensive menu
 *
 * Prefix command: .
 * Env:
 *   OWNER_JID           → owner JIDs (comma-separated, with or without @s.whatsapp.net)
 *   OWNER_NAME          → display name
 *   STORAGE             → postgres | sqlite | auto (default auto)
 *   POSTGRES_DSN        → DSN PostgreSQL (wajib bila STORAGE=postgres)
 *   SQLITE_DB           → path file SQLite (default ./data/bot.db)
 *   WEB_PORT            → Web UI port (default 3000)
 *   KANGWIFI_API_BASE   → base URL API media (opsional)
 */

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	waLog "github.com/polymorfa/hypermeow/util/log"
)

// botClient global so admincmd (broadcast) and other helpers can access it.
var botClient *whatsmeow.Client

func main() {
	// Load .env first so OWNER_JID / OWNER_LID / etc. available
	loadDotEnv()

	// ─── Memory tuning (reduce RSS bloat) ──────────────────
	// GOGC=20: trigger GC when heap grows 20% (very aggressive)
	// GOMEMLIMIT=200MB: soft cap — Go tries to stay under 200MB
	// Periodic FreeOSMemory: return unused memory to OS every 60s
	os.Setenv("GOGC", "20")
	debug.SetGCPercent(20)
	debug.SetMemoryLimit(200 * 1024 * 1024) // 200MB soft cap
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			runtime.GC()
			debug.FreeOSMemory()
			cleanupSpamTracker()
		}
	}()

	// Initialize storage (PostgreSQL or SQLite) for ALL bot databases
	pgInitAll()

	// Initialize ruby.db for AI conversation memory + thinking logs
	rubyDBInit()

	// Initialize CRM database (message cache for .crm/.insp/.relay)
	fioraCRMDBInit()

	// Start plugin lifecycle sweeper (auto-kill idle plugins)
	startPluginSweeper()

	logger := waLog.Stdout("Bot", "INFO", true)
	ctx := context.Background()

	// Log owner config (mask middle digits)
	{
		jids := ownerJIDs()
		if len(jids) == 0 {
			fmt.Println("⚠️  OWNER_JID / OWNER_LID belum di-set di .env — self mode tidak punya owner!")
		} else {
			fmt.Printf("👑 Owner identities (%d):\n", len(jids))
			for _, j := range jids {
				fmt.Printf("   • %s\n", j)
			}
		}
	}

	// ─── Initialize file-based stores ──────────────────────────
	_ = os.MkdirAll(dataDir, 0o755)
	loadGuardsFromDisk()
	loadAFKsFromDisk()
	loadStickerStore()
	loadSelfMode()
	loadAutoPresence()
	initGlobalKV()

	// ─── Initialize SQLite session store ───────────────────────
	container, err := initSQLiteStore(logger)
	if err != nil {
		fmt.Printf("❌ Gagal init SQLite: %v\n", err)
		os.Exit(1)
	}
	defer container.Close()

	// Patch the session tables with the extra columns the bot reads
	// (whatsmeow_device.platform, whatsmeow_sender_keys.created_at).
	// Must run AFTER the session store created them.
	pgInitSessionSchema()

	// ─── Start Web UI for QR ───────────────────────────────────
	startWebUI()

	// ─── Get first device ──────────────────────────────────────
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		fmt.Printf("❌ Gagal mengambil device: %v\n", err)
		os.Exit(1)
	}

	client := whatsmeow.NewClient(device, logger)
	initMeowcallerIfEnabled(client) // meowcaller VoIP — harus sebelum Connect()
	// Observability: log tiap kegagalan auto-reconnect (tetap retry).
	client.AutoReconnectHook = func(err error) bool {
		fmt.Printf("🔌 [RECONNECT] auto-reconnect gagal: %v (retry lanjut)\n", err)
		return true
	}
	botClient = client
	setWebClient(client)
	installSPRHooks(client) // anti-bocor SPR sebelum event apapun
	// Wire anti-SPR owner notify → DM ke semua owner (sebelumnya placebo)
	SetOwnerBroadcastFn(func(text string) {
		for _, oj := range ownerJIDs() {
			jid, err := parseAnyJID(oj)
			if err != nil || jid.IsEmpty() {
				continue
			}
			sendTextMsg(client, jid, text)
		}
	})
	client.AddEventHandler(func(evt interface{}) {
		handleEvent(client, evt)
	})

	// Connection
	if client.Store.ID == nil {
		fmt.Println("📱 Belum ada sesi. Menunggu QR code...")
		fmt.Println("   Scan QR ini lewat aplikasi WhatsApp: Perangkat Tertaut → Tautkan Perangkat")
	} else {
		fmt.Printf("🔑 Sesi ditemukan untuk %s. Mencoba login ulang...\n", client.Store.ID)
	}

	err = client.Connect()
	if err != nil {
		fmt.Printf("❌ Gagal connect: %v\n", err)
		os.Exit(1)
	}
	go connectionSupervisor(client)

	// ⚡ GRACEFUL SHUTDOWN: context-based (replaces blocking channel)
	initShutdownContext()
	waitForShutdown()
	client.Disconnect()
}

// connectionSupervisor: pengawas koneksi WA. Auto-reconnect bawaan
// hypermeow punya backoff yang tumbuh tak terbatas (jumlah error × 2s),
// jadi setelah outage panjang jeda reconnect bisa sangat lama. Bila
// koneksi mati >~90 detik berturut-turut padahal sesi masih login,
// paksa siklus reconnect baru via ResetConnection (backoff kereset).
func connectionSupervisor(client *whatsmeow.Client) {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	downStreak := 0
	for range ticker.C {
		if client.IsConnected() {
			downStreak = 0
			continue
		}
		if !client.IsLoggedIn() {
			// Belum login / logged out — butuh QR, bukan reconnect.
			downStreak = 0
			continue
		}
		downStreak++
		if downStreak >= 2 {
			fmt.Printf("🔌 [SUPERVISOR] koneksi mati %dx cek berturut-turut — paksa reconnect\n", downStreak)
			// ResetConnection membunuh socket zombie; Connect memulai
			// koneksi baru seketika tanpa menunggu backoff auto-reconnect.
			client.ResetConnection()
			if err := client.Connect(); err != nil {
				fmt.Printf("🔌 [SUPERVISOR] Connect: %v\n", err)
			}
			downStreak = 0
		}
	}
}

// ============================================================
// EVENT HANDLER
// ============================================================

func handleEvent(client *whatsmeow.Client, evt interface{}) {
	switch v := evt.(type) {
	case *events.QR:
		if len(v.Codes) > 0 {
			setWebQR(v.Codes[0])
			fmt.Println("\n📷 QR Code (scan dengan WhatsApp):")
			fmt.Println("   🌐 Atau buka Web UI di browser (lihat URL di atas)")
			qrterminal.GenerateHalfBlock(v.Codes[0], qrterminal.L, os.Stdout)
			fmt.Println("   (QR akan otomatis diperbarui jika kedaluwarsa)")
		}
	case *events.PairSuccess:
		fmt.Printf("✅ Pairing berhasil! JID: %s\n", v.ID)
	case *events.Connected:
		jid := ""
		if client.Store != nil && client.Store.ID != nil {
			jid = client.Store.ID.String()
			selfModeMu.Lock()
			selfModeState.BotJID = jid
			selfModeMu.Unlock()
			saveSelfMode()
		}
		installSPRHooks(client)
		setWebConnected(jid)
		fmt.Println("✅ Terhubung! Coba kirim: .menu")
		// ⚡ PRE-COMPUTE: warm cache in background
		precomputeWarmup(client)
	case *events.Disconnected:
		fmt.Printf("🔌 [DISCONNECT] koneksi WA terputus (%s), auto-reconnect berjalan\n", time.Now().Format("15:04:05"))
	case *events.LoggedOut:
		setWebLoggedOut()
		fmt.Println("⚠️  Logged out. Hapus sessions.db untuk scan ulang.")
	case *events.Message:
		handleMessage(client, v)
	case *events.UndecryptableMessage:
		// Tracker pasif grup NIXCODE: catat pesan yang tidak bisa
		// didekripsi device secondary ini (pola fanout SPR).
		trackUndecryptable(v)
		// Anti-SPR: someone may be sending SPR targeting other devices.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("⚠️ panic: %v\n", r)
				}
			}()
			antiSPRCheckUndecryptable(client, v)
		}()
	case *events.GroupInfo:
		handleGroupInfo(client, nil, v)
	}
}

// ============================================================
// HANDLER PESAN & COMMAND
// ============================================================

func handleMessage(client *whatsmeow.Client, msg *events.Message) {
	// Hardening: typed-nil event tidak boleh bikin panic
	if msg == nil {
		return
	}
	// ❌ reaction → trigger .dmsgva (owner only, grup only).
	// Dipasang SEBELUM filter IsFromMe: owner bisa react dari akun
	// bot sendiri (companion phone) dengan IsFromMe=true.
	if handleDMsgVAReaction(client, msg) {
		return
	}
	// 🤖 feedback AI dari owner → catat, jangan diproses lanjut.
	if handleAIFeedbackReaction(client, msg) {
		return
	}
	// Filter: skip self, status, newsletter
	if msg.Info.IsFromMe {
		return
	}
	if msg.Info.Chat.Server == types.NewsletterServer {
		return
	}
	if msg.Info.Chat == types.StatusBroadcastJID {
		return
	}

	// Always learn PN↔LID (even for media-only / no-text) so owner/trust stay accurate
	BgPool.Submit(func() { learnMappingFromMessage(client, msg) })
	// Cache raw message for .crm <id> and auto-save status for .gsw
	BgPool.Submit(func() { crmRememberIncoming(msg) })
	// ─── Background processing (1 goroutine instead of 5) ─────────
	// Combined to reduce goroutine spawning overhead (was 5 separate go func()
	// per message = 5x scheduler contention on GOMAXPROCS=2). Now 1 goroutine
	// runs all background tasks sequentially with per-task panic recovery.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("⚠️ panic: %v\n", r)
			}
		}()
		safe := func(fn func()) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("⚠️ task panic: %v\n", r)
				}
			}()
			fn()
		}
		safe(func() { archiveIncomingMessage(msg) })
		safe(func() { qCacheStore(msg) })
		safe(func() { fioraCRMAutoSave(client, msg) })
		safe(func() { antiSPRCheckMessage(client, msg) })
		safe(func() { checkAntibot(client, msg) })
		safe(func() { checkAntifdmsg(client, msg) })
		safe(func() { recordChatForBroadcast(msg.Info.Chat.String()) })
		safe(func() { maybeAutoRead(client, msg) })
		safe(func() { tagForwardMaybeForward(client, msg) })
	}()

	text := extractText(msg)
	if text == "" {
		// ─── Pesan tanpa teks ─────────────────────────────────────
		// Foto, video atau audio bisa jadi bahan template JAM.
		if msg.Info.IsGroup && !groupAllowsSender(msg) {
			return
		}
		jjamHandleMedia(client, msg, "")
		return
	}

	// Log incoming (ASYNC — don't block command dispatch for stdout write)
	go func() {
		pushName := msg.Info.PushName
		if pushName == "" {
			pushName = "kak"
		}
		chatType := "Personal"
		if msg.Info.IsGroup {
			chatType = "Grup"
		}
		cleanText := strings.ReplaceAll(text, "\n", " ")
		fmt.Printf("[%s] %s (%s) [%s]: %s\n",
			time.Now().In(time.FixedZone("WIB", 7*3600)).Format("15:04:05"),
			pushName, msg.Info.Sender, chatType, cleanText)
	}()

	// (recordChatForBroadcast moved to combined goroutine above)

	// (learnMappingFromMessage already submitted to BgPool above — duplicate removed)

	// ─── HARD SILENT GATE for locked groups ─────────────────────
	if msg.Info.IsGroup && !groupAllowsSender(msg) {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("⚠️ panic: %v\n", r)
				}
			}()
			_ = applyGuardFilters(client, nil, msg, text)
		}()
		return
	}

	// (maybeAutoRead moved to combined goroutine above)

	// Guard filters (antilink / antispam) — skip for owner (trusted, faster .ping/.menu)
	if !isOwnerMsg(msg) {
		if applyGuardFilters(client, nil, msg, text) {
			return
		}
	}

	// ─── Media untuk sesi JAM ─────────────────────────────────────
	// Foto, video atau audio yang dikirim user (dengan atau tanpa
	// caption) mengisi slot template JAM. Pesan yang merupakan
	// perintah bukan media, jadi dilewati.
	if !strings.HasPrefix(text, ".") && !strings.HasPrefix(text, "!") && !strings.HasPrefix(text, "$") {
		if jjamHandleMedia(client, msg, text) {
			return
		}
	}

	// 🔒 SILENT GATE for "ambient" output: AFK auto-respond + XP announce
	// TIDAK boleh muncul di grup terkunci. Hanya owner commands (di bawah)
	// yang boleh balas di grup terkunci. Jadi cek isGroupPublic dulu.
	isPublic := !msg.Info.IsGroup || isGroupPublic(msg.Info.Chat.String())

	// AFK: auto-off + respond to mentions — skip for command messages (.ping/.menu dll)
	// Command messages don't mention AFK users, skip to save latency
	if isPublic && !strings.HasPrefix(text, ".") && !strings.HasPrefix(text, "$") && !strings.HasPrefix(text, "!") {
		checkAFKMentions(client, nil, msg, text)
	}

	// ─── Auto-trigger AI on reply to ANY bot message ─────────
	// User reply pesan bot (menu, ping, AI response, dll) → AI auto-respond
	// Dengan personality Ruby Hoshino, jadi terasa natural.
	//
	// ─── No-prefix AI trigger for owners ───────────────────
	// Any owner can trigger AI with: "ayang ...", "ruby ...", etc.
	// Perintah dengan tanda seru ("!jjam") bukan AI trigger, jadi
	// tidak masuk blok ini.
	if !strings.HasPrefix(text, ".") && !strings.HasPrefix(text, "$") && !strings.HasPrefix(text, "!") {
		// Check no-prefix AI trigger for owners
		if isOwnerMsg(msg) && isAutoAIEnabled(msg.Info.Chat.String()) {
			lowerText := strings.ToLower(strings.TrimSpace(text))

			// Trigger prefixes (longest first for correct strip)
			triggers := []string{
				"ayang kamu ruby ",
				"ayang ruby ",
				"ayang ",
				"ruby ",
			}

			var aiText string
			matched := false
			for _, trigger := range triggers {
				if strings.HasPrefix(lowerText, trigger) {
					aiText = strings.TrimSpace(text[len(trigger):])
					matched = true
					break
				}
			}

			if matched && aiText != "" {
				fmt.Printf("[TRIGGER-DEBUG] ayang/ruby matched: trigger ok, aiText=%q sender=%s\n", aiText, msg.Info.Sender.String())
				go func() {
					defer func() {
						if r := recover(); r != nil {
							fmt.Printf("⚠️ AI no-prefix panic: %v\n", r)
						}
					}()
					reply := processCommand(client, ".ai", aiText, msg)
					fmt.Printf("[TRIGGER-DEBUG] .ai reply len=%d\n", len(reply))
					if reply != "" {
						aiMsgID := fmt.Sprintf("aiagent-%d", time.Now().UnixMilli())
						respID, err := humanSendReplyWithID(client, msg, reply, aiMsgID)
						if err == nil && respID != "" {
							markAIResponse(respID)
							rememberAISent(respID, aiText, reply, msg.Info.Chat.String())
						}
					}
				}()
				return
			}
		}

		// ─── "ruby" name trigger untuk NON-OWNER ──────────────────
		// Semua orang (selain owner) bisa panggil "ruby ..." → AI respon.
		// Owner sudah di-handle di blok atas (dengan trigger ayang/ruby).
		if !isOwnerMsg(msg) && isAutoAIEnabled(msg.Info.Chat.String()) {
			lowerText := strings.ToLower(strings.TrimSpace(text))
			if strings.HasPrefix(lowerText, "ruby ") {
				aiText := strings.TrimSpace(text[5:])
				if aiText != "" {
					go func() {
						defer func() {
							if r := recover(); r != nil {
								fmt.Printf("⚠️ AI ruby-trigger panic: %v\n", r)
							}
						}()
						reply := processCommand(client, ".ai", aiText, msg)
						if reply != "" {
							aiMsgID := fmt.Sprintf("aiagent-%d", time.Now().UnixMilli())
							respID, err := humanSendReplyWithID(client, msg, reply, aiMsgID)
							if err == nil && respID != "" {
								markAIResponse(respID)
								rememberAISent(respID, aiText, reply, msg.Info.Chat.String())
							}
						}
					}()
					return
				}
			}
		}

		if quoted := getQuotedMessage(msg); quoted != nil {
			// Check if quoted is from bot.
			// getQuotedMessage now sets IsFromMe via dual-form comparison.
			// Also check aiagent- prefix on quoted ID (custom AI reply IDs).
			isFromBot := quoted.Info.IsFromMe
			if !isFromBot && strings.HasPrefix(quoted.Info.ID, "aiagent-") {
				isFromBot = true
			}
			// Additional fallback: check participant against bot's own JID
			// (covers edge cases where getQuotedMessage couldn't resolve dual forms)
			if !isFromBot && botClient != nil && botClient.Store != nil {
				if ownJID := botClient.Store.ID; ownJID != nil {
					ext := msg.Message.GetExtendedTextMessage()
					if ext != nil && ext.ContextInfo != nil {
						if participant := ext.ContextInfo.GetParticipant(); participant != "" {
							if jid, err := types.ParseJID(participant); err == nil {
								// Check direct match
								if jid.String() == ownJID.String() || jid.User == ownJID.User {
									isFromBot = true
								} else {
									// Check dual forms (LID ↔ PN)
									for _, form := range dualForms(botClient, jid) {
										if form.String() == ownJID.String() || form.User == ownJID.User {
											isFromBot = true
											break
										}
									}
								}
							}
						}
					}
				}
			}
			if isFromBot && !msg.Info.IsFromMe && isAutoAIEnabled(msg.Info.Chat.String()) {
				// AI auto-respond dengan personality Ruby Hoshino
				if aiReply := aiTryAutoTrigger(client, msg, text); aiReply != "" {
					go func() {
						defer func() {
							if r := recover(); r != nil {
								fmt.Printf("⚠️ AI auto-trigger panic: %v\n", r)
							}
						}()
						aiMsgID := fmt.Sprintf("aiagent-%d", time.Now().UnixMilli())
						respID, err := humanSendReplyWithID(client, msg, aiReply, aiMsgID)
						if err == nil && respID != "" {
							markAIResponse(respID)
							rememberAISent(respID, text, aiReply, msg.Info.Chat.String())
						} else if err != nil {
							fmt.Printf("⚠️ AI auto-trigger send error: %v\n", err)
						}
					}()
				}
			}
		}
		return
	}

	// Per-user rate limit (anti-abuse). Owner bypasses.
	if ok, deny := rlCheckGlobal(msg); !ok {
		sendQuotedReply(client, msg, deny)
		return
	}

	// Auto-typing presence while processing
	maybeAutoTyping(client, msg.Info.Chat)

	cmd, args := parseCommand(text)

	// Process command in goroutine with panic recovery
	// ⚡ OPTIMIZED: single goroutine (removed double goroutine + channel + select + timer)
	// Saves ~10-50µs per command (goroutine creation + channel + select overhead)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("⚠️ Command panic (%s): %v\n", cmd, r)
			}
		}()

		// Process command directly (no inner goroutine + channel)
		reply := processCommand(client, cmd, args, msg)
		if reply != "" {
			// Send reply via pool (non-blocking, bounded — ala tokio)
			// Pool sudah handle panic recovery, tidak perlu defer di sini.
			if !MsgPool.Submit(func() {
				// AI replies get custom message ID so user replies can auto-trigger
				var respID string
				var err error
				if cmd == ".ai" {
					aiMsgID := fmt.Sprintf("aiagent-%d", time.Now().UnixMilli())
					respID, err = humanSendReplyWithID(client, msg, reply, aiMsgID)
					if err == nil && respID != "" {
						rememberAISent(respID, args, reply, msg.Info.Chat.String())
					}
				} else {
					respID, err = humanSendReply(client, msg, reply)
				}
				if err != nil {
					fmt.Printf("⚠️ Gagal kirim balasan: %v\n", err)
				}
				// Track AI responses (for auto-trigger on reply)
				if err == nil && cmd == ".ai" {
					markAIResponse(respID)
				}
			}) {
				fmt.Printf("⚠️ MsgPool penuh, reply di-drop: %s\n", cmd)
			}
		}
	}()
}

// recordChatForBroadcast stores the chat JID for later broadcast use.
func recordChatForBroadcast(jid string) {
	if globalKV == nil {
		return
	}
	// max 500 chats
	chats := []string{}
	if v := globalKV.Get("recent_chats"); v != nil {
		if arr, ok := v.([]any); ok {
			for _, c := range arr {
				if s, ok := c.(string); ok {
					chats = append(chats, s)
				}
			}
		}
	}
	// dedup + add new
	seen := map[string]bool{}
	var fresh []string
	fresh = append(fresh, jid)
	seen[jid] = true
	for _, c := range chats {
		if !seen[c] {
			fresh = append(fresh, c)
			seen[c] = true
		}
		if len(fresh) >= 500 {
			break
		}
	}
	anyFresh := make([]any, len(fresh))
	for i, c := range fresh {
		anyFresh[i] = c
	}
	globalKV.Set("recent_chats", anyFresh)
}

func parseCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	// $ alias for .shell — handle BEFORE SplitN so both "$ ls" and "$ls" work
	if strings.HasPrefix(text, "$") {
		rest := strings.TrimSpace(strings.TrimPrefix(text, "$"))
		return ".shell", rest
	}
	parts := strings.SplitN(text, " ", 2)
	cmd := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	// Support ".spr@628xxx teks" / ".trust@tag" (no space before @)
	if i := strings.Index(cmd, "@"); i > 1 {
		// keep leading "." command only
		rest := cmd[i:] // @...
		cmd = cmd[:i]
		if args != "" {
			args = rest + " " + args
		} else {
			args = rest
		}
	}
	return cmd, args
}

// processCommand dispatches the command. Returns reply text ("" = no reply).
func processCommand(client *whatsmeow.Client, cmd, args string, msg *events.Message) (result string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("⚠️ processCommand panic (%s): %v\n", cmd, r)
			result = ""
		}
	}()

	pushName := msg.Info.PushName
	if pushName == "" {
		pushName = "kak"
	}

	// Heavy command rate-limit (AI / DL / maker). Owner bypasses.
	if isHeavyCmd(cmd) {
		if ok, deny := rlCheckHeavy(msg); !ok {
			return deny
		}
	}

	// Sistem plugin self-registering (PreGate): command yang harus jalan
	// sebelum gate check. Lihat core/plugin.go.
	if reply, ok := runPreGatePlugin(client, msg, cmd, args); ok {
		return reply
	}

	// Apply self mode / per-group / trust gate
	// Locked groups: non-owner non-trusted get empty deny → bot 100% silent
	if ok, deny := gateCommand(msg); !ok {
		return deny
	}

	// Sistem plugin self-registering — semua command terdaftar via init().
	// Tambah plugin baru: cukup buat file .go di plugins/. Lihat core/plugin.go.
	if reply, ok := runPlugin(client, msg, cmd, args); ok {
		return reply
	}
	// 100% silent for unknown commands.
	return ""
}

// ============================================================
// UTIL PENGIRIMAN BALASAN
// ============================================================

func sendQuotedReply(client *whatsmeow.Client, msg *events.Message, text string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("⚠️ sendQuotedReply panic: %v\n", r)
			}
		}()
		if _, err := humanSendReply(client, msg, text); err != nil {
			fmt.Printf("⚠️  Gagal kirim balasan: %v\n", err)
		}
	}()
}

// ============================================================
// UTIL TEKS
// ============================================================

func extractText(msg *events.Message) string {
	if msg.Message == nil {
		return ""
	}
	if t := msg.Message.GetConversation(); t != "" {
		return t
	}
	if ext := msg.Message.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	if img := msg.Message.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.Message.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.Message.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}
	return ""
}

// getenvDefault reads an env var with fallback.
func getenvDefaultMain(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
