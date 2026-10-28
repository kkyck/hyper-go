package main

/**
 * 📄 envload.go — Load .env file (no external dependency)
 * ════════════════════════════════════════════════════════════
 * Membaca file .env di working directory (atau path ENV_FILE)
 * dan set ke os.Environ jika key belum ada di environment.
 *
 * Format:
 *   KEY=value
 *   KEY="value with spaces"
 *   # comment
 *
 * Owner dual:
 *   OWNER_JID=62812xxx
 *   OWNER_LID=123456789012345
 * Cukup isi salah satu atau keduanya.
 */

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func loadDotEnv() {
	path := os.Getenv("ENV_FILE")
	if path == "" {
		path = ".env"
	}
	f, err := os.Open(path)
	if err != nil {
		return // optional file
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// allow export KEY=val
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			continue
		}
		// strip quotes
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		// do not override existing env
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
			n++
		}
	}
	if n > 0 {
		fmt.Printf("📄 Loaded %d keys from %s\n", n, path)
	}
}
