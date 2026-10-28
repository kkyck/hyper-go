//go:build !linux && !darwin

package main

// Stub for non-Unix platforms (Windows etc.).
func getRusageCPUTimeMs() (int64, int64) {
	return 0, 0
}
