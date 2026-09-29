package consistency

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// TestGettingStartedCoversEveryInstallPath: install.sh from a clone or
// through gh, by hand from a release, the plugin, the first accounts.
func TestGettingStartedCoversEveryInstallPath(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	requirePhrases(t, "docs/getting-started.md",
		"gh repo clone "+repo, "./install.sh", "--version", "--repo",
		"repos/"+repo+"/contents/install.sh",
		"gh release download", "shasum -a 256 -c checksums.txt", "gh attestation verify",
		"chottag setup", "CHOTTAG_HOME", "claude plugin install chottag@c-hottag",
		"chottag login", "chottag status", "chottag doctor",
	)
}

// autoDur writes a duration the way the docs do: 30m, 3h, 15m, 24h.
func autoDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// TestAutoSwitchDocMatchesTheDefaults (P3-R1): the switch-point table and
// the settings table give internal/autoswitch's real defaults and ranges.
func TestAutoSwitchDocMatchesTheDefaults(t *testing.T) {
	const page = "docs/auto-switch.md"
	md := read(t, page)
	values := map[string]string{}
	for _, title := range []string{"Switch points", "Settings"} {
		s, ok := doccheck.Find(md, 2, title)
		if !ok {
			t.Fatalf("%s has no ## %s", page, title)
		}
		for _, row := range tableRows(s.Lead) {
			if len(row) >= 2 {
				values[strings.Trim(row[0], "`")] = strings.Trim(row[1], "`")
			}
		}
	}
	bal := autoswitch.Preset(autoswitch.ModeBalanced)
	for _, key := range autoswitch.SettingKeys() {
		got, ok := values[key]
		if !ok {
			t.Errorf("%s has no row for the setting %s", page, key)
			continue
		}
		var want string
		switch key {
		case "hold5h":
			want = autoDur(bal.Hold5h)
		case "hold7d":
			want = autoDur(bal.Hold7d)
		case "cooldown":
			want = autoDur(bal.Cooldown)
		default:
			w, tier, _ := strings.Cut(key, ".")
			want = strconv.Itoa(autoswitch.DefaultSwitchPoint(autoswitch.Window(w), autoswitch.Tier(tier)))
		}
		if got != want {
			t.Errorf("%s: %s is %q, want %q (balanced default)", page, key, got, want)
		}
	}
	requirePhrases(t, page,
		"`"+string(autoswitch.ModeBalanced)+"`", "`"+string(autoswitch.ModeCacheOptimize)+"`",
		strconv.Itoa(autoswitch.MinSwitchPoint), strconv.Itoa(autoswitch.MaxSwitchPoint),
		autoDur(autoswitch.MaxDuration), "chottag auto set", "chottag auto reset", "chottag plan",
		"Resend your last message",
	)
}
