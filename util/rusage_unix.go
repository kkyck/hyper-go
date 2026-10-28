//go:build linux || darwin

package main

import (
	"syscall"
	"time"
)

// getRusageCPUTimeMs returns (userMs, sysMs) for the current process.
// Linux/Darwin: syscall.Getrusage with RUSAGE_SELF.
func getRusageCPUTimeMs() (int64, int64) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	userMs := int64(ru.Utime.Sec)*1000 + int64(ru.Utime.Usec)/1000
	sysMs := int64(ru.Stime.Sec)*1000 + int64(ru.Stime.Usec)/1000
	_ = time.Now // keep time imported
	return userMs, sysMs
}
