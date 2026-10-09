// Package sessname names Claude Code sessions (R171). It reads a checkout's
// branch from files, never runs git, and reads only the ai-title and
// custom-title records of a session transcript. It never logs or stores a
// title: the only thing kept per session is a sha256 of the name chottag set.
package sessname

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

const (
	// HeadBytes is how much of a transcript's start is scanned: the title
	// comes right after the first prompt.
	HeadBytes = 4 << 20
	// TailBytes is how much of its end is scanned too, since a /rename can
	// come late.
	TailBytes = 1 << 20
	// MaxRunes caps a name.
	MaxRunes = 80
	sep      = " · "
)

var idRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidSessionID reports whether s is a UUID, the only form that may become a
// file name here.
func ValidSessionID(s string) bool { return idRE.MatchString(s) }

// readSmall reads at most 4 KiB of path, only when it is a regular file
// (never a FIFO or device, which could block).
func readSmall(path string) ([]byte, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	return b, err == nil
}

// BranchBase is the last path segment of the branch checked out at cwd (or
// the nearest parent that is a checkout), read from .git files. It is "" for
// main, master, a detached HEAD, or no git.
func BranchBase(cwd string) string {
	dir := filepath.Clean(cwd)
	for {
		if head := headFile(dir); head != "" {
			b, ok := readSmall(head)
			if !ok {
				return ""
			}
			ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
			if !ok {
				return ""
			}
			if i := strings.LastIndex(ref, "/"); i >= 0 {
				ref = ref[i+1:]
			}
			if ref == "main" || ref == "master" {
				return ""
			}
			return ref
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// headFile returns the HEAD file of the checkout at dir, "" if dir is none.
func headFile(dir string) string {
	g := filepath.Join(dir, ".git")
	fi, err := os.Stat(g)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return filepath.Join(g, "HEAD")
	}
	b, ok := readSmall(g)
	if !ok {
		return ""
	}
	p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Join(p, "HEAD")
}

// Titles returns the last ai-title and the last custom-title found in the
// first head and last tail bytes of the transcript at path. A missing file,
// bad lines or other fields give empty strings. Nothing else is read.
func Titles(path string, head, tail int64) (ai, custom string) {
	if sfi, err := os.Stat(path); err != nil || !sfi.Mode().IsRegular() {
		return "", ""
	}
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", ""
	}
	size := fi.Size()
	scan := func(off, n int64, dropFirst bool) {
		if n > size-off {
			n = size - off
		}
		if n <= 0 {
			return
		}
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			return
		}
		if dropFirst {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				return
			}
		}
		for _, line := range bytes.Split(buf, []byte{'\n'}) {
			isAI := bytes.Contains(line, []byte(`"ai-title"`))
			isCustom := bytes.Contains(line, []byte(`"custom-title"`))
			if !isAI && !isCustom {
				continue
			}
			var rec struct {
				Type        string `json:"type"`
				AITitle     string `json:"aiTitle"`
				CustomTitle string `json:"customTitle"`
			}
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			switch rec.Type {
			case "ai-title":
				if rec.AITitle != "" {
					ai = rec.AITitle
				}
			case "custom-title":
				if rec.CustomTitle != "" {
					custom = rec.CustomTitle
				}
			}
		}
	}
	scan(0, head, false)
	if size > head {
		start := size - tail
		if start < head {
			start = head
		}
		// Back up one byte so a line starting exactly at start is whole.
		scan(start-1, size-start+1, true)
	}
	return ai, custom
}

// Title is the live title of a transcript: custom-title wins, else ai-title.
func Title(path string) string {
	ai, custom := Titles(path, HeadBytes, TailBytes)
	if custom != "" {
		return custom
	}
	return ai
}

// Sanitize drops control and format characters (line breaks and tabs become
// spaces), trims, and caps the result at MaxRunes.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if rs := []rune(out); len(rs) > MaxRunes {
		out = strings.TrimSpace(string(rs[:MaxRunes]))
	}
	return out
}

// Compose joins a base name and an optional title.
func Compose(base, title string) string {
	if title == "" {
		return Sanitize(base)
	}
	return Sanitize(base + sep + title)
}

// HashOf is the hex sha256 of a name.
func HashOf(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:])
}

// State is what chottag keeps per session: the hash of the last name it set,
// and whether it is finished with the session.
type State struct {
	Hash string `json:"sha256,omitempty"`
	Done bool   `json:"done,omitempty"`
}

func statePath(home, id string) string {
	return filepath.Join(home, "sessions", "names", id)
}

// LoadState reads a session's state; ok is false when there is none (or the
// id is not a UUID).
func LoadState(home, id string) (State, bool) {
	if !ValidSessionID(id) {
		return State{}, false
	}
	b, err := os.ReadFile(statePath(home, id))
	if err != nil {
		return State{}, false
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return State{}, false
	}
	return s, true
}

// SaveState writes a session's state, refusing an id that is not a UUID.
func SaveState(home, id string, s State) error {
	if !ValidSessionID(id) {
		return os.ErrInvalid
	}
	dir := filepath.Dir(statePath(home, id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(statePath(home, id), b, 0o600)
}

// TranscriptAllowed reports whether path is an existing file under
// <configDir>/projects/, after symlinks resolve.
func TranscriptAllowed(path, configDir string) bool {
	if path == "" || !filepath.IsAbs(path) || configDir == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(filepath.Join(configDir, "projects"))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, real)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// FindTranscript locates <configDir>/projects/*/<id>.jsonl, "" if none. The
// star is a glob because Claude Code truncates and hashes long folder keys.
func FindTranscript(configDir, id string) string {
	if !ValidSessionID(id) || configDir == "" {
		return ""
	}
	m, err := filepath.Glob(filepath.Join(configDir, "projects", "*", id+".jsonl"))
	if err != nil || len(m) == 0 {
		return ""
	}
	for _, p := range m {
		if TranscriptAllowed(p, configDir) {
			return p
		}
	}
	return ""
}

// Input is the hook's JSON, reduced to what naming uses.
type Input struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source"`
	SessionTitle   string `json:"session_title"`
	Prompt         string `json:"prompt"`
}

// Env is what Decide needs from outside.
type Env struct {
	Home      string           // $CHOTTAG_HOME
	ConfigDir string           // Claude's config dir
	Now       func() time.Time // for the <folder> HH:MM fallback
	// Model, when set (CHOTTAG_NAME_SESSIONS=model, R172), makes a topic
	// from the first prompt; "" means none. Called at most once, only for
	// a session's first name with no branch and no generated title.
	Model func(prompt string) string
}

// Decide returns the name to set, and whether to print it. A failure to
// store state gives no name: it is better to name nothing than to repeat.
func Decide(in Input, env Env) (string, bool) {
	// Only a UserPromptSubmit name reaches Claude Code's running-session
	// listing; a SessionStart one reaches the transcript alone (F280).
	if in.Event != "UserPromptSubmit" {
		return "", false
	}
	if !ValidSessionID(in.SessionID) {
		return "", false
	}
	if in.Cwd == "" || !filepath.IsAbs(in.Cwd) {
		return "", false
	}
	st, have := LoadState(env.Home, in.SessionID)
	if have && st.Done {
		return "", false
	}
	if have && st.Hash != "" && in.SessionTitle == "" {
		// We set a name, yet the session reports none: it may have been
		// changed in a way we cannot see. Never overwrite a user's /rename.
		return "", false
	}
	var base string
	if in.SessionTitle != "" {
		if !have || st.Hash == "" || HashOf(in.SessionTitle) != st.Hash {
			// Named by the user or a plan accept: leave it alone, for good.
			_ = SaveState(env.Home, in.SessionID, State{Done: true})
			return "", false
		}
		base, _, _ = strings.Cut(in.SessionTitle, sep)
	}
	if base == "" {
		base = BranchBase(in.Cwd)
	}
	var title string
	if TranscriptAllowed(in.TranscriptPath, env.ConfigDir) {
		title, _ = Titles(in.TranscriptPath, HeadBytes, TailBytes)
	}
	generated := title != ""
	if base == "" {
		folder := filepath.Base(filepath.Clean(in.Cwd))
		base = folder + " " + env.Now().Format("15:04")
		if env.Model != nil && in.SessionTitle == "" && !have && title == "" && in.Prompt != "" {
			if t := env.Model(in.Prompt); t != "" {
				base, title = folder, t
			}
		}
	}
	name := Compose(base, title)
	if name == "" {
		return "", false
	}
	done := generated
	if name == in.SessionTitle {
		if done {
			_ = SaveState(env.Home, in.SessionID, State{Hash: HashOf(name), Done: true})
		}
		return "", false
	}
	if err := SaveState(env.Home, in.SessionID, State{Hash: HashOf(name), Done: done}); err != nil {
		return "", false
	}
	return name, true
}

// Model title (R172).
const (
	// ModelPromptRunes is how much of the prompt is sent to the model.
	ModelPromptRunes = 1000
	modelInstruction = "Give a 3 to 6 word title for this task. Reply with the title only."
	maxModelRunes    = 60
	maxModelWords    = 8
)

// ModelStdin is the child's whole stdin: one instruction line, a blank line,
// then the prompt cut to its first ModelPromptRunes characters.
func ModelStdin(prompt string) string {
	if rs := []rune(prompt); len(rs) > ModelPromptRunes {
		prompt = string(rs[:ModelPromptRunes])
	}
	return modelInstruction + "\n\n" + prompt
}

// ParseModelReply returns the title in a `claude -p --output-format json`
// result, or "" unless the result succeeded and is a short, clean, one-line
// title. It never returns error text.
func ParseModelReply(out []byte) string {
	var res struct {
		IsError *bool  `json:"is_error"`
		Subtype string `json:"subtype"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(out, &res) != nil || res.IsError == nil || *res.IsError || res.Subtype != "success" {
		return ""
	}
	if strings.ContainsAny(strings.TrimSpace(res.Result), "\r\n") {
		return ""
	}
	t := Sanitize(res.Result)
	t = strings.TrimSpace(strings.Trim(t, "\"'`"))
	if t == "" || len([]rune(t)) > maxModelRunes || len(strings.Fields(t)) > maxModelWords ||
		strings.HasPrefix(t, "API Error") || strings.HasPrefix(t, "Give a 3 to 6 word") || strings.Contains(t, modelInstruction) {
		return ""
	}
	return t
}
