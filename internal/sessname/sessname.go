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

// CheckoutRoot is the nearest directory at or above cwd that holds a .git
// (a directory or a worktree's gitdir file), "" outside git.
func CheckoutRoot(cwd string) string {
	dir := filepath.Clean(cwd)
	for {
		if headFile(dir) != "" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

var clockRE = regexp.MustCompile(`^(.+) ([0-2][0-9]:[0-5][0-9])$`)

// rebaseSubfolder heals a fallback base named after a subfolder by 0.10.5 or
// older (`src 17:38`): when its folder part is a folder between the checkout
// and cwd, it becomes the checkout's folder, and the time is kept.
func rebaseSubfolder(base, cwd string) string {
	m := clockRE.FindStringSubmatch(base)
	root := CheckoutRoot(cwd)
	if m == nil || root == "" {
		return base
	}
	rel, err := filepath.Rel(root, filepath.Clean(cwd))
	if err != nil || rel == "." {
		return base
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == m[1] {
			return filepath.Base(root) + " " + m[2]
		}
	}
	return base
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

// Compose joins a base name and an optional title, title first: a tab cut
// short still shows the topic (R181). The title is shortened so that the base
// always fits in MaxRunes; split relies on that.
func Compose(base, title string) string {
	base, title = Sanitize(base), Sanitize(title)
	if title == "" || base == "" {
		return base + title
	}
	room := MaxRunes - len([]rune(base)) - len([]rune(sep))
	if room < 1 {
		return base
	}
	if rs := []rune(title); len(rs) > room {
		title = strings.TrimSpace(string(rs[:room]))
	}
	if title == "" {
		return base
	}
	return title + sep + base
}

// split undoes Compose. titleFirst is false for a name set by 0.10.4 or
// older, which put the base first. A name with no separator is a base.
func split(name string, titleFirst bool) (base, title string) {
	if !titleFirst {
		base, title, _ = strings.Cut(name, sep)
		return base, title
	}
	i := strings.LastIndex(name, sep)
	if i < 0 {
		return name, ""
	}
	return name[i+len(sep):], name[:i]
}

// HashOf is the hex sha256 of a name.
func HashOf(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:])
}

// State is what chottag keeps per session: the hash of the last name it set,
// and whether it is finished with the session. Done with no hash means the
// user named the session; an empty state means one prompt was seen and no
// name set yet. (Before 0.10.4, done with a hash meant the generated title
// was in the name; that now reads as a name chottag still follows.)
type State struct {
	Hash string `json:"sha256,omitempty"`
	Done bool   `json:"done,omitempty"`
	// TitleFirst says the name hashed is `<title> · <base>` (0.10.5 on).
	TitleFirst bool `json:"titleFirst,omitempty"`
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
}

// Decide returns the name to set, and whether to print it. A failure to
// store state gives no name: it is better to name nothing than to repeat.
//
// While the name is still the one chottag set, it follows the session: the
// base becomes the branch of the current cwd (a session that enters a
// worktree is renamed after it), and the generated title is added once. A
// session with no generated title yet is not named on its first prompt:
// Claude Code generates no title for a session that already has a name
// (F285), so naming it then would leave it without a topic for good.
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
	if have && st.Done && st.Hash == "" {
		// Named by the user or a plan accept: left alone, for good.
		return "", false
	}
	if have && st.Hash != "" && in.SessionTitle == "" {
		// We set a name, yet the session reports none: it may have been
		// changed in a way we cannot see. Never overwrite a user's /rename.
		return "", false
	}
	var base, title string
	if in.SessionTitle != "" {
		if !have || st.Hash == "" || HashOf(in.SessionTitle) != st.Hash {
			_ = SaveState(env.Home, in.SessionID, State{Done: true})
			return "", false
		}
		base, title = split(in.SessionTitle, st.TitleFirst)
	}
	if b := BranchBase(in.Cwd); b != "" {
		base = b
	}
	if title == "" && TranscriptAllowed(in.TranscriptPath, env.ConfigDir) {
		title, _ = Titles(in.TranscriptPath, HeadBytes, TailBytes)
	}
	if title == "" && !have {
		// First prompt, no generated title yet: wait one prompt for it.
		_ = SaveState(env.Home, in.SessionID, State{})
		return "", false
	}
	base = rebaseSubfolder(base, in.Cwd)
	if base == "" {
		// The checkout's folder, not cwd's: a session that cd'd into
		// a subfolder (src) is still named after its repo (R182).
		dir := CheckoutRoot(in.Cwd)
		if dir == "" {
			dir = in.Cwd
		}
		folder := filepath.Base(filepath.Clean(dir))
		base = folder + " " + env.Now().Format("15:04")
	}
	name := Compose(base, title)
	if name == "" {
		return "", false
	}
	if name == in.SessionTitle {
		if !st.TitleFirst {
			_ = SaveState(env.Home, in.SessionID, State{Hash: st.Hash, TitleFirst: true})
		}
		return "", false
	}
	if err := SaveState(env.Home, in.SessionID, State{Hash: HashOf(name), TitleFirst: true}); err != nil {
		return "", false
	}
	return name, true
}
