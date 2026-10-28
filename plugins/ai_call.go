package main

// ai_call.go — Real-time AI conversation via WhatsApp call
// ════════════════════════════════════════════════════════════
//
// FEATURE:
//   User telepon bot → auto-answer → AI conversation 2-arah via voice
//
// FLOW:
//   1. Incoming call → meowcaller.OnIncomingCall
//   2. If AICall mode ON → call.Answer() + start conversation loop
//   3. call.Receive(sink) — terima PCM frames dari caller
//   4. VAD detect silence (user selesai bicara)
//   5. Buffer PCM → WAV → Groq Whisper STT → text
//   6. Text → kangwifi /ai/gemini → response text
//   7. Response → Google Translate TTS → MP3
//   8. call.Play(source) — putar ke caller
//   9. Loop (with interruption detection)
//
// COMMANDS:
//   .aicall on|off          — toggle AI conversation mode
//   .aicall status          — check status
//   .aicall setkey <key>    — set Groq API key
//   .aicall system <prompt> — set system prompt for AI
//   .aicall clear           — clear all conversation memory
//   .aicall help            — show help
//
// REQUIREMENTS:
//   - GROQ_API_KEY (free at https://console.groq.com/keys)
//     Set via: .aicall setkey gsk_xxxxx
//     Or via .env: GROQ_API_KEY=gsk_xxxxx
//   - kangwifi API (already used by bot)
//   - Google Translate TTS (free, no key needed)
//
// LATENCY (per turn):
//   - VAD silence detection: ~1000ms
//   - STT (Groq Whisper): ~500-1500ms
//   - AI (kangwifi gemini): ~1500-3000ms
//   - TTS (Google Translate): ~500-1000ms
//   - Playback start: ~200ms
//   TOTAL: ~3-6 seconds per turn (acceptable for voice conversation)

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
	meowcaller "github.com/purpshell/meowcaller"
)

func init() {
	RegisterPlugin(Plugin{
		Command:     "aicall",
		Description: "AI conversation via telepon (auto-answer)",
		Category:    "Call",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAICallCommand(client, msg, args)
		},
	})
}

// ─── Configuration ────────────────────────────────────────────────

const (
	aicallSilenceDuration  = 1000 // ms of silence to detect end-of-utterance
	aicallSilenceThreshold = 0.01 // RMS threshold below which is "silence"
	aicallMaxTurns         = 10   // conversation memory per caller
	aicallMaxRecordingSec  = 30   // max single utterance length
	aicallSampleRate       = 16000
	aicallFrameSamples     = 960 // 60ms at 16kHz (meowcaller native frame)
)

type AICallConfig struct {
	Enabled      bool   `json:"enabled"`
	GroqAPIKey   string `json:"groq_api_key"`
	SystemPrompt string `json:"system_prompt"`
}

var (
	aicallCfgMu sync.RWMutex
	aicallCfg   = AICallConfig{
		Enabled:      false,
		SystemPrompt: "Kamu adalah asisten AI yang ramah dan membantu. Jawab dengan singkat dan jelas dalam Bahasa Indonesia. Jawaban maksimal 2-3 kalimat agar cepat diucapkan.",
	}
	aicallCfgLoaded bool

	// Per-caller conversation memory
	aicallMemMu  sync.Mutex
	aicallMemory = map[string][]ChatTurn{} // callerJID → conversation

	// Active AI calls (callID → *aiCallSession)
	aicallActiveMu sync.Mutex
	aicallActive   = map[string]*aiCallSession{}
)

// ChatTurn dipindah ke plugins/ai_agentic.go (jangan duplikat).

type aiCallSession struct {
	callID    string
	callerJID string
	cancel    context.CancelFunc
}

// aicall global config is stored in kv_store (key="aicall").
// Single global config — kv_store is a perfect fit. The ai_call_config
// table (per-group) is reserved for future per-group AI call overrides.
const aicallKVKey = "aicall"

func loadAICallConfig() {
	aicallCfgMu.Lock()
	defer aicallCfgMu.Unlock()
	if aicallCfgLoaded {
		return
	}
	aicallCfgLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !kvStoreGet(ctx, aicallKVKey, &aicallCfg) {
		// No row — use defaults (already set in struct init).
		// Persist defaults so future loads are consistent.
		_ = kvStoreSet(ctx, aicallKVKey, &aicallCfg)
	}
	// Try to load GROQ_API_KEY from env if not in config
	if aicallCfg.GroqAPIKey == "" {
		if k := os.Getenv("GROQ_API_KEY"); k != "" {
			aicallCfg.GroqAPIKey = k
		}
	}
}

func saveAICallConfig() {
	aicallCfgMu.RLock()
	// Copy under RLock to release fast
	snapshot := aicallCfg
	aicallCfgMu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = kvStoreSet(ctx, aicallKVKey, &snapshot)
}

// ─── Command handler: .aicall ─────────────────────────────────────

func handleAICallCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	loadAICallConfig()

	parts := strings.Fields(strings.TrimSpace(args))
	if len(parts) == 0 {
		return aicallStatus()
	}

	switch strings.ToLower(parts[0]) {
	case "on":
		aicallCfgMu.Lock()
		aicallCfg.Enabled = true
		aicallCfgMu.Unlock()
		saveAICallConfig()
		return "✅ *AI Call Mode ON*\n\nBot sekarang auto-answer incoming calls + AI conversation.\n\nPastikan:\n• Groq API key sudah di-set (.aicall setkey)\n• Bot sudah connect ke WhatsApp"

	case "off":
		aicallCfgMu.Lock()
		aicallCfg.Enabled = false
		aicallCfgMu.Unlock()
		saveAICallConfig()
		// Hangup all active AI calls
		aicallActiveMu.Lock()
		for _, session := range aicallActive {
			if session.cancel != nil {
				session.cancel()
			}
		}
		aicallActive = map[string]*aiCallSession{}
		aicallActiveMu.Unlock()
		return "⚠️ *AI Call Mode OFF*\n\nBot kembali auto-reject incoming calls."

	case "status":
		return aicallStatus()

	case "setkey", "key":
		if len(parts) < 2 {
			return "❓ .aicall setkey gsk_xxxxx\n\nGet free key: https://console.groq.com/keys"
		}
		key := strings.TrimSpace(parts[1])
		if key == "" {
			return "❌ API key tidak boleh kosong."
		}
		aicallCfgMu.Lock()
		aicallCfg.GroqAPIKey = key
		aicallCfgMu.Unlock()
		saveAICallConfig()
		return fmt.Sprintf("✅ Groq API key disimpan: %s", maskAPIKey(key))

	case "system", "prompt":
		if len(parts) < 2 {
			return "❓ .aicall system <prompt text>\n\nContoh: .aicall system Kamu asisten ramah, jawab singkat"
		}
		prompt := strings.TrimSpace(strings.Join(parts[1:], " "))
		aicallCfgMu.Lock()
		aicallCfg.SystemPrompt = prompt
		aicallCfgMu.Unlock()
		saveAICallConfig()
		return "✅ System prompt diupdate:\n\n" + prompt

	case "clear":
		aicallMemMu.Lock()
		count := len(aicallMemory)
		aicallMemory = map[string][]ChatTurn{}
		aicallMemMu.Unlock()
		return fmt.Sprintf("✅ Memory %d callers dibersihkan", count)

	case "help":
		return aicallHelp()

	default:
		return aicallHelp()
	}
}

// maskAPIKey menyamarkan API key untuk ditampilkan: aman untuk key pendek sekalipun.
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func aicallStatus() string {
	aicallCfgMu.RLock()
	defer aicallCfgMu.RUnlock()

	status := "OFF ⚠️"
	if aicallCfg.Enabled {
		status = "ON ✅"
	}

	keyStatus := "❌ Tidak ada"
	if aicallCfg.GroqAPIKey != "" {
		keyStatus = "✅ " + maskAPIKey(aicallCfg.GroqAPIKey)
	}

	aicallActiveMu.Lock()
	activeCount := len(aicallActive)
	aicallActiveMu.Unlock()

	aicallMemMu.Lock()
	memCount := len(aicallMemory)
	aicallMemMu.Unlock()

	return fmt.Sprintf(`🤖 *AI Call Status*

• Mode:           %s
• Groq API key:   %s
• Active calls:   %d
• Memory (callers): %d

System prompt:
%s

Commands:
  .aicall on|off          — toggle mode
  .aicall status          — lihat status ini
  .aicall setkey <key>    — set Groq API key
  .aicall system <prompt> — set AI personality
  .aicall clear           — hapus semua memory
  .aicall help            — bantuan

Get free Groq key: https://console.groq.com/keys`, status, keyStatus, activeCount, memCount, aicallCfg.SystemPrompt)
}

func aicallHelp() string {
	return `🤖 *AI Call — Real-time Voice Conversation*

Bot auto-answer incoming calls + AI conversation 2-arah via voice.

SETUP:
1. Get free Groq API key: https://console.groq.com/keys
2. .aicall setkey gsk_xxxxx
3. .aicall on
4. Telepon nomor bot dari WhatsApp

CARA PAKAI:
• Telepon bot → bot auto-answer
• Bicara → AI dengar → jawab dengan voice
• Bicara lagi → AI jawab lagi (ingat context)
• Hangup untuk selesai

COMMANDS:
  .aicall on|off          — toggle mode
  .aicall status          — lihat status
  .aicall setkey <key>    — set Groq API key
  .aicall system <prompt> — set AI personality
  .aicall clear           — hapus memory
  .aicall help            — bantuan

LATENCY: 3-6 detik per turn (VAD + STT + AI + TTS)
STT: Groq Whisper (free, akurat)
AI: kangwifi gemini (free)
TTS: Google Translate (free)`
}

// ─── Incoming call hook (called from initMeowcallerClient) ────────

// aicallOnIncomingCall is called when an incoming call arrives.
// If AI Call mode is ON, auto-answer and start conversation.
// Otherwise, fall through to default reject behavior.
//
// Returns true if handled (AI mode active), false if should reject.
func aicallOnIncomingCall(call *meowcaller.Call) bool {
	loadAICallConfig()
	aicallCfgMu.RLock()
	enabled := aicallCfg.Enabled
	hasKey := aicallCfg.GroqAPIKey != ""
	aicallCfgMu.RUnlock()

	if !enabled {
		return false
	}
	if !hasKey {
		fmt.Println("[AICALL] WARNING: mode ON but no Groq API key — rejecting call")
		return false
	}
	// Guard UDP: media call butuh UDP. Kalau diblokir, jangan jawab —
	// yang ada cuma call nyangkut tanpa suara.
	if !callCheckUDP() {
		fmt.Println("[AICALL] WARNING: UDP diblokir — rejecting incoming call (media tidak akan jalan)")
		return false
	}
	if call == nil {
		return false
	}

	// Get caller JID
	var callerJID string
	if p := call.Peer(); !p.IsEmpty() {
		callerJID = p.String()
	} else {
		callerJID = "unknown"
	}
	callID := call.ID()
	if callID == "" {
		callID = fmt.Sprintf("aicall-%d", time.Now().UnixNano())
	}

	fmt.Printf("[AICALL] Incoming call from %s (callID=%s) — auto-answering\n", callerJID, callID)

	// Answer the call
	if err := call.Answer(); err != nil {
		fmt.Printf("[AICALL] Answer failed: %v\n", err)
		return false
	}

	// Start conversation session
	ctx, cancel := context.WithCancel(context.Background())
	session := &aiCallSession{
		callID:    callID,
		callerJID: callerJID,
		cancel:    cancel,
	}

	aicallActiveMu.Lock()
	aicallActive[callID] = session
	aicallActiveMu.Unlock()

	// Cleanup on call end
	call.OnEnd(func(reason string) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[AICALL] PANIC in OnEnd: %v\n", r)
			}
		}()
		fmt.Printf("[AICALL] Call %s ended: %s\n", callID, reason)
		cancel()
		aicallActiveMu.Lock()
		delete(aicallActive, callID)
		aicallActiveMu.Unlock()
	})

	// Start conversation loop in background
	go aicallConversationLoop(ctx, call, session)

	return true
}

// ─── Conversation loop ────────────────────────────────────────────

func aicallConversationLoop(ctx context.Context, call *meowcaller.Call, session *aiCallSession) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[AICALL] PANIC in conversation loop: %v\n", r)
		}
	}()

	// Wait for media ready
	readyCh := make(chan struct{}, 1)
	call.OnReady(func() {
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	select {
	case <-readyCh:
		fmt.Printf("[AICALL] Media ready for call %s — starting conversation\n", session.callID)
	case <-time.After(5 * time.Second):
		// Fallback: OnReady might not fire reliably.
		// Proceed anyway — setup receiver + greeting.
		fmt.Printf("[AICALL] OnReady timeout (5s) — proceeding anyway for call %s\n", session.callID)
	case <-ctx.Done():
		return
	}

	// CRITICAL: Set up PERMANENT audio receiver ONCE.
	audioCh := make(chan []float32, 128)
	sink := meowcaller.SinkFunc(func(frame []float32) {
		select {
		case audioCh <- frame:
		default:
		}
	})
	call.Receive(sink)
	fmt.Printf("[AICALL] Audio sink installed for call %s\n", session.callID)

	// Small delay to let media stabilize
	time.Sleep(500 * time.Millisecond)

	// Send greeting — Ruby Hoshino style
	greeting := "Halo~! Aku Ruby Hoshino! ⭐ Ada yang bisa aku bantu?"
	aicallSpeak(ctx, call, greeting, session, audioCh)

	// Main conversation loop
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Listen for user utterance (reads from permanent audioCh)
		userText, err := aicallListenUtterance(ctx, audioCh)
		if err != nil {
			if ctx.Err() != nil {
				return // call ended
			}
			fmt.Printf("[AICALL] Listen error: %v\n", err)
			continue
		}
		if userText == "" {
			continue // silence timeout, try again
		}

		fmt.Printf("[AICALL] User said: %q\n", userText)

		// Check for exit commands
		lower := strings.ToLower(userText)
		if strings.Contains(lower, "bye") || strings.Contains(lower, "dadah") ||
			strings.Contains(lower, "sampai jumpa") || strings.Contains(lower, "stop") ||
			strings.Contains(lower, "tutup") {
			aicallSpeak(ctx, call, "Baik, sampai jumpa!", session, audioCh)
			_ = call.Hangup()
			return
		}

		// Get AI response
		aiResponse, err := aicallGetAIResponse(userText, session.callerJID)
		if err != nil {
			fmt.Printf("[AICALL] AI error: %v\n", err)
			aicallSpeak(ctx, call, "Maaf, saya tidak bisa memproses itu sekarang.", session, audioCh)
			continue
		}

		fmt.Printf("[AICALL] AI response: %q\n", aiResponse)

		// Speak the response
		aicallSpeak(ctx, call, aiResponse, session, audioCh)
	}
}

// ─── Listen for one utterance (VAD + STT) ─────────────────────────
// Reads from the permanent audioCh — does NOT touch call.Receive().

func aicallListenUtterance(ctx context.Context, audioCh chan []float32) (string, error) {
	var pcmBuffer []float32
	var silenceStart time.Time
	var speaking bool
	// Drain any buffered audio first (from previous TTS playback)
	// to avoid picking up echo of our own voice
	drainTimer := time.After(500 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-drainTimer:
			break drainLoop
		case <-audioCh:
			// discard
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	timeout := time.After(aicallMaxRecordingSec * time.Second)
	idleTimeout := time.After(30 * time.Second) // if no speech for 30s, return empty

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()

		case <-timeout:
			if len(pcmBuffer) > 0 {
				return aicallSTT(pcmBuffer)
			}
			return "", nil

		case <-idleTimeout:
			// No speech detected in 30s — return empty (caller will loop)
			if len(pcmBuffer) > 0 {
				return aicallSTT(pcmBuffer)
			}
			return "", nil

		case frame := <-audioCh:
			// Compute RMS (volume)
			rms := aicallComputeRMS(frame)

			if rms > aicallSilenceThreshold {
				// User is speaking
				speaking = true
				pcmBuffer = append(pcmBuffer, frame...)
				silenceStart = time.Time{} // reset silence timer
			} else if speaking {
				// User was speaking, now silence
				if silenceStart.IsZero() {
					silenceStart = time.Now()
				}
				// Still record during silence (in case it's a brief pause)
				pcmBuffer = append(pcmBuffer, frame...)

				// Check if silence duration exceeded threshold
				if time.Since(silenceStart) > time.Duration(aicallSilenceDuration)*time.Millisecond {
					// End of utterance
					if len(pcmBuffer) >= aicallSampleRate { // at least 1 second
						return aicallSTT(pcmBuffer)
					}
					// Too short, reset
					pcmBuffer = nil
					speaking = false
				}
			}
		}
	}
}

func aicallComputeRMS(frame []float32) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

// ─── STT: Groq Whisper ────────────────────────────────────────────

func aicallSTT(pcm []float32) (string, error) {
	aicallCfgMu.RLock()
	apiKey := aicallCfg.GroqAPIKey
	aicallCfgMu.RUnlock()

	if apiKey == "" {
		return "", fmt.Errorf("no Groq API key")
	}

	// Convert float32 PCM → WAV bytes
	wavBytes, err := aicallPCMToWAV(pcm)
	if err != nil {
		return "", fmt.Errorf("PCM→WAV: %w", err)
	}

	// Build multipart form
	var body bytes.Buffer
	writer := multipartWriter(&body)
	_ = writer.WriteField("model", "whisper-large-v3")
	_ = writer.WriteField("language", "id")
	_ = writer.WriteField("response_format", "json")
	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", err
	}
	part.Write(wavBytes)
	writer.Close()

	// POST to Groq
	if base := groqAPIBaseURL(); base == "" {
		return "", &envMissingError{Name: "GROQ_API_BASE"}
	}
	req, err := http.NewRequest("POST", groqAPIBaseURL()+"/openai/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("STT request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("STT HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return strings.TrimSpace(result.Text), nil
}

// aicallPCMToWAV converts float32 PCM samples (16kHz mono) to WAV bytes.
func aicallPCMToWAV(pcm []float32) ([]byte, error) {
	// Convert float32 → int16 PCM
	samples := make([]int16, len(pcm))
	for i, f := range pcm {
		s := int(f * 32767)
		if s > 32767 {
			s = 32767
		}
		if s < -32768 {
			s = -32768
		}
		samples[i] = int16(s)
	}

	// Build WAV
	var buf bytes.Buffer
	// RIFF header
	buf.WriteString("RIFF")
	dataLen := len(samples) * 2
	buf.Write(aicallUint32LE(uint32(dataLen + 36)))
	buf.WriteString("WAVE")
	// fmt chunk
	buf.WriteString("fmt ")
	buf.Write(aicallUint32LE(16)) // chunk size
	buf.Write(aicallUint16LE(1))  // PCM format
	buf.Write(aicallUint16LE(1))  // mono
	buf.Write(aicallUint32LE(aicallSampleRate))
	buf.Write(aicallUint32LE(aicallSampleRate * 2)) // byte rate
	buf.Write(aicallUint16LE(2))                    // block align
	buf.Write(aicallUint16LE(16))                   // bits per sample
	// data chunk
	buf.WriteString("data")
	buf.Write(aicallUint32LE(uint32(dataLen)))

	// Write samples
	err := binary.Write(&buf, binary.LittleEndian, samples)
	return buf.Bytes(), err
}

func aicallUint32LE(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

func aicallUint16LE(v uint16) []byte {
	return []byte{byte(v), byte(v >> 8)}
}

// multipartWriter is a helper to create multipart form (avoids importing mime/multipart twice)
type simpleWriter struct {
	*bytes.Buffer
	boundary string
}

func multipartWriter(buf *bytes.Buffer) *simpleWriter {
	w := &simpleWriter{Buffer: buf, boundary: "----aicallboundary"}
	return w
}

func (w *simpleWriter) WriteField(name, value string) error {
	w.WriteString("--" + w.boundary + "\r\n")
	w.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=%q\r\n\r\n", name))
	w.WriteString(value + "\r\n")
	return nil
}

func (w *simpleWriter) CreateFormFile(name, filename string) (io.Writer, error) {
	w.WriteString("--" + w.boundary + "\r\n")
	w.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=%q; filename=%q\r\n", name, filename))
	w.WriteString("Content-Type: audio/wav\r\n\r\n")
	return w, nil
}

func (w *simpleWriter) Close() error {
	w.WriteString("--" + w.boundary + "--\r\n")
	return nil
}

func (w *simpleWriter) FormDataContentType() string {
	return "multipart/form-data; boundary=" + w.boundary
}

// ─── AI response (kangwifi gemini) ────────────────────────────────

func aicallGetAIResponse(userText, callerJID string) (string, error) {
	// Build conversation context
	aicallMemMu.Lock()
	history := aicallMemory[callerJID]
	aicallMemMu.Unlock()

	aicallCfgMu.RLock()
	systemPrompt := aicallCfg.SystemPrompt
	aicallCfgMu.RUnlock()

	// Build prompt with history
	var promptParts []string
	promptParts = append(promptParts, "System: "+systemPrompt)
	promptParts = append(promptParts, "\nConversation:")
	for _, turn := range history {
		role := "User"
		if turn.Role == "assistant" {
			role = "Assistant"
		}
		promptParts = append(promptParts, fmt.Sprintf("%s: %s", role, turn.Content))
	}
	promptParts = append(promptParts, "User: "+userText)
	promptParts = append(promptParts, "Assistant:")

	fullPrompt := strings.Join(promptParts, "\n")

	// Call AI backend (dulu kwAIChat, sekarang aiBackendAsk)
	resp, _, err := aiBackendAsk(fullPrompt)
	if err != nil {
		return "", err
	}

	// Save to memory
	aicallMemMu.Lock()
	aicallMemory[callerJID] = append(aicallMemory[callerJID],
		ChatTurn{Role: "user", Content: userText},
		ChatTurn{Role: "assistant", Content: resp},
	)
	// Trim to last N turns
	if len(aicallMemory[callerJID]) > aicallMaxTurns*2 {
		aicallMemory[callerJID] = aicallMemory[callerJID][len(aicallMemory[callerJID])-aicallMaxTurns*2:]
	}
	aicallMemMu.Unlock()

	return resp, nil
}

// ─── TTS: Google Translate ────────────────────────────────────────

func aicallTTS(text string) ([]byte, error) {
	// Multi-provider TTS: Edge Neural (terbaik) → Google Translate (fallback)
	// Implementasi di plugins/tts_providers.go
	audio, provider, err := ttsSynthesize(text)
	if err != nil {
		return nil, err
	}
	fmt.Printf("[AICALL] TTS via %s: %d bytes\n", provider, len(audio))
	return audio, nil
}

// ─── Speak: TTS + Play to caller ──────────────────────────────────
// audioCh is the permanent audio channel — we drain it during playback
// to prevent echo (our own TTS audio being picked up by the mic).

func aicallSpeak(ctx context.Context, call *meowcaller.Call, text string, session *aiCallSession, audioCh chan []float32) {
	// Generate TTS
	audioData, err := aicallTTS(text)
	if err != nil {
		fmt.Printf("[AICALL] TTS error: %v\n", err)
		return
	}
	if len(audioData) == 0 {
		return
	}

	// Save to temp file (meowcaller needs file path for MP3File)
	tmpFile, err := os.CreateTemp("", "aicall-tts-*.mp3")
	if err != nil {
		fmt.Printf("[AICALL] temp file error: %v\n", err)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(audioData); err != nil {
		tmpFile.Close()
		return
	}
	tmpFile.Close()

	// Open as meowcaller MP3 source
	src, err := meowcaller.MP3File(tmpPath)
	if err != nil {
		fmt.Printf("[AICALL] MP3 decode error: %v\n", err)
		return
	}
	defer src.Close()

	// Play and wait for finish
	finishCh := make(chan struct{}, 1)
	player := call.Play(src)
	if player == nil {
		fmt.Printf("[AICALL] Play returned nil player\n")
		return
	}
	player.OnFinish(func() {
		select {
		case finishCh <- struct{}{}:
		default:
		}
	})

	// Wait for playback to finish or context to cancel.
	// While playing, DRAIN audioCh to prevent echo (our TTS audio
	// being captured by the receiver and re-transcribed).
	drainDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-audioCh:
				// discard echo
			case <-drainDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	select {
	case <-finishCh:
		// playback done
	case <-ctx.Done():
		// call ended
		if player != nil {
			player.Stop()
		}
	}
	// Stop the drain goroutine
	close(drainDone)
}

// keep imports used
var _ = whatsmeow.MediaAudio
