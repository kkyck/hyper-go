package main

// config.go — Centralized config for WS / lacak / game server settings.
//
// Reads data/config.json at startup (hot-reloadable via .config reload command).
// Falls back to env vars, then hardcoded defaults.
//
// Fields:
//   - lacak: port, public_url (HTTP tracker for OSINT)
//   - ws:    port, public_url (game server + TTLive + media streaming)
//   - groq:  api_key, model (AI2)
//
// Commands:
//   .config show    → show current config
//   .config reload  → reload config from file (owner)
//   .config set <key> <value> → update config (owner)

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ─── Config types ────────────────────────────────────────────────

type LacakConfig struct {
	Port      string `json:"port"`       // default "24724"
	PublicURL string `json:"public_url"` // default "" (auto from port)
}

type WSConfig struct {
	Port      string `json:"port"`       // default "24848"
	PublicURL string `json:"public_url"` // default "" (auto from port)
}

type GroqConfig struct {
	APIKey string `json:"api_key"` // default built-in
	Model  string `json:"model"`   // default "openai/gpt-oss-20b"
}

type BotConfig struct {
	Lacak LacakConfig `json:"lacak"`
	WS    WSConfig    `json:"ws"`
	Groq  GroqConfig  `json:"groq"`
}

// ─── Defaults ────────────────────────────────────────────────────

func defaultConfig() BotConfig {
	return BotConfig{
		Lacak: LacakConfig{
			Port:      "24724",
			PublicURL: "",
		},
		WS: WSConfig{
			Port:      "24848",
			PublicURL: "",
		},
		Groq: GroqConfig{
			// API key WAJIB lewat env GROQ_API_KEY (tidak di-hardcode).
			APIKey: os.Getenv("GROQ_API_KEY"),
			Model:  getenvDefault("GROQ_MODEL", "openai/gpt-oss-20b"),
		},
	}
}

// ─── State ───────────────────────────────────────────────────────

var (
	configMu     sync.RWMutex
	configCache  BotConfig
	configLoaded bool
)

func configPath() string {
	return filepath.Join("data", "config.json")
}

// loadConfig reads config from data/config.json.
// Falls back to defaults if file missing or invalid.
// Env vars override file values.
func loadConfig() BotConfig {
	cfg := defaultConfig()

	// Read file
	data, err := os.ReadFile(configPath())
	if err == nil {
		var fileCfg BotConfig
		if err := json.Unmarshal(data, &fileCfg); err == nil {
			// Merge file values (only if non-empty)
			if fileCfg.Lacak.Port != "" {
				cfg.Lacak.Port = fileCfg.Lacak.Port
			}
			if fileCfg.Lacak.PublicURL != "" {
				cfg.Lacak.PublicURL = fileCfg.Lacak.PublicURL
			}
			if fileCfg.WS.Port != "" {
				cfg.WS.Port = fileCfg.WS.Port
			}
			if fileCfg.WS.PublicURL != "" {
				cfg.WS.PublicURL = fileCfg.WS.PublicURL
			}
			if fileCfg.Groq.APIKey != "" {
				cfg.Groq.APIKey = fileCfg.Groq.APIKey
			}
			if fileCfg.Groq.Model != "" {
				cfg.Groq.Model = fileCfg.Groq.Model
			}
		}
	}

	// Env overrides (highest priority)
	if v := os.Getenv("LACAK_PORT"); v != "" {
		cfg.Lacak.Port = v
	}
	if v := os.Getenv("LACAK_PUBLIC_URL"); v != "" {
		cfg.Lacak.PublicURL = v
	}
	if v := os.Getenv("GAME_PORT"); v != "" {
		cfg.WS.Port = v
	}
	if v := os.Getenv("GAME_PUBLIC_URL"); v != "" {
		cfg.WS.PublicURL = v
	}
	if v := os.Getenv("GROQ_API_KEY"); v != "" {
		cfg.Groq.APIKey = v
	}
	if v := os.Getenv("GROQ_MODEL"); v != "" {
		cfg.Groq.Model = v
	}

	configMu.Lock()
	configCache = cfg
	configLoaded = true
	configMu.Unlock()

	return cfg
}

// getConfig returns cached config (loads on first call).
func getConfig() BotConfig {
	configMu.RLock()
	if configLoaded {
		cfg := configCache
		configMu.RUnlock()
		return cfg
	}
	configMu.RUnlock()
	return loadConfig()
}

// saveConfig writes config to data/config.json (pretty-printed).
func saveConfig(cfg BotConfig) error {
	_ = os.MkdirAll("data", 0o755)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o644)
}

// ─── Accessors (used by other files) ─────────────────────────────

// lacakPort returns the configured lacak port.
func lacakPort() string {
	return getConfig().Lacak.Port
}

// lacakPublicURL returns the configured lacak public URL.
// Falls back to http://localhost:<port> if not set.
func lacakPublicURL() string {
	cfg := getConfig()
	if cfg.Lacak.PublicURL != "" {
		return strings.TrimRight(cfg.Lacak.PublicURL, "/")
	}
	return "http://localhost:" + cfg.Lacak.Port
}

// wsPort returns the configured WS (game server) port.
func wsPort() string {
	return getConfig().WS.Port
}

// wsPublicURL returns the configured WS public URL.
// Falls back to ws://localhost:<port> if not set.
func wsPublicURL() string {
	cfg := getConfig()
	if cfg.WS.PublicURL != "" {
		return strings.TrimRight(cfg.WS.PublicURL, "/")
	}
	return "ws://localhost:" + cfg.WS.Port
}

// groqAPIKey returns the configured Groq API key.
func groqAPIKey() string {
	return getConfig().Groq.APIKey
}

// groqModelName returns the configured Groq model.
func groqModelName() string {
	return getConfig().Groq.Model
}

// ─── Command handler ─────────────────────────────────────────────

func handleConfigCommand(args string) string {
	if !strings.Contains(args, "show") && !strings.Contains(args, "reload") && !strings.Contains(args, "set") {
		return configHelp()
	}

	lower := strings.ToLower(strings.TrimSpace(args))
	parts := strings.Fields(lower)

	if len(parts) == 0 || parts[0] == "help" || parts[0] == "-h" {
		return configHelp()
	}

	switch parts[0] {
	case "show":
		cfg := getConfig()
		var b strings.Builder
		b.WriteString("⚙️ *Bot Config*\n\n")
		b.WriteString("*Lacak (HTTP tracker):*\n")
		b.WriteString(fmt.Sprintf("• Port: `%s`\n", cfg.Lacak.Port))
		b.WriteString(fmt.Sprintf("• Public URL: `%s`\n", lacakPublicURL()))
		b.WriteString("\n*WS (game + TTLive):*\n")
		b.WriteString(fmt.Sprintf("• Port: `%s`\n", cfg.WS.Port))
		b.WriteString(fmt.Sprintf("• Public URL: `%s`\n", wsPublicURL()))
		b.WriteString(fmt.Sprintf("• WS URL: `%s/ws`\n", wsPublicURL()))
		b.WriteString("\n*Groq AI:*\n")
		b.WriteString(fmt.Sprintf("• Model: `%s`\n", cfg.Groq.Model))
		keyPreview := cfg.Groq.APIKey
		if len(keyPreview) > 12 {
			keyPreview = keyPreview[:8] + "..." + keyPreview[len(keyPreview)-4:]
		}
		b.WriteString(fmt.Sprintf("• API Key: `%s`\n", keyPreview))
		b.WriteString(fmt.Sprintf("\n📂 Config file: `%s`", configPath()))
		return b.String()

	case "reload":
		cfg := loadConfig()
		return fmt.Sprintf("✅ Config reloaded.\n\n• Lacak port: %s\n• WS port: %s\n• Groq model: %s\n\n⚠️ Restart bot untuk apply perubahan port/URL.", cfg.Lacak.Port, cfg.WS.Port, cfg.Groq.Model)

	case "set":
		if len(parts) < 3 {
			return "❓ Format: .config set <key> <value>\n\nKeys: lacak_port, lacak_url, ws_port, ws_url, groq_key, groq_model"
		}
		key := parts[1]
		value := strings.Join(parts[2:], " ")
		// Reconstruct original value (preserve case) from args
		origArgs := strings.TrimSpace(args)
		origParts := strings.Fields(origArgs)
		if len(origParts) >= 3 {
			value = strings.Join(origParts[2:], " ")
		}

		cfg := getConfig()
		switch key {
		case "lacak_port":
			cfg.Lacak.Port = value
		case "lacak_url":
			cfg.Lacak.PublicURL = value
		case "ws_port":
			cfg.WS.Port = value
		case "ws_url":
			cfg.WS.PublicURL = value
		case "groq_key":
			cfg.Groq.APIKey = value
		case "groq_model":
			cfg.Groq.Model = value
		default:
			return fmt.Sprintf("❌ Key tidak dikenal: %s\n\nKeys: lacak_port, lacak_url, ws_port, ws_url, groq_key, groq_model", key)
		}
		if err := saveConfig(cfg); err != nil {
			return fmt.Sprintf("❌ Gagal simpan config: %v", err)
		}
		return fmt.Sprintf("✅ Config updated: %s = %s\n\n⚠️ Restart bot untuk apply perubahan port/URL.", key, value)
	}

	return configHelp()
}

func configHelp() string {
	return `⚙️ *Config Manager*

*Commands:*
• .config show → lihat config saat ini
• .config reload → reload config dari file
• .config set <key> <value> → update config

*Keys:*
• lacak_port — port lacak HTTP (default 24724)
• lacak_url — public URL lacak (default auto)
• ws_port — port WS game+TTLive (default 24848)
• ws_url — public URL WS (default auto)
• groq_key — Groq API key
• groq_model — Groq model (default openai/gpt-oss-20b)

*Config file:* data/config.json
*Env vars override:* LACAK_PORT, LACAK_PUBLIC_URL, GAME_PORT, GAME_PUBLIC_URL, GROQ_API_KEY, GROQ_MODEL

⚠️ Perubahan port/URL butuh restart bot untuk apply.`
}

// ─── Init: load config + ensure file exists ───────────────────────

func init() {
	// Load config on startup
	cfg := loadConfig()
	// Ensure config file exists (write defaults if missing)
	if _, err := os.Stat(configPath()); os.IsNotExist(err) {
		_ = saveConfig(cfg)
		fmt.Printf("[config] created default config at %s\n", configPath())
	}
	fmt.Printf("[config] loaded: lacak=%s ws=%s groq_model=%s\n", cfg.Lacak.Port, cfg.WS.Port, cfg.Groq.Model)
}
