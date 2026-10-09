package journal

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

var nativeRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidNative reports whether s is a canonical UUID, the only form of Claude
// Code session id that is stored or put in a shell command. The id arrives in
// a request header, so anything else could carry shell syntax.
func ValidNative(s string) bool { return nativeRE.MatchString(s) }

// LostBatch returns the sessions lost together in the newest crash: lost,
// not yet resumed, with a resume id that no open entry is running, within
// BatchWindow of the newest end, one per resume id, newest end first. Only
// sessions that ended within RecentWindow of now count: an older loss is
// most likely a tab closed on purpose.
func LostBatch(es []Entry, now time.Time) []Entry {
	running := map[string]bool{}
	for _, e := range es {
		if !e.Open() {
			continue
		}
		if e.Native != "" {
			running[e.Native] = true
		}
		if e.ResumeOf != "" {
			running[e.ResumeOf] = true
		}
	}
	var cands []Entry
	for _, e := range es {
		if e.Outcome == OutcomeLost && e.Resumed.IsZero() && ValidNative(e.ResumeID()) && !running[e.ResumeID()] && now.Sub(e.Ended) <= RecentWindow {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	newest := cands[0].Ended
	for _, e := range cands {
		if e.Ended.After(newest) {
			newest = e.Ended
		}
	}
	best := map[string]Entry{}
	for _, e := range cands {
		if newest.Sub(e.Ended) > BatchWindow {
			continue
		}
		if b, ok := best[e.ResumeID()]; !ok || e.Ended.After(b.Ended) || (e.Ended.Equal(b.Ended) && e.PID > b.PID) {
			best[e.ResumeID()] = e
		}
	}
	out := make([]Entry, 0, len(best))
	for _, e := range best {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Ended.Equal(out[b].Ended) {
			return out[a].Ended.After(out[b].Ended)
		}
		return out[a].PID > out[b].PID
	})
	return out
}

// ResumeID is the id `claude --resume` must get to reopen e's conversation.
// A session started with --resume X keeps writing X's transcript but sends a
// new native id, so X (ResumeOf) wins; a --fork-session writes a new
// transcript, so its own Native is right. A ResumeOf that is not a UUID is
// ignored.
func (e Entry) ResumeID() string {
	if !e.Fork && ValidNative(e.ResumeOf) {
		return e.ResumeOf
	}
	return e.Native
}

// Command is the shell line that resumes e in its launch directory. The
// ResumeID must be valid (ValidNative); it is not quoted.
func Command(e Entry) string {
	pool := ""
	if e.Pool != "" && e.Pool != "default" {
		pool = "CHOTTAG_POOL=" + e.Pool + " "
	}
	return "cd " + shellQuote(e.Dir) + " && " + pool + "claude --resume " + e.ResumeID()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
