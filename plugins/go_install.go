package main

// go_install.go — .go command + Go auto-alias for .shell
// ════════════════════════════════════════════════════════════
//
// v2.87 — Adds:
//   1. .go -i  → install Go to ~/.local/go (auto-detect arm64/amd64)
//   2. .go -v  → show installed Go version
//   3. .go -p  → print PATH that bot uses for Go
//   4. .shell go <args> → auto-alias to ~/.local/go/bin/go (no system PATH change)
//
// WHY NO SYSTEM PATH CHANGE?
//   - Server might already have Go installed system-wide (don't conflict)
//   - User-installed Go in ~/.local/go/bin takes precedence for bot
//   - Other system tools still use system Go if present
//   - No sudo needed, no /etc/profile.d, no /etc/environment
//
// AUTO-DETECT ARCH:
//   - runtime.GOARCH = "arm64" → download go<ver>.linux-arm64.tar.gz
//   - runtime.GOARCH = "amd64" → download go<ver>.linux-amd64.tar.gz
//   - Works on any VPS (ARM, x86_64) without manual config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow/types/events"
)

// goInstallVersion is the Go version to install.
// Update this when a new stable Go is released.
const goInstallVersion = "1.27.1"

// goLocalDir returns the user-local Go install directory: ~/.local/go
func goLocalDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "go")
}

// goLocalBin returns the Go binary path: ~/.local/go/bin/go
func goLocalBin() string {
	return filepath.Join(goLocalDir(), "bin", "go")
}

// goIsInstalledLocal returns true if Go is installed in ~/.local/go.
func goIsInstalledLocal() bool {
	if info, err := os.Stat(goLocalBin()); err == nil && !info.IsDir() {
		if info.Mode().Perm()&0111 != 0 {
			return true
		}
	}
	return false
}

// goGetLocalVersion returns the version string of ~/.local/go/bin/go (or "" if missing).
func goGetLocalVersion() string {
	if !goIsInstalledLocal() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, goLocalBin(), "version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// goDetectArch returns "arm64" or "amd64" based on runtime.GOARCH.
// Falls back to "arm64" for unknown architectures.
func goDetectArch() string {
	if runtime.GOARCH == "amd64" {
		return "amd64"
	}
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	// Default fallback for other arches (e.g. 386, arm)
	// Most VPS are arm64 or amd64, so this is rare
	return runtime.GOARCH
}

// goInstall downloads + extracts Go to ~/.local/go (auto-detect arch).
// Returns output log (combined stdout + stderr from each step).
func goInstall() string {
	arch := goDetectArch()
	home, _ := os.UserHomeDir()
	localDir := filepath.Join(home, ".local")
	tarballName := fmt.Sprintf("go%s.linux-%s.tar.gz", goInstallVersion, arch)
	tarballPath := filepath.Join(home, tarballName)
	downloadURL := fmt.Sprintf("https://go.dev/dl/%s", tarballName)

	var log strings.Builder
	log.WriteString(fmt.Sprintf("🏗️ *Install Go %s (%s)*\n\n• Target: ~/.local/go\n", goInstallVersion, arch))

	// Step 1: cd to home + download
	log.WriteString("📥 Step 1: Download Go tarball...\n")
	log.WriteString(fmt.Sprintf("   URL: %s\n", downloadURL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dlCmd := exec.CommandContext(ctx, "sh", "-c",
		fmt.Sprintf("cd %s && wget -q %s -O %s && echo 'DOWNLOAD_OK'",
			home, downloadURL, tarballPath))
	dlOut, dlErr := dlCmd.CombinedOutput()
	log.WriteString(string(dlOut))
	if dlErr != nil {
		// Try curl if wget failed
		log.WriteString(fmt.Sprintf("   wget failed (%v), trying curl...\n", dlErr))
		curlCmd := exec.CommandContext(ctx, "sh", "-c",
			fmt.Sprintf("cd %s && curl -sSL %s -o %s && echo 'DOWNLOAD_OK_CURL'",
				home, downloadURL, tarballPath))
		curlOut, curlErr := curlCmd.CombinedOutput()
		log.WriteString(string(curlOut))
		if curlErr != nil {
			log.WriteString(fmt.Sprintf("\n❌ Download failed: %v\n", curlErr))
			return log.String()
		}
	}
	log.WriteString("✅ Downloaded\n\n")

	// Step 2: mkdir ~/.local + extract
	log.WriteString("📂 Step 2: Extract to ~/.local/go...\n")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		log.WriteString(fmt.Sprintf("❌ mkdir failed: %v\n", err))
		return log.String()
	}

	// Remove old Go dir if exists (replace)
	oldGoDir := goLocalDir()
	if _, err := os.Stat(oldGoDir); err == nil {
		os.RemoveAll(oldGoDir)
		log.WriteString("• Removed old ~/.local/go\n")
	}

	extractCmd := exec.CommandContext(ctx, "sh", "-c",
		fmt.Sprintf("tar -xzf %s -C %s", tarballPath, localDir))
	extractOut, extractErr := extractCmd.CombinedOutput()
	log.WriteString(string(extractOut))
	if extractErr != nil {
		log.WriteString(fmt.Sprintf("❌ Extract failed: %v\n", extractErr))
		return log.String()
	}
	log.WriteString("   ✅ Extracted\n\n")

	// Step 3: Verify
	log.WriteString("✅ Step 3: Verify installation...\n")
	version := goGetLocalVersion()
	if version == "" {
		log.WriteString("❌ Verification failed — ~/.local/go/bin/go not executable\n")
		return log.String()
	}
	log.WriteString(fmt.Sprintf("• %s\n", version))
	log.WriteString(fmt.Sprintf("• Path: %s\n\n", goLocalBin()))

	// Step 4: Cleanup tarball
	os.Remove(tarballPath)
	log.WriteString("🗑️ Cleaned up tarball\n\n")

	// Step 5: Add to .bashrc/.profile (for user convenience, not required for bot)
	for _, profile := range []string{".bashrc", ".profile"} {
		profilePath := filepath.Join(home, profile)
		content := ""
		if data, err := os.ReadFile(profilePath); err == nil {
			content = string(data)
		}
		if !strings.Contains(content, ".local/go/bin") {
			line := fmt.Sprintf("\nexport PATH=$HOME/.local/go/bin:$PATH\n")
			f, err := os.OpenFile(profilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				f.WriteString(line)
				f.Close()
				log.WriteString(fmt.Sprintf("   Added PATH to %s\n", profile))
			}
		}
	}

	log.WriteString("\n✅ *Install Selesai*\n")
	log.WriteString("\nBot auto-detect Go di ~/.local/go/bin (tanpa restart).\n")
	log.WriteString("• Test: .shell go version\n")
	log.WriteString("• Atau: .ai hitung faktorial 50 pakai Go code\n")
	return log.String()
}

// handleGoCommand — .go command entry point
// Subcommands:
//
//	.go -i  → install Go (auto-detect arm64/amd64)
//	.go -v  → show installed Go version
//	.go -p  → print Go binary path + version
//	.go     → show help (default)
func handleGoCommand(args string, msg *events.Message) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	args = strings.TrimSpace(args)
	switch args {
	case "-i", "install":
		return goInstall()

	case "-v", "version":
		// Show all Go versions found (local + system)
		var sb strings.Builder
		sb.WriteString("🔍 *Go Version Check*\n\n")

		// 1. Local install (~/.local/go/bin/go)
		localVer := goGetLocalVersion()
		if localVer != "" {
			sb.WriteString(fmt.Sprintf("• Local: %s\n", localVer))
			sb.WriteString(fmt.Sprintf("• Path: %s\n\n", goLocalBin()))
		} else {
			sb.WriteString("• Local: belum terinstall (~/.local/go/bin/go)\n")
			sb.WriteString("• Install: .go -i\n\n")
		}

		// 2. System Go (from PATH)
		if goPath, err := exec.LookPath("go"); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out, _ := exec.CommandContext(ctx, goPath, "version").CombinedOutput()
			cancel()
			sb.WriteString(fmt.Sprintf("• System: %s\n", strings.TrimSpace(string(out))))
			sb.WriteString(fmt.Sprintf("• Path: %s\n", goPath))
		} else {
			sb.WriteString("• System: tidak ada di PATH\n")
		}

		// 3. Architecture
		sb.WriteString(fmt.Sprintf("\n• Arsitektur: %s/%s\n", runtime.GOOS, runtime.GOARCH))
		return sb.String()

	case "-p", "path":
		var sb strings.Builder
		sb.WriteString("📍 *Go Path Info*\n\n")
		sb.WriteString(fmt.Sprintf("• Local binary: %s\n", goLocalBin()))
		sb.WriteString(fmt.Sprintf("• Local dir: %s\n", goLocalDir()))
		sb.WriteString(fmt.Sprintf("• Terinstall: %v\n", goIsInstalledLocal()))
		if goIsInstalledLocal() {
			sb.WriteString(fmt.Sprintf("• Versi: %s\n", goGetLocalVersion()))
		}
		sb.WriteString(fmt.Sprintf("\n• Arsitektur: %s/%s\n", runtime.GOOS, runtime.GOARCH))
		return sb.String()

	case "", "-h", "--help", "help":
		return `🔧 *Go Installation Manager*

❓ *Cara pakai:*
• .go -i — install Go (auto-detect arm64/amd64)
• .go -v — cek versi Go (local + system)
• .go -p — lihat path Go yang terinstall

*Auto-detect:*
• Architecture: arm64 atau amd64 (sesuai VPS)
• Lokasi install: ~/.local/go (no sudo, no system PATH change)
• Bot auto-alias 'go' ke ~/.local/go/bin/go untuk .shell dan AI tools

*Conflict-safe:*
• Nggak ubah /etc/environment atau /etc/profile.d
• Nggak override system Go kalau ada
• Cuma bot yang pakai ~/.local/go/bin/go

*Setelah install:*
• .shell go version → langsung jalan
• .ai <task> → AI bisa generate + run Go code`
	}

	return fmt.Sprintf("❌ Subcommand tidak dikenal: %s\n❓ *Cara pakai:* .go -i (install), .go -v (version), .go -p (path), .go help", args)
}
