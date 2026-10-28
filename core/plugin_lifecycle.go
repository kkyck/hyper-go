package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// plugin_lifecycle.go — Sistem manajemen resource plugin.
//
// Aturan:
//  - Default semua plugin OFF (Enabled=false).
//  - Trigger (command dipanggil) → otomatis ON, LastUsed diupdate.
//  - Auto-kill: plugin yang idle lebih dari IdleKill (default 10 mnt)
//    otomatis dimatikan + OnKill dipanggil.
//  - Manual: .plugon / .plugoff / .plugkill / .pluglist (owner only).
//  - Plugin dengan NoAutoKill=true tidak pernah auto-kill.

const defaultPluginIdleKill = 10 * time.Minute

func pluginIdleKillDuration(p *Plugin) time.Duration {
	if p.IdleKill < 0 {
		return 0 // jangan auto-kill
	}
	if p.IdleKill > 0 {
		return p.IdleKill
	}
	return defaultPluginIdleKill
}

// pluginEnable menyalakan plugin.
func pluginEnable(cmd string) bool {
	name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(cmd, ".")))
	pluginMu.Lock()
	defer pluginMu.Unlock()
	p, ok := pluginRegistry[name]
	if !ok {
		return false
	}
	p.Enabled = true
	p.LastUsed = time.Now()
	return true
}

// pluginDisable mematikan plugin (soft, tanpa OnKill).
func pluginDisable(cmd string) bool {
	name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(cmd, ".")))
	pluginMu.Lock()
	defer pluginMu.Unlock()
	p, ok := pluginRegistry[name]
	if !ok {
		return false
	}
	p.Enabled = false
	return true
}

// pluginKill mematikan plugin + panggil OnKill (cleanup resource).
func pluginKill(cmd string) bool {
	name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(cmd, ".")))
	pluginMu.Lock()
	p, ok := pluginRegistry[name]
	if !ok {
		pluginMu.Unlock()
		return false
	}
	p.Enabled = false
	onKill := p.OnKill
	pluginMu.Unlock()

	if onKill != nil {
		func() {
			defer func() { _ = recover() }()
			onKill()
		}()
	}
	return true
}

// pluginCategoryAction menjalankan aksi (on/off/kill) ke semua plugin
// dalam satu kategori (case-insensitive; kategori kosong = "Plugin").
// Return jumlah plugin yang terkena aksi.
func pluginCategoryAction(cat, action string) int {
	if cat == "" {
		cat = "Plugin"
	}
	pluginMu.Lock()
	var toKill []*Plugin
	n := 0
	for _, p := range pluginList {
		pcat := p.Category
		if pcat == "" {
			pcat = "Plugin"
		}
		if !strings.EqualFold(pcat, cat) {
			continue
		}
		switch action {
		case "on":
			p.Enabled = true
			p.LastUsed = time.Now()
			n++
		case "off":
			if p.Enabled {
				p.Enabled = false
				n++
			}
		case "kill":
			if p.Enabled && !p.NoAutoKill {
				p.Enabled = false
				toKill = append(toKill, p)
				n++
			}
		}
	}
	pluginMu.Unlock()

	for _, p := range toKill {
		if p.OnKill != nil {
			func() {
				defer func() { _ = recover() }()
				p.OnKill()
			}()
		}
	}
	return n
}

// pluginKillAll mematikan semua plugin yang sedang ON (kecuali NoAutoKill).
// Return jumlah yang di-kill.
func pluginKillAll() int {
	pluginMu.Lock()
	var toKill []*Plugin
	for _, p := range pluginList {
		if p.Enabled && !p.NoAutoKill {
			p.Enabled = false
			toKill = append(toKill, p)
		}
	}
	pluginMu.Unlock()

	for _, p := range toKill {
		if p.OnKill != nil {
			func() {
				defer func() { _ = recover() }()
				p.OnKill()
			}()
		}
	}
	return len(toKill)
}

// pluginSweepOnce mematikan plugin yang idle terlalu lama. Dipanggil tiap menit.
func pluginSweepOnce() int {
	now := time.Now()
	pluginMu.Lock()
	var toKill []*Plugin
	for _, p := range pluginList {
		if !p.Enabled || p.NoAutoKill {
			continue
		}
		idleDur := pluginIdleKillDuration(p)
		if idleDur <= 0 {
			continue
		}
		if now.Sub(p.LastUsed) > idleDur {
			p.Enabled = false
			toKill = append(toKill, p)
		}
	}
	pluginMu.Unlock()

	for _, p := range toKill {
		if p.OnKill != nil {
			func() {
				defer func() { _ = recover() }()
				p.OnKill()
			}()
		}
	}
	return len(toKill)
}

// startPluginSweeper menjalankan background sweeper tiap 1 menit.
func startPluginSweeper() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if n := pluginSweepOnce(); n > 0 {
				fmt.Printf("[plugin] auto-kill %d idle plugin\n", n)
			}
		}
	}()
}

// ─── Commands (owner only) ───

func handlePlugOn(_ *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	cmd := strings.TrimSpace(args)
	if cmd == "" {
		return "❓ *Cara pakai:* *.plugon* <command>"
	}
	if pluginEnable(cmd) {
		return "✅ Plugin *" + cmd + "* ON"
	}
	return "❌ Plugin *" + cmd + "* tidak ditemukan"
}

func handlePlugOff(_ *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	cmd := strings.TrimSpace(args)
	if cmd == "" {
		return "❓ *Cara pakai:* *.plugoff* <command>"
	}
	if pluginDisable(cmd) {
		return "✅ Plugin *" + cmd + "* OFF"
	}
	return "❌ Plugin *" + cmd + "* tidak ditemukan"
}

func handlePlugKill(_ *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	cmd := strings.TrimSpace(args)
	if cmd == "" {
		return "❓ *Cara pakai:* *.plugkill* <command>"
	}
	if pluginKill(cmd) {
		return "💀 Plugin *" + cmd + "* di-kill (resource dibersihkan)"
	}
	return "❌ Plugin *" + cmd + "* tidak ditemukan"
}

func handlePlugKillAll(_ *whatsmeow.Client, msg *events.Message, _ string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	n := pluginKillAll()
	return fmt.Sprintf("💀 %d plugin di-kill", n)
}

func handlePlugCat(action string) func(*whatsmeow.Client, *events.Message, string) string {
	return func(_ *whatsmeow.Client, msg *events.Message, args string) string {
		if !isOwnerMsg(msg) {
			return "❌ Hanya owner"
		}
		cat := strings.TrimSpace(args)
		labels := map[string]string{"on": "plugoncat", "off": "plugoffcat", "kill": "plugkillcat"}
		if cat == "" {
			return fmt.Sprintf("❓ *Cara pakai:* *.%s* <kategori>\nContoh: `.%s AI`", labels[action], labels[action])
		}
		verb := map[string]string{"on": "diaktifkan", "off": "dimatikan", "kill": "di-kill"}
		n := pluginCategoryAction(cat, action)
		if n == 0 {
			return fmt.Sprintf("❌ Kategori tidak ditemukan atau tidak ada plugin aktif: %s", cat)
		}
		return fmt.Sprintf("✅ %d plugin kategori *%s* %s", n, cat, verb[action])
	}
}

func handlePlugList(_ *whatsmeow.Client, msg *events.Message, _ string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	plugs := ListPlugins()

	// Pisah ON dan OFF. ON ditampilkan detail, OFF diringkas per kategori.
	var onPlugs []*Plugin
	byCat := map[string]int{}
	var cats []string
	for _, p := range plugs {
		if p.Enabled {
			onPlugs = append(onPlugs, p)
			continue
		}
		cat := p.Category
		if cat == "" {
			cat = "Plugin"
		}
		if _, ok := byCat[cat]; !ok {
			cats = append(cats, cat)
		}
		byCat[cat]++
	}
	sort.Slice(onPlugs, func(i, j int) bool { return onPlugs[i].Command < onPlugs[j].Command })
	sort.Strings(cats)

	var sb strings.Builder
	sb.WriteString("*🔌 Plugin Status*\n\n")
	sb.WriteString(fmt.Sprintf("🟢 *ON: %d* / %d total\n", len(onPlugs), len(plugs)))
	for _, p := range onPlugs {
		idle := "-"
		if !p.LastUsed.IsZero() {
			idle = time.Since(p.LastUsed).Round(time.Second).String() + " lalu"
		}
		sb.WriteString(fmt.Sprintf("• `.%s` — %s\n", p.Command, idle))
	}
	sb.WriteString("\n⚫ *OFF per kategori:*\n")
	for _, c := range cats {
		sb.WriteString(fmt.Sprintf("• %s: %d\n", c, byCat[c]))
	}
	sb.WriteString("\nAuto-kill: idle >10 mnt\n")
	sb.WriteString("`*.plugon/.plugoff/.plugkill <cmd>*")
	return sb.String()
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "plugon",
		Description: "nyalakan plugin",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugOn(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "plugoff",
		Description: "matikan plugin",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugOff(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "plugkill",
		Description: "kill plugin + cleanup",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugKill(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "plugkillall",
		Description: "kill semua plugin ON",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugKillAll(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "plugoncat",
		Description: "aktifkan semua plugin dalam kategori",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugCat("on")(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "plugoffcat",
		Description: "matikan semua plugin dalam kategori",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugCat("off")(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "plugkillcat",
		Description: "kill semua plugin dalam kategori",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugCat("kill")(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "pluglist",
		Description: "status semua plugin",
		Category:    "Plugin",
		OwnerOnly:   true,
		NoAutoKill:  true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handlePlugList(client, msg, args)
		},
	})
}
