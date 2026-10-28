package main

// sysinfo.go — System Info + Ping + Node.js eval
//
// Commands:
//   .ping        → roundtrip ping (port dari core-ping2.js Baileys)
//   .sysinfo     → system info (CPU, RAM, uptime, Go version, dll)
//   .evaljs <code> → evaluasi kode Node.js (owner only)

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// ─── .ping — roundtrip measurement (port dari core-ping2.js) ────

func handlePingCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !isOwnerMsg(msg) && !isTrustedMsg(msg) && !isGroupAdmin(client, msg) {
		return "❌ Hanya owner/trusted/admin"
	}

	start := time.Now()

	// ─── Self SPR mode (per-group): send via SPR Hide (skip edit pattern) ───
	// When selfSpr is ON for THIS group, .ping sends final result via SPR
	// instead of the normal send+edit pattern (which bypasses humanSendReply).
	// ⚡ OPTIMIZED: removed time.Sleep(10ms) — was simulating send delay, not needed
	if isSelfSprActive(msg.Info.Chat.String()) && msg.Info.IsGroup {
		diff := time.Since(start).Milliseconds()
		finalText := fmt.Sprintf("🏓 *Pong!*\n\n⚡ Roundtrip: *%dms*", diff)
		// Route via SPR Hide to owner + whitelist (per-group)
		sendReplyViaSPR2(client, msg, finalText)
		return "" // silent — SPR sent
	}

	// Step 1: Send "pong" message (humanized — looks like official WhatsApp)
	pongMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("🏓 pong"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:    proto.String(msg.Info.ID),
				Participant: proto.String(msg.Info.Sender.String()),
			},
		},
	}
	pongMsg = humanizeMessage(pongMsg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.SendMessage(ctx, msg.Info.Chat, pongMsg)
	if err != nil {
		return fmt.Sprintf("❌ Gagal kirim pong: %v", err)
	}

	// Step 2: Calculate roundtrip
	diff := time.Since(start).Milliseconds()

	// Step 3: Edit message with roundtrip time (only)
	editType := waE2E.ProtocolMessage_MESSAGE_EDIT
	editedText := fmt.Sprintf("🏓 *Pong!*\n\n⚡ Roundtrip: *%dms*", diff)

	editMsg := &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Key: &waCommon.MessageKey{
				RemoteJID: proto.String(msg.Info.Chat.String()),
				FromMe:    proto.Bool(true),
				ID:        proto.String(resp.ID),
			},
			Type: &editType,
			EditedMessage: &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(editedText),
					ContextInfo: &waE2E.ContextInfo{
						StanzaID:    proto.String(msg.Info.ID),
						Participant: proto.String(msg.Info.Sender.String()),
					},
				},
			},
		},
	}
	editMsg = humanizeMessage(editMsg)

	editCtx, editCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer editCancel()
	_, editErr := client.SendMessage(editCtx, msg.Info.Chat, editMsg, whatsmeow.SendRequestExtra{
		ID: types.MessageID(resp.ID),
	})
	if editErr != nil {
		return editedText
	}

	return ""
}

// ─── .sysinfo — System Info ─────────────────────────────────────

func handleSysInfoCommand(client *whatsmeow.Client, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	var b strings.Builder
	b.WriteString("🖥️ *SERVER INFORMATION*\n")

	// ── Hardware ──
	cpuModel, cpuClock := sysinfoCPU()
	b.WriteString("\n*Hardware*\n")
	b.WriteString(fmt.Sprintf("• CPU : %s\n", cpuModel))
	b.WriteString(fmt.Sprintf("• Core : %d\n", runtime.NumCPU()))
	b.WriteString(fmt.Sprintf("• Clock : %s\n", cpuClock))
	b.WriteString(fmt.Sprintf("• Architecture : %s\n", runtime.GOARCH))
	b.WriteString(fmt.Sprintf("• Endianness : %s\n", sysinfoEndianness()))

	// ── Memory ──
	memTotal, memFree := sysinfoMem()
	b.WriteString("\n*Memory*\n")
	b.WriteString(fmt.Sprintf("• Total : %s\n", sysinfoBytes(memTotal)))
	b.WriteString(fmt.Sprintf("• Used : %s\n", sysinfoBytes(memTotal-memFree)))
	b.WriteString(fmt.Sprintf("• Free : %s\n", sysinfoBytes(memFree)))

	// ── Operating System ──
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "Unknown"
	}
	b.WriteString("\n*Operating System*\n")
	b.WriteString(fmt.Sprintf("• Platform : %s\n", runtime.GOOS))
	b.WriteString(fmt.Sprintf("• Type : %s\n", sysinfoOSType()))
	b.WriteString(fmt.Sprintf("• Release : %s\n", sysinfoKernelRelease()))
	b.WriteString(fmt.Sprintf("• Hostname : %s\n", hostname))
	b.WriteString(fmt.Sprintf("• Uptime : %s\n", formatUptimeLong(sysinfoHostUptime())))

	// ── Runtime (Go) ──
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	b.WriteString("\n*Runtime*\n")
	b.WriteString(fmt.Sprintf("• Go : %s\n", runtime.Version()))
	b.WriteString(fmt.Sprintf("• GOMAXPROCS : %d\n", runtime.GOMAXPROCS(0)))
	b.WriteString(fmt.Sprintf("• PID : %d\n", os.Getpid()))
	b.WriteString(fmt.Sprintf("• Uptime : %s\n", formatUptimeLong(time.Since(botStartTime))))
	b.WriteString(fmt.Sprintf("• Architecture : %s\n", runtime.GOARCH))
	b.WriteString(fmt.Sprintf("• Goroutines : %d\n", runtime.NumGoroutine()))
	b.WriteString(fmt.Sprintf("• GC runs : %d\n", m.NumGC))

	// ── Process Memory ──
	b.WriteString("\n*Process Memory*\n")
	b.WriteString(fmt.Sprintf("• RSS : %s\n", sysinfoBytes(sysinfoRSS())))
	b.WriteString(fmt.Sprintf("• Heap Used : %s\n", sysinfoBytes(m.HeapAlloc)))
	b.WriteString(fmt.Sprintf("• Heap Total : %s\n", sysinfoBytes(m.HeapSys)))
	b.WriteString(fmt.Sprintf("• Stack : %s\n", sysinfoBytes(m.StackSys)))

	return b.String()
}

// sysinfoBytes formats bytes as "23.41 GB" / "233.00 MB" (decimal, 2 decimals).
func sysinfoBytes(n uint64) string {
	const (
		KB = 1000
		MB = 1000 * KB
		GB = 1000 * MB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.2f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// sysinfoCPU reads CPU model + clock from /proc/cpuinfo (Linux).
func sysinfoCPU() (model, clock string) {
	model, clock = "Unknown", "Unknown"
	if runtime.GOOS != "linux" {
		return model, clock
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return model, clock
	}
	for _, line := range strings.Split(string(data), "\n") {
		if model == "Unknown" && strings.HasPrefix(line, "model name") {
			if i := strings.Index(line, ":"); i >= 0 {
				model = strings.TrimSpace(line[i+1:])
			}
		}
		if clock == "Unknown" && strings.HasPrefix(line, "cpu MHz") {
			if i := strings.Index(line, ":"); i >= 0 {
				mhz, ferr := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
				if ferr == nil {
					clock = fmt.Sprintf("%.0f MHz", mhz)
				}
			}
		}
		if model != "Unknown" && clock != "Unknown" {
			break
		}
	}
	return model, clock
}

// sysinfoMem reads MemTotal + MemFree from /proc/meminfo (Linux), in bytes.
func sysinfoMem() (total, free uint64) {
	if runtime.GOOS != "linux" {
		return 0, 0
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		var key string
		var kb uint64
		if _, serr := fmt.Sscanf(line, "%s %d", &key, &kb); serr != nil {
			continue
		}
		switch key {
		case "MemTotal:":
			total = kb * 1024
		case "MemFree:":
			free = kb * 1024
		}
		if total > 0 && free > 0 {
			break
		}
	}
	return total, free
}

// sysinfoHostUptime reads system uptime from /proc/uptime (Linux).
func sysinfoHostUptime() time.Duration {
	if runtime.GOOS != "linux" {
		return 0
	}
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// sysinfoRSS reads process RSS from /proc/self/status (Linux), in bytes.
func sysinfoRSS() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb uint64
			if _, serr := fmt.Sscanf(line, "VmRSS: %d", &kb); serr == nil {
				return kb * 1024
			}
		}
	}
	return 0
}

// sysinfoKernelRelease returns kernel release via uname (e.g. "6.17.0-1018-oracle").
func sysinfoKernelRelease() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "Unknown"
	}
	return unix.ByteSliceToString(uts.Release[:])
}

// sysinfoOSType returns a friendly OS type name.
func sysinfoOSType() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}

// sysinfoEndianness returns "LE" or "BE".
func sysinfoEndianness() string {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], 0x1)
	if buf[0] == 0x1 {
		return "LE"
	}
	return "BE"
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return wd
}

// ─── .evaljs — Evaluate Node.js code ────────────────────────────

func handleEvalJSCommand(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner yang bisa pakai .evaljs"
	}

	code := strings.TrimSpace(args)
	if code == "" || code == "help" {
		return `📜 *EVALJS — Node.js Evaluator*

• *.evaljs <code>* — evaluasi kode Node.js

Contoh:
• .evaljs console.log("hello")
• .evaljs const x = 1+2; console.log(x)
• .evaljs const os = require('os'); console.log(os.platform())

Output: stdout + stderr (max 3000 chars)
Timeout: 10 detik`
	}

	nodePath, err := exec.LookPath("node")
	if err != nil || nodePath == "" {
		return "❌ Node.js tidak terinstall. Install dulu: apt install nodejs"
	}

	tmpFile, err := os.CreateTemp("", "evaljs-*.js")
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	wrappedCode := fmt.Sprintf("(async () => {\n%s\n})();\n", code)

	_, err = tmpFile.WriteString(wrappedCode)
	tmpFile.Close()
	if err != nil {
		return fmt.Sprintf("❌ Gagal tulis temp file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "❌ Timeout (10 detik). Kode terlalu lambat atau infinite loop"
		}
		output := string(out)
		if len(output) == 0 {
			output = err.Error()
		}
		return fmt.Sprintf("❌ Error:\n```\n%s\n```", truncateStr(output, 3000))
	}

	output := strings.TrimSpace(string(out))
	if output == "" {
		return "✅ Executed (no output)"
	}
	return fmt.Sprintf("✅ Output:\n```\n%s\n```", truncateStr(output, 3000))
}

// handleGoroutinesCommand — .goroutines
// Shows current goroutine count and optionally kills excess goroutines.
func handleGoroutinesCommand(msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	count := runtime.NumGoroutine()
	var b strings.Builder
	b.WriteString(fmt.Sprintf("🧵 *Goroutines Status*\n\n"))
	b.WriteString(fmt.Sprintf("• Active goroutines: *%d*\n", count))
	b.WriteString(fmt.Sprintf("• Go version: %s\n", runtime.Version()))
	b.WriteString(fmt.Sprintf("• CPUs: %d cores\n", runtime.NumCPU()))

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	b.WriteString(fmt.Sprintf("• Memory alloc: %s\n", formatBytes(m.Alloc)))
	b.WriteString(fmt.Sprintf("• Heap objects: %d\n", m.HeapObjects))

	// GC stats
	var gcStats debug.GCStats
	debug.ReadGCStats(&gcStats)
	b.WriteString(fmt.Sprintf("\n*GC Stats:*\n"))
	b.WriteString(fmt.Sprintf("• GC runs: %d\n", gcStats.NumGC))
	b.WriteString(fmt.Sprintf("• Last GC pause: %s\n", formatUptime(gcStats.PauseTotal)))
	b.WriteString(fmt.Sprintf("• GC pause total: %s\n", formatUptime(gcStats.PauseTotal)))

	if count > 100 {
		b.WriteString(fmt.Sprintf("\n⚠️ *%d goroutines* — jumlah tinggi!\n", count))
		b.WriteString("Saran: jalankan *.goroutines gc* untuk force GC, atau *.goroutines kill* untuk reduce.\n")
	}

	return b.String()
}

// handleGoroutinesAction — .goroutines gc | .goroutines kill
func handleGoroutinesAction(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	sub := strings.ToLower(strings.TrimSpace(args))
	switch sub {
	case "gc", "garbage", "cleanup":
		before := runtime.NumGoroutine()
		var m1, m2 runtime.MemStats
		runtime.ReadMemStats(&m1)
		runtime.GC()
		runtime.ReadMemStats(&m2)
		after := runtime.NumGoroutine()
		freed := int64(m1.HeapAlloc) - int64(m2.HeapAlloc)
		return fmt.Sprintf("✅ *GC Run*\n\n• Goroutines: %d → %d\n• Memory freed: %s\n• Heap objects: %d → %d",
			before, after, formatBytes(uint64(freed)), m1.HeapObjects, m2.HeapObjects)

	case "kill", "reduce":
		// Force GC multiple times to clean up finished goroutines
		before := runtime.NumGoroutine()
		for i := 0; i < 3; i++ {
			runtime.GC()
			time.Sleep(100 * time.Millisecond)
		}
		after := runtime.NumGoroutine()
		return fmt.Sprintf("✅ *Goroutine Cleanup*\n\n• Before: %d\n• After: %d\n• Reduced: %d",
			before, after, before-after)

	case "status", "stat", "":
		return handleGoroutinesCommand(msg)

	default:
		return `🧵 *Goroutines Manager*

• .goroutines — lihat status
• .goroutines gc — force garbage collection
• .goroutines kill — cleanup excess goroutines`
	}
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "goroutines",
		Aliases:     []string{"go"},
		Description: "goroutines",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleGoroutinesAction(args, msg)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "evaljs",
		Aliases:     []string{"jseval", "node"},
		Description: "evaljs",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleEvalJSCommand(args, msg)
		},
	})
}
