package cli

import (
	"testing"
	"time"
)

func TestParseDarwinBoot(t *testing.T) {
	// tv_sec 1_700_000_000 = 0x6553F100, little-endian; the usec half is all
	// zero bytes, so Sysctl's trailing-NUL trim leaves only 4 or 5 bytes.
	full := string([]byte{0x00, 0xF1, 0x53, 0x65, 0, 0, 0, 0, 0x10, 0x27, 0, 0, 0, 0, 0, 0})
	trimmed := string([]byte{0x00, 0xF1, 0x53, 0x65})
	want := time.Unix(1700000000, 0)
	for name, in := range map[string]string{"full": full, "trimmed": trimmed} {
		if got := parseDarwinBoot(in); !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	for _, bad := range []string{"", string(make([]byte, 17)), string(make([]byte, 8))} {
		if got := parseDarwinBoot(bad); !got.IsZero() {
			t.Errorf("%q: got %v, want zero", bad, got)
		}
	}
}

func TestParseLinuxBtime(t *testing.T) {
	stat := []byte("cpu  1 2 3\nintr 5\nbtime 1700000000\nprocesses 9\n")
	if got := parseLinuxBtime(stat); !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("got %v", got)
	}
	for _, bad := range []string{"", "btime x\n", "cpu 1\n"} {
		if got := parseLinuxBtime([]byte(bad)); !got.IsZero() {
			t.Errorf("%q: got %v, want zero", bad, got)
		}
	}
}
