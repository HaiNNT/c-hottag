package cli

import (
	"slices"
	"strings"
	"testing"
)

// TestSplitGlobal is spec §5.3's pre-pass table: every position of the
// flag, --json=false, --, a flag value spelled --json, no command, an
// unknown command, and the single-dash spelling Go's flag package accepts.
func TestSplitGlobal(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantJSON bool
		wantRest []string
	}{
		{"before_the_command", []string{"--json", "tag", "B"}, true, []string{"tag", "B"}},
		{"between_command_and_name", []string{"tag", "--json", "B"}, true, []string{"tag", "B"}},
		{"last", []string{"tag", "B", "--json"}, true, []string{"tag", "B"}},
		{"between_command_and_verb", []string{"daemon", "--json", "stop", "--force"}, true, []string{"daemon", "stop", "--force"}},
		{"explicit_true", []string{"--json=true", "status"}, true, []string{"status"}},
		{"explicit_false", []string{"--json=false", "status"}, false, []string{"status"}},
		{"last_one_wins", []string{"--json", "status", "--json=false"}, false, []string{"status"}},
		{"single_dash", []string{"status", "-json"}, true, []string{"status"}},
		{"single_dash_false", []string{"-json=false", "status"}, false, []string{"status"}},
		{"json_after_double_dash_stays", []string{"trace", "mark", "--", "--json"}, false, []string{"trace", "mark", "--", "--json"}},
		{"json_after_double_dash_only_the_first_counts", []string{"--json", "trace", "mark", "--", "--json"}, true, []string{"trace", "mark", "--", "--json"}},
		{"a_flag_value_spelled_json_is_taken_as_global", []string{"login", "--claude", "--json", "A"}, true, []string{"login", "--claude", "A"}},
		{"no_command", []string{"--json"}, true, []string{}},
		{"nothing_at_all", nil, false, []string{}},
		{"unknown_command", []string{"nosuch", "--json"}, true, []string{"nosuch"}},
		{"not_a_global_flag", []string{"--jsonx", "tag"}, false, []string{"--jsonx", "tag"}},
		{"three_dashes_is_not_the_flag", []string{"---json", "tag"}, false, []string{"---json", "tag"}},
		{"command_flags_pass_through", []string{"next", "--force"}, false, []string{"next", "--force"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, rest, err := splitGlobal(c.args)
			if err != nil {
				t.Fatalf("splitGlobal(%q) error: %v", c.args, err)
			}
			if g.json != c.wantJSON {
				t.Errorf("json = %v, want %v", g.json, c.wantJSON)
			}
			if !slices.Equal(rest, c.wantRest) {
				t.Errorf("rest = %q, want %q", rest, c.wantRest)
			}
		})
	}
}

// An unparsable value cannot say whether JSON was wanted, so it is an error
// the caller reports in text (exit 2).
func TestSplitGlobalRejectsABadValue(t *testing.T) {
	_, _, err := splitGlobal([]string{"--json=maybe", "status"})
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("err = %v, want an error naming --json", err)
	}
}

// The pre-pass must not share its backing array with the caller's args: a
// caller that later appends to args must never see rest change.
func TestSplitGlobalDoesNotAliasItsInput(t *testing.T) {
	args := []string{"tag", "--json", "B"}
	_, rest, err := splitGlobal(args)
	if err != nil {
		t.Fatal(err)
	}
	rest[0] = "changed"
	if args[0] != "tag" {
		t.Fatalf("args[0] = %q: splitGlobal's result aliases its input", args[0])
	}
}
