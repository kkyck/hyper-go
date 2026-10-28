package main

// latency_exp.go — Eksperimen latensi bot ke server WhatsApp.
//
// Tujuan: latensi kecil dan stabil.
// Metode:
//   1. Tuning keepalive interval (lebih sering = deteksi putus lebih cepat)
//   2. TCP_NODELAY (disable Nagle = kirim langsung tanpa buffer)
//   3. Monitor RTT via .pinglat
//
// Usage:
//   .pinglat          — ukur RTT ke server WA (kirim pesan ke diri sendiri)
//   .latmode fast     — keepalive agresif (15s)
//   .latmode normal   — keepalive default (20-30s)
//   .latmode save     — keepalive hemat (30-45s)

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

var (
	latMode   = "normal"
	latModeMu sync.RWMutex
	latStats  = &latencyStats{}
)

type latencyStats struct {
	mu       sync.Mutex
	samples  []time.Duration
	lastRTT  time.Duration
	lastTime time.Time
}

func (s *latencyStats) add(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, d)
	if len(s.samples) > 100 {
		s.samples = s.samples[1:]
	}
	s.lastRTT = d
	s.lastTime = time.Now()
}

func (s *latencyStats) avg() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range s.samples {
		sum += d
	}
	return sum / time.Duration(len(s.samples))
}

func handleLatencyCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	sub := strings.Fields(strings.ToLower(args))
	if len(sub) == 0 {
		return latencyStatus()
	}
	switch sub[0] {
	case "fast", "normal", "save":
		return setLatencyMode(sub[0])
	default:
		return latencyStatus()
	}
}

func latencyStatus() string {
	latModeMu.RLock()
	mode := latMode
	latModeMu.RUnlock()
	avg := latStats.avg()
	return fmt.Sprintf("📡 *Latency Mode:* %s\n📊 RTT rata-rata: %v\n📈 Sampel: %d\n\nMode: `.latmode fast|normal|save`",
		mode, avg.Round(time.Millisecond), len(latStats.samples))
}

func setLatencyMode(mode string) string {
	latModeMu.Lock()
	latMode = mode
	latModeMu.Unlock()

	switch mode {
	case "fast":
		whatsmeow.KeepAliveIntervalMin = 15 * time.Second
		whatsmeow.KeepAliveIntervalMax = 20 * time.Second
		whatsmeow.KeepAliveResponseDeadline = 8 * time.Second
	case "save":
		whatsmeow.KeepAliveIntervalMin = 30 * time.Second
		whatsmeow.KeepAliveIntervalMax = 45 * time.Second
		whatsmeow.KeepAliveResponseDeadline = 15 * time.Second
	default: // normal
		whatsmeow.KeepAliveIntervalMin = 20 * time.Second
		whatsmeow.KeepAliveIntervalMax = 30 * time.Second
		whatsmeow.KeepAliveResponseDeadline = 10 * time.Second
	}
	return fmt.Sprintf("✅ Latency mode: %s", mode)
}

func handlePingLatCommand(client *whatsmeow.Client, msg *events.Message) string {
	if client == nil {
		return "❌ Client tidak siap"
	}
	start := time.Now()
	// Kirim presence sebagai ping (ringan, tidak ganggu)
	err := client.SendPresence(context.Background(), types.PresenceAvailable)
	rtt := time.Since(start)
	if err != nil {
		return fmt.Sprintf("❌ Ping gagal: %v", err)
	}
	latStats.add(rtt)
	return fmt.Sprintf("🏓 RTT: %v\n📊 Rata-rata: %v",
		rtt.Round(time.Millisecond), latStats.avg().Round(time.Millisecond))
}

func init() {
	// Set fast keepalive as default on startup (15s interval, 8s deadline)
	whatsmeow.KeepAliveIntervalMin = 15 * time.Second
	whatsmeow.KeepAliveIntervalMax = 20 * time.Second
	whatsmeow.KeepAliveResponseDeadline = 8 * time.Second

	RegisterPlugin(Plugin{
		Command:     "pinglat",
		Description: "pinglat",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePingLatCommand(client, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "latmode",
		Description: "latmode",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleLatencyCommand(client, msg, args)
		},
	})
}
