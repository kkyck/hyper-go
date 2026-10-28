package main

import (
	"fmt"
	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
	"os"
	"runtime"
	"strings"
	"time"
)

/**
 * 📊 RSS / Stats / Process — Memory & Runtime Stats
 * ════════════════════════════════════════════════════════════
 * Port dari plugins/rss.ts (Baileys), plus alias .stats & .proses
 *
 * Commands (prefix .):
 *   .rss / .mem / .heap   → Snapshot memory, GC, goroutine, uptime
 *   .rss compress / gc    → Paksa runtime.GC()
 *   .rss restart          → Petunjuk restart proses
 *   .stats                → Runtime memory info (rich format matching user spec)
 *   .proses               → Process info (PID, Go version, uptime, CPU user/sys)
 */

var botStartTime = time.Now()

// startTime returns the boot time of the bot (used by .proses)
func startTime() time.Time { return botStartTime }

func formatBytes(b uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(MiB))
	case b >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatUptime(d time.Duration) string {
	sec := int(d.Seconds())
	days := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	parts = append(parts, fmt.Sprintf("%ds", s))
	return strings.Join(parts, " ")
}

// formatUptimeLong matches the user's pasted format like "1d 11h 41m 21s"
func formatUptimeLong(d time.Duration) string {
	sec := int(d.Seconds())
	days := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	return fmt.Sprintf("%dd %dh %dm %ds", days, h, m, s)
}

func handleRSSCommand(args string) string {
	sub := ""
	fields := strings.Fields(strings.ToLower(args))
	if len(fields) > 0 {
		sub = fields[0]
	}

	switch sub {
	case "compress", "c", "gc":
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		runtime.GC()
		runtime.ReadMemStats(&after)
		freed := int64(before.HeapAlloc) - int64(after.HeapAlloc)
		if freed < 0 {
			freed = 0
		}
		return fmt.Sprintf(`🗜️ *RSS / GC Compress*

• Heap sebelum: *%s*
• Heap sesudah: *%s*
• Turun: *%s*
• GC cycles: %d

Catatan: di Go, memori sering ditahan allocator OS — restart proses cara paling bersih untuk reset RSS.`,
			formatBytes(before.HeapAlloc),
			formatBytes(after.HeapAlloc),
			formatBytes(uint64(freed)),
			after.NumGC)

	case "restart", "r":
		return `♻️ *Restart*

Bot Go tidak me-restart dirinya sendiri secara aman dari dalam proses.

*Cara restart:*
1. Stop bot (Ctrl+C / systemctl stop)
2. Jalankan lagi: ./bot

Atau pakai supervisor (systemd / pm2-like) agar auto-restart.`
	}

	// default: full stats
	return handleStatsCommand()
}

// handleStatsCommand prints runtime memory stats in the user's chosen
// Node-style layout, mapped to Go runtime.MemStats fields.
//
// Node.js      → Go equivalent
//
//	rss          → Sys (memory obtained from OS)
//	heapTotal    → HeapSys (total heap memory reserved)
//	heapUsed     → HeapAlloc (live heap)
//	heapIdle     → HeapIdle (idle heap spans)
//	heapReleased → HeapReleased (memory returned to OS)
//	external     → OtherSys (non-heap, non-stack sys)
//	arrayBuffers → HeapInuse (Go has no ArrayBuffers concept)
func handleStatsCommand() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	mib := func(b uint64) string {
		return fmt.Sprintf("%.1fMiB", float64(b)/float64(1024*1024))
	}

	return fmt.Sprintf(`ᯓ *MEMORY USAGE* ᯓ
⊹ *rss*: %s
⊹ *heapTotal*: %s
⊹ *heapUsed*: %s
⊹ *heapIdle*: %s
⊹ *heapReleased*: %s
⊹ *external*: %s
⊹ *arrayBuffers*: %s`,
		mib(m.Sys),
		mib(m.HeapSys),
		mib(m.HeapAlloc),
		mib(m.HeapIdle),
		mib(m.HeapReleased),
		mib(m.OtherSys),
		mib(m.HeapInuse))
}

// handleProsesCommand prints process info matching the user's pasted Node.js
// example, mapped to Go runtime equivalents.
//
//	PID       → os.Getpid()
//	Node:     → runtime.Version()
//	Uptime    → time.Since(botStartTime)
//	CPU User  → process CPU time (user) via syscall.Getrusage
//	CPU Sys   → process CPU time (system) via syscall.Getrusage
func handleProsesCommand() string {
	pid := os.Getpid()
	goVer := runtime.Version()
	uptime := time.Since(botStartTime)

	// Process CPU time (user + system) via syscall.Getrusage
	cpuUserMs, cpuSysMs := getRusageCPUTimeMs()

	return fmt.Sprintf(`⚙️ *Process*

• PID: %d
• Go: %s
• Uptime: %s
• CPU User: %d ms
• CPU Sys: %d ms`,
		pid, goVer,
		formatUptimeLong(uptime),
		cpuUserMs, cpuSysMs)
}

// getProcessCPUTimeMs returns (userMs, sysMs) for the current process.
func getProcessCPUTimeMs() (int64, int64) {
	return getRusageCPUTimeMs()
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "rss",
		Aliases:     []string{"heap"},
		Description: "rss",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleRSSCommand(args)
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "runtime",
		Description: "runtime",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleRSSCommand("")
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "stats",
		Description: "stats",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleStatsCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "proses",
		Description: "proses",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleProsesCommand()
		},
	})
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "gc",
		Description: "gc",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleRSSCommand("compress")
			// ─── Extras ──────────────────────────────────────────────
		},
	})
}
