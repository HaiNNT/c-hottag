// Package cmuxctl is a thin wrapper over the cmux terminal app's CLI: a
// process snapshot with idle detection, send text to a tab, open a tab and
// open a workspace. It defines no production runner; the CLI supplies one.
package cmuxctl

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// AppCLI is where the cmux app bundles its CLI.
const AppCLI = "/Applications/cmux.app/Contents/Resources/bin/cmux"

// Runner runs bin with args and env additions, returning combined output.
type Runner func(bin string, args []string, env []string) ([]byte, error)

// Surface is one terminal tab.
type Surface struct {
	ID        string // UUID
	Ref       string // surface:N
	Title     string
	Workspace string // workspace UUID
	Idle      bool   // its process tree holds only shells, or nothing
}

// Snapshot is the surfaces and workspaces cmux reports.
type Snapshot struct {
	Surfaces   map[string]Surface // by UUID
	Workspaces map[string]string  // UUID -> title
}

// Client drives the cmux CLI through Run.
type Client struct {
	Bin string
	Run Runner
}

var (
	surfaceRe   = regexp.MustCompile(`OK surface:\d+ \(([0-9A-Fa-f-]{36})\)`)
	workspaceRe = regexp.MustCompile(`OK (workspace:\d+)`)
	shells      = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true, "dash": true,
		"ksh": true, "tcsh": true, "csh": true, "nu": true, "login": true}
)

// Find picks the cmux binary: env CMUX_BUNDLED_CLI_PATH if it names an
// executable file, else lookPath("cmux"), else AppCLI if executable, else "".
func Find(env []string, lookPath func(string) (string, error), isExec func(string) bool) string {
	const key = "CMUX_BUNDLED_CLI_PATH="
	for _, e := range env {
		if p, ok := strings.CutPrefix(e, key); ok && p != "" && isExec(p) {
			return p
		}
	}
	if p, err := lookPath("cmux"); err == nil && p != "" {
		return p
	}
	if isExec(AppCLI) {
		return AppCLI
	}
	return ""
}

type proc struct {
	Name     string `json:"name"`
	Children []proc `json:"children"`
}

type topDoc struct {
	Windows []struct {
		Workspaces []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Panes []struct {
				Surfaces []struct {
					ID        string `json:"id"`
					Ref       string `json:"ref"`
					Title     string `json:"title"`
					Processes []proc `json:"processes"`
				} `json:"surfaces"`
			} `json:"panes"`
		} `json:"workspaces"`
	} `json:"windows"`
}

// helperPrefix names processes a shell starts for itself (zsh prompt
// plugins): they do not make a tab busy.
const helperPrefix = "gitstatusd"

func isShell(name string) bool {
	f := strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '(' })
	if len(f) == 0 {
		return false
	}
	n := strings.TrimLeft(f[0], "-")
	return shells[n] || strings.HasPrefix(n, helperPrefix)
}

func allShells(ps []proc) bool {
	for _, p := range ps {
		if !isShell(p.Name) || !allShells(p.Children) {
			return false
		}
	}
	return true
}

func (c Client) run(args ...string) (string, error) {
	out, err := c.Run(c.Bin, args, []string{"CMUX_QUIET=1"})
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("cmux %s: %w: %s", args[0], err, s)
	}
	return s, nil
}

// Snapshot lists every surface with its idle state.
func (c Client) Snapshot() (Snapshot, error) {
	out, err := c.run("--json", "--id-format", "both", "top", "--all", "--processes")
	if err != nil {
		return Snapshot{}, err
	}
	var d topDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return Snapshot{}, fmt.Errorf("cmux top: decode: %w", err)
	}
	snap := Snapshot{Surfaces: map[string]Surface{}, Workspaces: map[string]string{}}
	for _, w := range d.Windows {
		for _, ws := range w.Workspaces {
			snap.Workspaces[ws.ID] = ws.Title
			for _, p := range ws.Panes {
				for _, s := range p.Surfaces {
					snap.Surfaces[s.ID] = Surface{ID: s.ID, Ref: s.Ref, Title: s.Title,
						Workspace: ws.ID, Idle: allShells(s.Processes)}
				}
			}
		}
	}
	return snap, nil
}

// Send types text plus a newline into a tab. Empty ids are refused: an empty
// --workspace targets the focused workspace.
func (c Client) Send(workspace, surface, text string) error {
	if workspace == "" || surface == "" {
		return fmt.Errorf("cmux send: workspace and surface ids are required")
	}
	_, err := c.run("send", "--workspace", workspace, "--surface", surface, text+"\n")
	return err
}

// NewTab opens a terminal tab in a workspace without focusing it and returns
// its surface UUID.
func (c Client) NewTab(workspace string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("cmux new-surface: workspace id is required")
	}
	out, err := c.run("--id-format", "both", "new-surface", "--type", "terminal",
		"--workspace", workspace, "--focus", "false")
	if err != nil {
		return "", err
	}
	m := surfaceRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("cmux new-surface: unexpected output: %s", out)
	}
	return m[1], nil
}

// NewWorkspace opens a workspace running command in cwd without focusing it
// and returns its ref ("workspace:N").
func (c Client) NewWorkspace(name, cwd, command string) (string, error) {
	if name == "" || cwd == "" || command == "" {
		return "", fmt.Errorf("cmux new-workspace: name, cwd and command are required")
	}
	out, err := c.run("new-workspace", "--name", name, "--cwd", cwd, "--command", command, "--focus", "false")
	if err != nil {
		return "", err
	}
	m := workspaceRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("cmux new-workspace: unexpected output: %s", out)
	}
	return m[1], nil
}
