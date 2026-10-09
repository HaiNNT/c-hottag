package cli

import (
	"syscall"
	"time"
)

// readBootTime reads kern.boottime; any error is the zero time.
func readBootTime() time.Time {
	s, err := syscall.Sysctl("kern.boottime")
	if err != nil {
		return time.Time{}
	}
	return parseDarwinBoot(s)
}
