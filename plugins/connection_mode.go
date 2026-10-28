package main

// connection_mode.go — Switch between WhatsApp Web and Mobile connection mode.
//
// Commands:
//   .mode              — Show current mode
//   .mode web          — Switch to WhatsApp Web mode (Chrome browser identity)
//   .mode mobile       — Switch to Mobile mode (Android phone identity)
//
// How it works:
//   1. UPDATE whatsmeow_device SET platform = 'chrome' or 'android'
//   2. Disconnect from WA websocket
//   3. Reconnect with new platform
//   4. If WhatsApp accepts the change → bot stays authenticated
//   5. If WhatsApp requires re-pairing → generate new pairing code
//
// The platform field controls how the bot appears in WhatsApp's
// "Linked Devices" list on the user's phone:
//   - "chrome"  → Shows as "Chrome Browser"
//   - "android" → Shows as "Android Phone"

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

var modeSwitchMu sync.Mutex // prevent concurrent mode switches

// Connection mode state (persisted to PG whatsmeow_device.platform)
type ConnectionMode string

const (
	ModeWeb    ConnectionMode = "chrome"  // web mode (use .mode web)
	ModeMobile ConnectionMode = "android" // DEFAULT: mobile
)

// getCurrentMode reads the platform from whatsmeow_device table
func getCurrentMode() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := pgGet()
	if err != nil {
		return "unknown"
	}
	var platform string
	_ = pgQueryRow(ctx, db, `SELECT platform FROM whatsmeow_device LIMIT 1`).Scan(&platform)
	if platform == "" {
		return "chrome (default)"
	}
	return platform
}

// setPlatform updates whatsmeow_device.platform in PG
func setPlatform(platform string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := pgGet()
	if err != nil {
		return fmt.Errorf("pg: %w", err)
	}
	_, err = pgExec(ctx, db, `UPDATE whatsmeow_device SET platform = ?`, platform)
	return err
}

// switchMode disconnects, updates platform, reconnects
func switchMode(client *whatsmeow.Client, platform string) error {
	modeSwitchMu.Lock()
	defer modeSwitchMu.Unlock()
	// 1. Update platform in PG
	if err := setPlatform(platform); err != nil {
		return fmt.Errorf("update platform: %w", err)
	}

	// 2. Update client's device store (in-memory)
	if client != nil && client.Store != nil {
		client.Store.Platform = platform
	}

	// 3. Disconnect
	if client != nil {
		client.Disconnect()
		time.Sleep(2 * time.Second)
	}

	// 4. Reconnect (will use new platform from store)
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := client.ConnectContext(ctx)
		if err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
	}

	return nil
}

func handleModeCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	sub := strings.TrimSpace(strings.ToLower(args))
	current := getCurrentMode()

	if sub == "" {
		// Show current mode
		return fmt.Sprintf("📱 *Connection Mode*\n\nCurrent: `%s`\n\nSwitch:\n• `.mode web` — Chrome browser\n• `.mode mobile` — Android phone", current)
	}

	var newPlatform string
	var displayName string
	switch sub {
	case "web", "chrome", "browser":
		newPlatform = string(ModeWeb)
		displayName = "WhatsApp Web (Chrome)"
	case "mobile", "android", "phone":
		newPlatform = string(ModeMobile)
		displayName = "Mobile (Android)"
	default:
		return "❓ Mode tidak dikenal.\n\nGunakan:\n• `.mode web` — Chrome browser\n• `.mode mobile` — Android phone"
	}

	// Check if already in this mode
	if strings.HasPrefix(current, newPlatform) {
		return fmt.Sprintf("ℹ️ Sudah dalam mode %s", displayName)
	}

	// Switch mode
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[mode] panic: %v\n", r)
			}
		}()

		fmt.Printf("[mode] Switching to %s...\n", displayName)
		err := switchMode(client, newPlatform)
		if err != nil {
			fmt.Printf("[mode] Switch failed: %v\n", err)
			// Try to reconnect anyway
			if client != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = client.ConnectContext(ctx)
			}
		} else {
			fmt.Printf("[mode] Switched to %s successfully\n", displayName)
		}
	}()

	return fmt.Sprintf("🔄 Switching to *%s*...\n\nBot akan disconnect + reconnect dengan platform baru.\nKalau WhatsApp minta re-pair, request pairing code:\n`curl http://localhost:3000/api/paircode?phone=<nomor>`", displayName)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "mode",
		Description: "Switch connection mode (web/mobile)",
		Category:    "Core",
		OwnerOnly:   true,
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleModeCommand(client, msg, args)
		},
	})
}
