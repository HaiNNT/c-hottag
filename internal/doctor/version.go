package doctor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/status"
)

// installedVersionRe takes Claude Code's version from the first line of
// `claude --version` ("2.1.282 (Claude Code)"). Each part is bounded to
// 1-6 digits, so unrelated output that happens to start with a long run of
// digits (never a real Claude Code version) does not get misread as one.
// The trailing (?:[^0-9]|$) requires the last part to actually end there
// (a non-digit or the end of the string): without it, a 7+ digit last part
// like "2.1.1234567" would still match by silently truncating to its first
// six digits, "2.1.123456".
var installedVersionRe = regexp.MustCompile(`^([0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6})(?:[^0-9]|$)`)

const versionDriftHint = "chottag trace on"

// versionDriftCheck is row 15 (M2c spec §5): the installed Claude Code
// against the last traced one. Never a problem (T8): Claude Code updates
// often, and an exit 3 after each one would teach users to ignore doctor.
func versionDriftCheck() Check {
	return Check{ID: "version-drift", Detect: func(e *Env) Finding {
		var lt *status.TracedVersion
		if v, ok := loadStatus(e).LastTraced(); ok {
			lt = &v
		}
		installed, ok, err := installedVersion(e)
		switch {
		case err != nil:
			return Internal(err)
		case !ok:
			return Finding{Status: StatusInfo, Detail: "could not read the installed version", LastTraced: lt}
		case lt == nil:
			return Finding{Status: StatusInfo, Detail: "no trace recorded on this machine", Hint: versionDriftHint, Installed: installed}
		case lt.ClaudeVersion == installed:
			return Finding{Status: StatusOK, Detail: fmt.Sprintf("Claude Code %s was traced on %s", installed, lt.At.Local().Format("Jan 2 15:04")), Installed: installed, LastTraced: lt}
		}
		return Finding{
			Status:     StatusInfo,
			Detail:     fmt.Sprintf("Claude Code %s has not been traced (last traced: %s at %s)", installed, lt.ClaudeVersion, lt.At.Local().Format("Jan 2 15:04")),
			Hint:       versionDriftHint,
			Installed:  installed,
			LastTraced: lt,
		}
	}}
}

// installedVersion runs --version on row 8's resolved real claude. ok is
// false when there is no real claude, the run fails or times out, or the
// first line doesn't start with a version. err is only an unreadable
// state.json, which every state row reports as internal.
func installedVersion(e *Env) (version string, ok bool, err error) {
	st, err := e.State()
	if err != nil {
		return "", false, err
	}
	bin, err := e.ResolveClaude(e.PATH, e.BinDir(), st.RealClaude)
	if err != nil {
		return "", false, nil
	}
	out, err := e.ClaudeVersion(bin)
	if err != nil {
		return "", false, nil
	}
	first, _, _ := strings.Cut(out, "\n")
	m := installedVersionRe.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return "", false, nil
	}
	return m[1], true, nil
}
