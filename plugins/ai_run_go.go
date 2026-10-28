package main

// ai_run_go.go — AI tool to run arbitrary Go code on the server
// ════════════════════════════════════════════════════════════
//
// v2.85 — Expands AI capability: AI can now write + run Go code on-the-fly.
//
// USE CASES (AI self-expanding capability):
//   - Complex math/calculations (BigRat, matrix, statistics)
//   - Data processing (CSV/JSON parsing, filtering, sorting)
//   - Custom algorithms (graph traversal, compression, encryption)
//   - HTTP requests with custom logic (concurrent fetch + merge)
//   - Date/time calculations, calendar logic
//   - Generate random data (UUIDs, passwords, tokens)
//   - File parsing (regex extraction, format conversion)
//
// SECURITY:
//   - 30s execution timeout (context.WithTimeout)
//   - Output truncated to 8KB (stdout) + 3KB (stderr)
//   - Code runs in isolated /tmp directory (cleaned up after)
//   - GOPATH isolated to /tmp/ai_gopath (doesn't touch bot's modules)
//   - GOTOOLCHAIN=local (prevents auto-download of Go versions)
//   - GOFLAGS=-mod=mod (allows fetching missing deps if needed)
//
// HOW IT WORKS:
//   1. AI writes Go code in tool args
//   2. Bot writes code to /tmp/ai_go_XXX/main.go
//   3. Bot runs `go run main.go` with 30s timeout
//   4. Captures stdout + stderr
//   5. Returns formatted result to AI
//   6. AI uses result for next decision (can chain more tools)
//
// REQUIREMENTS:
//   - Go must be installed on the server (checked at runtime)
//   - If Go missing, returns clear error message
//   - Internet access available (for go mod download if needed)
//
// EXAMPLE AI USAGE:
//   User: "hitung faktorial 100"
//   AI: <tool>run_go_code
//        package main
//        import ("fmt"; "math/big")
//        func main() {
//            n := big.NewInt(1)
//            for i := int64(2); i <= 100; i++ {
//                n.Mul(n, big.NewInt(i))
//            }
//            fmt.Println("100! =", n)
//        }
//        </tool>
//   Bot runs code → returns "100! = 933262154439441526816992388562667..."
//   AI: "100! itu angka raksasa: 93326215443944152681699238856266700490715..."

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// aiFindGoBinary locates the Go binary on the server.
// Checks in order:
//  1. PATH (exec.LookPath)
//  2. $HOME/.local/go/bin/go (common for non-root installs)
//  3. $HOME/go/bin/go (GOPATH-style)
//  4. /usr/local/go/bin/go (system install)
//  5. /usr/bin/go (package manager install)
//  6. /opt/go/bin/go
//
// Returns the path to go binary, or "" if not found.
func aiFindGoBinary() string {
	// 1. Check PATH
	if goPath, err := exec.LookPath("go"); err == nil {
		return goPath
	}

	// 2-6. Check common install locations
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "go", "bin", "go"),
		filepath.Join(home, "go", "bin", "go"),
		"/usr/local/go/bin/go",
		"/usr/bin/go",
		"/opt/go/bin/go",
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			// Check if executable
			if info.Mode().Perm()&0111 != 0 {
				return p
			}
		}
	}
	return ""
}

// aiRunGoCode writes Go source code to a temp file and runs it with `go run`.
// Returns formatted stdout + stderr + execution status.
func aiRunGoCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "Error: code kosong. Tulis kode Go lengkap (package main + func main)."
	}

	// Validate basic structure
	if !strings.Contains(code, "package main") {
		return "Error: kode harus punya 'package main' di baris pertama"
	}
	if !strings.Contains(code, "func main()") {
		return "Error: kode harus punya 'func main()' sebagai entry point"
	}

	// Find Go binary (auto-detect in common locations)
	goPath := aiFindGoBinary()
	if goPath == "" {
		home, _ := os.UserHomeDir()
		return fmt.Sprintf(`Error: Go compiler tidak ditemukan di server.

Cek lokasi:
  - PATH (go)
  - %s/.local/go/bin/go
  - %s/go/bin/go
  - /usr/local/go/bin/go
  - /usr/bin/go
  - /opt/go/bin/go

Cara install Go (no sudo needed):
  .shell bash install_go.sh

Atau manual:
  .shell wget https://go.dev/dl/go1.27.1.linux-arm64.tar.gz
  .shell mkdir -p ~/.local
  .shell tar -xzf go1.27.1.linux-arm64.tar.gz -C ~/.local
  .shell mv ~/.local/go ~/.local/go

Setelah install, jalankan: .restart`, home, home)
	}

	// Create isolated temp directory for the code
	tmpDir, err := os.MkdirTemp("", "ai_go_")
	if err != nil {
		return fmt.Sprintf("Error membuat temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir) // cleanup

	// Write the code to main.go
	mainFile := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(mainFile, []byte(code), 0644); err != nil {
		return fmt.Sprintf("Error menulis main.go: %v", err)
	}

	// Create isolated GOPATH in /tmp (don't touch bot's own modules)
	gopathDir, err := os.MkdirTemp("", "ai_gopath_")
	if err != nil {
		return fmt.Sprintf("Error membuat GOPATH: %v", err)
	}
	defer os.RemoveAll(gopathDir)

	// Set up environment for isolated execution
	// - GOTOOLCHAIN=local: don't try to download other Go versions
	// - GOFLAGS=-mod=mod: allow fetching missing deps
	// - GOPATH: isolated cache
	// - GOOS/GOARCH: match server (so binary runs here)
	env := append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
		"GOPATH="+gopathDir,
		"GOCACHE="+filepath.Join(gopathDir, "go-build"),
		"GOOS="+runtime.GOOS,
		"GOARCH="+runtime.GOARCH,
		"CGO_ENABLED=0", // pure Go, no C deps (faster, no compiler needed)
	)

	// Run with 30s timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, goPath, "run", mainFile)
	cmd.Dir = tmpDir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	startTime := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(startTime)

	// Build result
	var result strings.Builder
	result.WriteString(fmt.Sprintf("⏱️ Execution time: %v\n", elapsed.Truncate(time.Millisecond)))
	result.WriteString(fmt.Sprintf("🖥️ Server: %s/%s\n\n", runtime.GOOS, runtime.GOARCH))

	// Stdout (truncated to 8KB)
	out := stdout.String()
	if len(out) > 8192 {
		out = out[:8192] + fmt.Sprintf("\n... (truncated, total %d bytes)", len(stdout.String()))
	}
	if out != "" {
		result.WriteString("📤 STDOUT:\n")
		result.WriteString(out)
		result.WriteString("\n")
	} else {
		result.WriteString("📤 STDOUT: (empty)\n")
	}

	// Stderr (truncated to 3KB)
	errOut := stderr.String()
	if len(errOut) > 3072 {
		errOut = errOut[:3072] + fmt.Sprintf("\n... (truncated, total %d bytes)", len(stderr.String()))
	}
	if errOut != "" {
		result.WriteString("\n⚠️ STDERR:\n")
		result.WriteString(errOut)
		result.WriteString("\n")
	}

	// Error / status
	if ctx.Err() == context.DeadlineExceeded {
		result.WriteString("\n❌ TIMEOUT: 30 detik terlewati. Kode terlalu lambat atau infinite loop.\n")
	} else if runErr != nil {
		// Check if it's a compilation error or runtime error
		errStr := runErr.Error()
		if strings.Contains(errStr, "exit status 2") {
			result.WriteString("\n❌ COMPILE/RUNTIME ERROR: ")
		} else {
			result.WriteString("\n❌ ERROR: ")
		}
		result.WriteString(errStr)
		result.WriteString("\n")
	} else {
		result.WriteString("\n✅ SUCCESS (exit 0)\n")
	}

	return result.String()
}

// aiRunGoCodeQuick is a simplified variant that returns only stdout (for AI chaining).
// Useful when AI just needs the result, not the debug info.
func aiRunGoCodeQuick(code string) string {
	code = strings.TrimSpace(code)
	if !strings.Contains(code, "package main") || !strings.Contains(code, "func main()") {
		return "Error: kode harus punya 'package main' + 'func main()'"
	}

	goPath := aiFindGoBinary()
	if goPath == "" {
		return "Error: Go compiler tidak ditemukan. Install via: .shell bash install_go.sh"
	}

	tmpDir, err := os.MkdirTemp("", "ai_goq_")
	if err != nil {
		return fmt.Sprintf("Error: gagal buat temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	mainFile := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(mainFile, []byte(code), 0644); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	gopathDir, err := os.MkdirTemp("", "ai_gopathq_")
	if err != nil {
		return fmt.Sprintf("Error: gagal buat gopath temp dir: %v", err)
	}
	defer os.RemoveAll(gopathDir)

	env := append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOPATH="+gopathDir,
		"GOCACHE="+filepath.Join(gopathDir, "go-build"),
		"CGO_ENABLED=0",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, goPath, "run", mainFile)
	cmd.Dir = tmpDir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "TIMEOUT: 30s exceeded"
	}
	if err != nil {
		// Return stderr (helpful for AI to fix code)
		errOut := stderr.String()
		if len(errOut) > 2000 {
			errOut = errOut[:2000] + "..."
		}
		return fmt.Sprintf("ERROR: %s\nSTDERR: %s", err.Error(), errOut)
	}

	out := stdout.String()
	if len(out) > 5000 {
		out = out[:5000] + "... (truncated)"
	}
	return out
}

// aiGenID generates a random hex ID (for temp file naming, not exposed as AI tool).
func aiGenID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
