package shim

import (
	"slices"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/journal"
)

// resumeArg returns the value of --resume X, --resume=X, -r X or -r=X in
// args (argv after argv0), or "" when absent or the value starts with "-".
func resumeArg(args []string) string {
	for i, a := range args {
		var v string
		switch {
		case a == "--resume" || a == "-r":
			if i+1 >= len(args) {
				return ""
			}
			v = args[i+1]
		case strings.HasPrefix(a, "--resume="):
			v = strings.TrimPrefix(a, "--resume=")
		case strings.HasPrefix(a, "-r="):
			v = strings.TrimPrefix(a, "-r=")
		default:
			continue
		}
		if strings.HasPrefix(v, "-") {
			return ""
		}
		return v
	}
	return ""
}

// journalEntry builds the journal record for a launch. Pool is set only with
// a sid, mirroring the registry entry.
func journalEntry(pid, ppid int, started time.Time, sid, pool, dir string, env, args []string) journal.Entry {
	e := journal.Entry{
		PID: pid, PPID: ppid, Started: started, SID: sid, Dir: dir,
		CmuxWorkspace: envGet(env, "CMUX_WORKSPACE_ID"),
		CmuxSurface:   envGet(env, "CMUX_SURFACE_ID"),
		ResumeOf:      resumeArg(args),
		Fork:          slices.Contains(args, "--fork-session"),
	}
	if sid != "" {
		e.Pool = pool
	}
	return e
}
