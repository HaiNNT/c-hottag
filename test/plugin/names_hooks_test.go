package plugin

import (
	"path/filepath"
	"strings"
	"testing"
)

// R171: the plugin registers the session-naming command hooks. They live in
// a second hooks file that plugin.json names (Claude Code merges it with
// hooks/hooks.json, which keeps the mod's "modules" key to itself).
func TestPluginRegistersTheSessionNamingHooks(t *testing.T) {
	var m struct {
		Hooks string `json:"hooks"`
	}
	decodeOne(t, filepath.Join(repoRoot(t), "plugin", ".claude-plugin", "plugin.json"), &m)
	if m.Hooks == "" || !strings.HasPrefix(m.Hooks, "./") {
		t.Fatalf("plugin.json hooks = %q, want a ./ path to the hooks file", m.Hooks)
	}
	var f struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	decodeOne(t, filepath.Join(repoRoot(t), "plugin", filepath.FromSlash(m.Hooks)), &f)
	for _, ev := range []string{"UserPromptSubmit"} {
		groups := f.Hooks[ev]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: want one group with one hook, got %+v", ev, groups)
		}
		h := groups[0].Hooks[0]
		if h.Type != "command" || h.Timeout != 10 {
			t.Errorf("%s: %+v, want a command hook with timeout 10", ev, h)
		}
		// The command must always exit 0 (exit 2 from UserPromptSubmit blocks
		// the prompt) and print only a successful run's output.
		if strings.Contains(h.Command, "exec ") || !strings.HasSuffix(h.Command, "; exit 0") ||
			!strings.Contains(h.Command, `2>/dev/null) && [ -n "$o" ]`) {
			t.Errorf("%s: command must not exec, must drop stderr, print only on success and end with exit 0: %s", ev, h.Command)
		}
		for _, want := range []string{"name-session", "command -v chottag", "${CHOTTAG_HOME:-$HOME/.chottag}/bin/chottag"} {
			if !strings.Contains(h.Command, want) {
				t.Errorf("%s: command lacks %q: %s", ev, want, h.Command)
			}
		}
	}
	if len(f.Hooks) != 1 {
		t.Errorf("hooks file declares %d events, want 1", len(f.Hooks))
	}
}
