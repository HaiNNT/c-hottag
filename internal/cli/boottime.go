package cli

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"time"
)

// journalBootTime is when this machine booted, the zero time when unknown. A
// package-level var so tests never read the real boot time.
var journalBootTime = realBootTime

func realBootTime() time.Time { return readBootTime() }

// parseDarwinBoot reads kern.boottime: the raw struct timeval bytes, which
// syscall.Sysctl returns as a string with trailing NULs trimmed. It pads the
// string back to 16 bytes and reads tv_sec, a little-endian int64, from the
// first 8. Anything shorter than 8 bytes or without a positive tv_sec is the
// zero time.
func parseDarwinBoot(s string) time.Time {
	if len(s) > 16 || len(s) < 1 {
		return time.Time{}
	}
	b := make([]byte, 16)
	copy(b, s)
	sec := int64(binary.LittleEndian.Uint64(b[:8]))
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// parseLinuxBtime reads the `btime N` line of /proc/stat.
func parseLinuxBtime(stat []byte) time.Time {
	for _, line := range bytes.Split(stat, []byte("\n")) {
		f := bytes.Fields(line)
		if len(f) == 2 && string(f[0]) == "btime" {
			n, err := strconv.ParseInt(string(f[1]), 10, 64)
			if err != nil || n <= 0 {
				return time.Time{}
			}
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}
