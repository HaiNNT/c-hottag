package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cmuxctl"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/journal"
	"github.com/HaiNNT/c-hottag/internal/sessname"
)

// cmuxRun runs the cmux CLI. A package-level var so tests never reach a real
// cmux.
var cmuxRun cmuxctl.Runner = realCmuxRun

func realCmuxRun(bin string, args []string, env []string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

// cmuxFind locates the cmux binary, "" when there is none. A package-level
// var so tests never look at this machine's cmux.
var cmuxFind = func() string {
	return cmuxctl.Find(os.Environ(), exec.LookPath, isExecFile)
}

// isExecFile is a regular file with any exec bit.
func isExecFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

const (
	sessionsUsage = "usage: chottag sessions [--all] [--json]"
	resumeUsage   = "usage: chottag resume [--pick] [--print] [--json]"
)

// journalRow is one session in `sessions --json`.
type journalRow struct {
	PID           int        `json:"pid"`
	Dir           string     `json:"dir"`
	Pool          string     `json:"pool,omitempty"`
	Native        string     `json:"native,omitempty"`
	Started       time.Time  `json:"started"`
	Ended         *time.Time `json:"ended,omitempty"`
	Outcome       string     `json:"outcome,omitempty"`
	Resumed       *time.Time `json:"resumed,omitempty"`
	CmuxWorkspace string     `json:"cmuxWorkspace,omitempty"`
	CmuxSurface   string     `json:"cmuxSurface,omitempty"`
	Command       string     `json:"command,omitempty"` // journal.Command, for lost rows with a native
	Title         string     `json:"title,omitempty"`   // read live from the transcript, never stored (R171)
}

type sessionsResult struct {
	Sessions []journalRow `json:"sessions"`
}

type resumeRow struct {
	Dir       string    `json:"dir"`
	Pool      string    `json:"pool,omitempty"`
	Native    string    `json:"native"`
	Ended     time.Time `json:"ended"`
	Placement string    `json:"placement"` // tab | new-tab | new-workspace | print | failed
	Command   string    `json:"command"`
	Title     string    `json:"title,omitempty"` // read live from the transcript, never stored (R171)
	Error     string    `json:"error,omitempty"`
}

type resumeResult struct {
	Sessions []resumeRow `json:"sessions"`
}

// lostBatch opens the journal under home, ends the entries whose process is
// dead, and returns the sessions lost together, with the journal for updates.
func lostBatch(home string, now time.Time) ([]journal.Entry, *journal.Journal, error) {
	j, err := journal.Open(filepath.Join(home, "sessions"))
	if err != nil {
		return nil, nil, err
	}
	if _, err := j.Sweep(now, journalBootTime(), journalAlive); err != nil {
		return nil, nil, err
	}
	es, err := j.List()
	if err != nil {
		return nil, nil, err
	}
	return journal.LostBatch(es, now), j, nil
}

// lostSessions is status's hint: how many sessions were lost together and
// when the newest ended. A missing or unreadable journal is none.
func lostSessions(home string, now time.Time) (int, time.Time) {
	if fi, err := os.Stat(filepath.Join(home, "sessions")); err != nil || !fi.IsDir() {
		return 0, time.Time{}
	}
	batch, _, err := lostBatch(home, now)
	if err != nil || len(batch) == 0 {
		return 0, time.Time{}
	}
	newest := batch[0].Ended
	for _, e := range batch {
		if e.Ended.After(newest) {
			newest = e.Ended
		}
	}
	return len(batch), newest
}

// titleOf is e's session title, read live from its transcript: the last
// custom-title, else the last ai-title, else "". Control characters are
// stripped, since it is printed to a terminal. It is never stored.
func titleOf(e journal.Entry) string {
	id := e.ResumeID()
	if !journal.ValidNative(id) {
		return ""
	}
	p := sessname.FindTranscript(claudeConfigDir(), id)
	if p == "" {
		return ""
	}
	return sessname.Sanitize(sessname.Title(p))
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func journalRowOf(e journal.Entry, withCommand bool) journalRow {
	row := journalRow{PID: e.PID, Dir: e.Dir, Pool: e.Pool, Native: e.ResumeID(), Started: e.Started,
		Ended: ptrTime(e.Ended), Outcome: e.Outcome, Resumed: ptrTime(e.Resumed),
		CmuxWorkspace: e.CmuxWorkspace, CmuxSurface: e.CmuxSurface, Title: titleOf(e)}
	if withCommand && e.ResumeID() != "" {
		row.Command = journal.Command(e)
	}
	return row
}

func stateOf(e journal.Entry) string {
	switch {
	case e.Open():
		return "running"
	case !e.Resumed.IsZero():
		return "resumed"
	case e.Outcome == journal.OutcomeLost:
		return "lost"
	}
	return "exited"
}

// runSessions lists the lost batch, or with --all every recorded session.
func runSessions(args []string, r *reporter) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	all := fs.Bool("all", false, "list every recorded session")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage(sessionsUsage)
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	now := time.Now()
	batch, j, err := lostBatch(h, now)
	if err != nil {
		return r.FailErr(err)
	}
	entries := batch
	// A command is shown only for the lost batch: a row outside it is
	// resumed, running again, or from an earlier crash.
	inBatch := map[string]bool{}
	for _, e := range batch {
		inBatch[e.Key()] = true
	}
	if *all {
		if entries, err = j.List(); err != nil {
			return r.FailErr(err)
		}
	}
	rows := make([]journalRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, journalRowOf(e, inBatch[e.Key()]))
	}
	if len(entries) == 0 {
		if *all {
			r.Text("no sessions recorded\n")
		} else {
			r.Text("no lost sessions\n")
		}
		return r.OK(sessionsResult{Sessions: rows})
	}
	tw := tabwriter.NewWriter(r.Stdout(), 0, 4, 3, ' ', 0)
	fmt.Fprintln(tw, "DIR\tPOOL\tSTATE\tENDED\tTITLE\tCOMMAND")
	for i, e := range entries {
		pool, ended, cmd := e.Pool, "-", "-"
		if pool == "" {
			pool = "default"
		}
		if !e.Ended.IsZero() {
			ended = e.Ended.Local().Format("2006-01-02 15:04")
		}
		row := rows[i]
		if row.Command != "" {
			cmd = row.Command
		}
		title := row.Title
		if title == "" {
			title = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Dir, pool, stateOf(e), ended, title, cmd)
	}
	tw.Flush()
	return r.OK(sessionsResult{Sessions: rows})
}

// cmuxState returns a client and snapshot, or a nil client when cmux is not
// there or cannot be read (the reason, for a warning, is the string).
func cmuxState() (*cmuxctl.Client, cmuxctl.Snapshot, string) {
	bin := cmuxFind()
	if bin == "" {
		return nil, cmuxctl.Snapshot{}, ""
	}
	c := &cmuxctl.Client{Bin: bin, Run: cmuxRun}
	snap, err := c.Snapshot()
	if err != nil {
		return nil, cmuxctl.Snapshot{}, fmt.Sprintf("cmux top: %v", err)
	}
	return c, snap, ""
}

// runResume relaunches the lost sessions (M11 spec §6).
func runResume(args []string, stdin io.Reader, r *reporter) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	pick := fs.Bool("pick", false, "choose which sessions to resume")
	printOnly := fs.Bool("print", false, "only print the commands")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage(resumeUsage)
	}
	if *pick && r.JSON() {
		return r.Fail(exit.Usage, codeUsage, "--pick asks questions and cannot be used with --json", nil)
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	// One resume at a time: two would place the same batch twice.
	if err := os.MkdirAll(filepath.Join(h, "sessions"), 0o700); err != nil {
		return r.FailErr(err)
	}
	unlock, locked, err := fsutil.TryLock(filepath.Join(h, "sessions", "resume.lock"))
	if err != nil {
		return r.FailErr(err)
	}
	if !locked {
		return r.Fail(exit.Error, codeResumeBusy, "another chottag resume is running", nil)
	}
	defer unlock()
	now := time.Now()
	batch, j, err := lostBatch(h, now)
	if err != nil {
		return r.FailErr(err)
	}
	if len(batch) == 0 {
		r.Text("chottag: no lost sessions to resume\n")
		return r.OK(resumeResult{Sessions: []resumeRow{}})
	}

	c, snap, why := cmuxState()
	if why != "" {
		fmt.Fprintf(r.Stderr(), "chottag: %s; printing the commands instead\n", why)
	}

	if *pick {
		chosen, code, done := pickSessions(batch, snap, stdin, r)
		if done {
			return code
		}
		batch = chosen
		if c != nil { // the tabs may have changed while the user chose
			c, snap, why = cmuxState()
			if why != "" {
				fmt.Fprintf(r.Stderr(), "chottag: %s; printing the commands instead\n", why)
			}
		}
	}

	mark := func(e journal.Entry) {
		if err := j.Update(e.Key(), func(x *journal.Entry) bool { x.Resumed = now; return true }); err != nil {
			fmt.Fprintf(r.Stderr(), "chottag: could not mark %s resumed: %v\n", e.Dir, err)
		}
	}
	// The commands of a run that could not reach a cmux that exists are
	// printed, not marked: the batch stays for the next run.
	rows := placeAll(batch, c, snap, os.Getenv("CMUX_SURFACE_ID"), *printOnly, !*printOnly && why == "", mark)

	failed := 0
	for _, row := range rows {
		switch row.Placement {
		case "tab":
			r.Text("resumed %s in its tab\n", row.Dir)
		case "new-tab":
			r.Text("resumed %s in a new tab\n", row.Dir)
		case "new-workspace":
			r.Text("resumed %s in a new workspace\n", row.Dir)
		case "print":
			r.Text("%s\n", row.Command)
		default:
			failed++
			r.Text("failed %s: %s\n", row.Dir, row.Error)
		}
	}
	if failed > 0 {
		return r.Fail(exit.Error, codeResumeFailed,
			fmt.Sprintf("%d of %d sessions could not be resumed", failed, len(rows)),
			map[string]any{"sessions": rows})
	}
	return r.OK(resumeResult{Sessions: rows})
}

// pickSessions lists the batch and reads the user's choice. done means the
// command is over, with code.
func pickSessions(batch []journal.Entry, snap cmuxctl.Snapshot, stdin io.Reader, r *reporter) (chosen []journal.Entry, code int, done bool) {
	for i, e := range batch {
		pool := e.Pool
		if pool == "" {
			pool = "default"
		}
		line := fmt.Sprintf("%d  %s  (%s)  ended %s", i+1, e.Dir, pool, e.Ended.Local().Format("15:04"))
		if t := titleOf(e); t != "" {
			line += "  " + strconv.Quote(t)
		}
		if s, ok := snap.Surfaces[e.CmuxSurface]; ok && s.Title != "" {
			line += "  tab " + strconv.Quote(s.Title)
		}
		r.Text("%s\n", line)
	}
	r.Text("resume which? (numbers, all, or empty to cancel): ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, r.FailErr(err), true
	}
	line = strings.TrimSpace(line)
	if line == "" {
		r.Text("\n")
		r.Text("chottag: nothing chosen\n")
		return nil, r.OK(resumeResult{Sessions: []resumeRow{}}), true
	}
	if line == "all" {
		return batch, 0, false
	}
	seen := map[int]bool{}
	for _, tok := range strings.FieldsFunc(line, func(c rune) bool { return c == ' ' || c == ',' || c == '\t' }) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > len(batch) {
			return nil, r.Fail(exit.Usage, codeUsage, fmt.Sprintf("%q is not a number from 1 to %d, or all", tok, len(batch)), nil), true
		}
		if !seen[n] {
			seen[n] = true
			chosen = append(chosen, batch[n-1])
		}
	}
	return chosen, 0, false
}

// placeAll puts each session back, in batch order grouped by old workspace
// (M11 spec §6). c is nil when cmux is absent. mark is called after each
// placement that counts as resumed, so a later failure or crash cannot lead
// to a duplicate. self is the surface chottag itself runs in ("" outside
// cmux): it counts as idle, since cmux types the text and the shell reads it
// after chottag exits. print forces printing; markPrints says whether a
// printed row counts as resumed (only when there is no cmux at all).
func placeAll(batch []journal.Entry, c *cmuxctl.Client, snap cmuxctl.Snapshot, self string, print, markPrints bool, mark func(journal.Entry)) []resumeRow {
	var order []string
	groups := map[string][]journal.Entry{}
	for _, e := range batch {
		if _, ok := groups[e.CmuxWorkspace]; !ok {
			order = append(order, e.CmuxWorkspace)
		}
		groups[e.CmuxWorkspace] = append(groups[e.CmuxWorkspace], e)
	}
	used := map[string]bool{}
	created := map[string]string{}
	var rows []resumeRow
	for _, ws := range order {
		for _, e := range groups[ws] {
			row := resumeRow{Dir: e.Dir, Pool: e.Pool, Native: e.ResumeID(), Ended: e.Ended, Title: titleOf(e)}
			if !journal.ValidNative(e.ResumeID()) {
				// Never build a shell line from an id that is not a UUID.
				row.Placement, row.Error = "failed", "unsafe session id"
				rows = append(rows, row)
				continue
			}
			cmd := journal.Command(e)
			row.Command = cmd
			var err error
			s, onSnap := snap.Surfaces[e.CmuxSurface]
			_, wsThere := snap.Workspaces[e.CmuxWorkspace]
			switch {
			case print || c == nil || e.CmuxSurface == "":
				row.Placement = "print"
			case onSnap && (s.Idle || (self != "" && s.ID == self)) && !used[s.ID]:
				row.Placement = "tab"
				if err = c.Send(s.Workspace, s.ID, cmd); err != nil {
					err = fmt.Errorf("cmux send: %w", err)
				}
				used[s.ID] = true
			case e.CmuxWorkspace != "" && wsThere:
				row.Placement = "new-tab"
				err = newTabAndSend(c, e.CmuxWorkspace, cmd)
			case created[e.CmuxWorkspace] != "" && e.CmuxWorkspace != "":
				row.Placement = "new-tab"
				err = newTabAndSend(c, created[e.CmuxWorkspace], cmd)
			default:
				row.Placement = "new-workspace"
				var ref string
				if ref, err = c.NewWorkspace(filepath.Base(e.Dir), e.Dir, cmd); err != nil {
					err = fmt.Errorf("cmux new-workspace: %w", err)
				} else if e.CmuxWorkspace != "" {
					created[e.CmuxWorkspace] = ref
				}
			}
			if err != nil {
				row.Placement, row.Error = "failed", err.Error()
			} else if row.Placement != "print" || markPrints {
				mark(e)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func newTabAndSend(c *cmuxctl.Client, workspace, cmd string) error {
	id, err := c.NewTab(workspace)
	if err != nil {
		return fmt.Errorf("cmux new-surface: %w", err)
	}
	if err := c.Send(workspace, id, cmd); err != nil {
		return fmt.Errorf("opened a new tab but could not send the command: %w", err)
	}
	return nil
}
