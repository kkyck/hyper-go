// plugin.go — Sistem plugin self-registering.
//
// CARA PAKAI (tambah plugin baru):
//  1. Buat file baru di plugins/, mis. plugins/halo.go
//  2. Tulis init() yang memanggil RegisterPlugin — SELESAI.
//  3. Tidak perlu ubah main.go, menu.go, atau file lain.
//
// Contoh minimal (plugins/halo.go):
//
//	package main
//
//	import (
//	    whatsmeow "github.com/polymorfa/hypermeow"
//	    "github.com/polymorfa/hypermeow/types/events"
//	)
//
//	func init() {
//	    RegisterPlugin(Plugin{
//	        Command:     "halo",
//	        Aliases:     []string{"hi", "hello"},
//	        Description: "Sapa balik",
//	        Category:    "Fun",
//	        Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
//	            nama := args
//	            if nama == "" {
//	                nama = "kawan"
//	            }
//	            return "Halo " + nama + "! 👋"
//	        },
//	    })
//	}
//
// Catatan:
//   - Command tanpa titik, otomatis lower-case. ".halo", ".hi", ".hello" aktif.
//   - Handler return "" = silent (tidak kirim balasan).
//   - OwnerOnly=true = hanya owner bisa pakai.
//   - Plugin lewat gate normal (self-mode/trust/lock grup). Untuk command yang
//     harus bypass gate (admin tools), tetap daftar manual di switch pre-gate
//     core/main.go — itu disengaja demi keamanan.
//   - Plugin otomatis muncul di .menu sesuai Category.
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// Plugin adalah satu command self-registering.
type Plugin struct {
	// Command: nama utama tanpa titik, mis. "halo". Wajib, unik.
	Command string
	// Aliases: nama alternatif tanpa titik, mis. ["hi"].
	Aliases []string
	// Description: teks di .menu, mis. "Sapa balik".
	Description string
	// Category: grup di .menu, mis. "Fun". Default "Plugin".
	Category string
	// OwnerOnly: true = hanya owner.
	OwnerOnly bool
	// PreGate: true = jalan SEBELUM gate check (untuk admin tools yang harus
	// selalu respon meski self-mode/lock aktif). Default false.
	PreGate bool
	// Handler: client, msg, cmd (dengan titik, mis. ".halo"), args -> teks balasan.
	// cmd adalah alias yang sebenarnya dipanggil (berguna bila ada Aliases).
	// Return "" = silent.
	Handler func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string
	// ── Resource management ──
	// Enabled: false = plugin mati, tidak respon. Default false (off),
	// otomatis ON saat ada trigger (command dipanggil).
	Enabled bool
	// LastUsed: terakhir kali plugin dipakai (untuk auto-kill).
	LastUsed time.Time
	// IdleKill: auto-kill setelah idle selama ini. 0 = pakai default (10 mnt).
	// Negatif = jangan auto-kill.
	IdleKill time.Duration
	// NoAutoKill: true = tidak pernah auto-kill (untuk plugin kritis).
	NoAutoKill bool
	// OnKill: hook cleanup saat plugin di-kill (tutup koneksi, clear cache, stop goroutine).
	OnKill func()
}

var (
	pluginMu       sync.RWMutex
	pluginRegistry = map[string]*Plugin{} // "halo" -> plugin (termasuk alias)
	pluginList     = []*Plugin{}          // urutan pendaftaran, untuk .menu
)

// RegisterPlugin mendaftarkan plugin. Panggil dari init() di file plugin.
// Panik bila Command kosong/duplikat — fail fast saat startup, bukan diam.
func RegisterPlugin(p Plugin) {
	cmd := strings.ToLower(strings.TrimSpace(p.Command))
	if cmd == "" {
		panic("plugin: Command kosong")
	}
	if p.Handler == nil {
		panic(fmt.Sprintf("plugin %q: Handler nil", cmd))
	}
	if p.Category == "" {
		p.Category = "Plugin"
	}
	if p.Description == "" {
		p.Description = cmd
	}
	pp := p // copy agar aman
	pp.Command = cmd
	aliases := []string{}
	for _, a := range p.Aliases {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && a != cmd {
			aliases = append(aliases, a)
		}
	}
	pp.Aliases = aliases

	pluginMu.Lock()
	defer pluginMu.Unlock()
	names := append([]string{cmd}, aliases...)
	for _, n := range names {
		if _, exists := pluginRegistry[n]; exists {
			panic(fmt.Sprintf("plugin: command %q sudah terdaftar", n))
		}
	}
	for _, n := range names {
		pluginRegistry[n] = &pp
	}
	pluginList = append(pluginList, &pp)
}

// LookupPlugin mencari plugin dari cmd (dengan titik, mis. ".halo").
// Prefix "!" juga diterima, karena perintah JAM bisa dipanggil dengan
// tanda seru juga.
func LookupPlugin(cmd string) (*Plugin, bool) {
	name := strings.ToLower(strings.TrimSpace(cmd))
	name = strings.TrimPrefix(name, ".")
	name = strings.TrimPrefix(name, "!")
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	p, ok := pluginRegistry[name]
	return p, ok
}

// ListPlugins mengembalikan semua plugin terdaftar (urutan daftar).
func ListPlugins() []*Plugin {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	out := make([]*Plugin, len(pluginList))
	copy(out, pluginList)
	return out
}

// runPreGatePlugin mengeksekusi plugin PreGate (bypass gate). Return (reply, true)
// bila command cocok dengan plugin PreGate.
func runPreGatePlugin(client *whatsmeow.Client, msg *events.Message, cmd, args string) (string, bool) {
	plug, ok := LookupPlugin(cmd)
	if !ok || !plug.PreGate {
		return "", false
	}
	if plug.OwnerOnly && !isOwnerMsg(msg) {
		return "❌ Hanya owner", true
	}
	// Trigger: otomatis ON + update LastUsed
	pluginMu.Lock()
	plug.Enabled = true
	plug.LastUsed = time.Now()
	pluginMu.Unlock()
	defer func() {
		_ = recover()
	}()
	return plug.Handler(client, msg, cmd, args), true
}

// runPlugin mengeksekusi plugin dengan guard owner + panic recovery.
// Trigger otomatis menyalakan plugin (default OFF) + update LastUsed.
// Return "" bila silent/tidak ada plugin.
func runPlugin(client *whatsmeow.Client, msg *events.Message, cmd, args string) (string, bool) {
	plug, ok := LookupPlugin(cmd)
	if !ok {
		return "", false
	}
	if plug.OwnerOnly && !isOwnerMsg(msg) {
		return "❌ Hanya owner", true
	}
	// Trigger: otomatis ON + update LastUsed
	pluginMu.Lock()
	plug.Enabled = true
	plug.LastUsed = time.Now()
	pluginMu.Unlock()
	defer func() {
		_ = recover() // plugin tidak boleh menjatuhkan bot
	}()
	return plug.Handler(client, msg, cmd, args), true
}

// menuPluginsSection membuat section .menu untuk plugin terdaftar,
// dikelompokkan per Category. Hanya tampilkan plugin yang bisa diakses
// oleh user (ownerOnly disaring untuk non-owner).
// Return "" bila tidak ada plugin.
func menuPluginsSection(isOwner bool) string {
	plugs := ListPlugins()
	if len(plugs) == 0 {
		return ""
	}
	// kelompokkan per category, jaga urutan kemunculan
	// saring OwnerOnly untuk non-owner
	var cats []string
	byCat := map[string][]*Plugin{}
	for _, p := range plugs {
		if p.OwnerOnly && !isOwner {
			continue
		}
		cat := p.Category
		if cat == "" {
			cat = "Plugin"
		}
		if _, ok := byCat[cat]; !ok {
			cats = append(cats, cat)
		}
		byCat[cat] = append(byCat[cat], p)
	}
	var sb strings.Builder
	sb.WriteString("*🧩 Plugin*\n")
	for _, c := range cats {
		if len(cats) > 1 {
			sb.WriteString("_" + c + "_\n")
		}
		for _, p := range byCat[c] {
			sb.WriteString("• *" + botMenuPrefix + p.Command + "* — " + p.Description + "\n")
		}
	}
	sb.WriteString("\n")
	return sb.String()
}
