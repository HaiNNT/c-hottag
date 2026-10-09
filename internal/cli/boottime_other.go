//go:build !darwin

package cli

import (
	"os"
	"runtime"
	"time"
)

// readBootTime reads `btime` from /proc/stat on linux; elsewhere, or on any
// error, the zero time.
func readBootTime() time.Time {
	if runtime.GOOS != "linux" {
		return time.Time{}
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	return parseLinuxBtime(b)
}
