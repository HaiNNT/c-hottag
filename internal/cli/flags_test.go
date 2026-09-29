package cli

import (
	"bytes"
	"flag"
	"slices"
	"testing"
)

// TestParseInterspersedAcceptsFlagsBeforeOrAfterThePositional (F2): a bare
// fs.Parse(args) stops at the first non-flag token, so `NAME --claude X`
// never reaches --claude at all. parseInterspersed must accept both orders,
// and more than one flag after the positional.
func TestParseInterspersedAcceptsFlagsBeforeOrAfterThePositional(t *testing.T) {
	t.Run("flags first", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(new(bytes.Buffer))
		claude := fs.String("claude", "default", "")
		pos, err := parseInterspersed(fs, []string{"--claude", "X", "NAME"})
		if err != nil {
			t.Fatal(err)
		}
		if len(pos) != 1 || pos[0] != "NAME" || *claude != "X" {
			t.Errorf("positional=%v claude=%q, want [NAME] X", pos, *claude)
		}
	})

	t.Run("positional first", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(new(bytes.Buffer))
		claude := fs.String("claude", "default", "")
		pos, err := parseInterspersed(fs, []string{"NAME", "--claude", "X"})
		if err != nil {
			t.Fatal(err)
		}
		if len(pos) != 1 || pos[0] != "NAME" || *claude != "X" {
			t.Errorf("positional=%v claude=%q, want [NAME] X", pos, *claude)
		}
	})

	t.Run("two flags after the positional", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(new(bytes.Buffer))
		force := fs.Bool("force", false, "")
		yes := fs.Bool("yes", false, "")
		pos, err := parseInterspersed(fs, []string{"NAME", "--force", "--yes"})
		if err != nil {
			t.Fatal(err)
		}
		if len(pos) != 1 || pos[0] != "NAME" || !*force || !*yes {
			t.Errorf("positional=%v force=%v yes=%v, want [NAME] true true", pos, *force, *yes)
		}
	})

	t.Run("no positional, just flags", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(new(bytes.Buffer))
		claude := fs.String("claude", "default", "")
		pos, err := parseInterspersed(fs, []string{"--claude", "X"})
		if err != nil {
			t.Fatal(err)
		}
		if len(pos) != 0 || *claude != "X" {
			t.Errorf("positional=%v claude=%q, want [] X", pos, *claude)
		}
	})

	t.Run("a bad flag still errors", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(new(bytes.Buffer))
		_, err := parseInterspersed(fs, []string{"NAME", "--nosuchflag"})
		if err == nil {
			t.Fatal("want an error for an unknown flag")
		}
	})
}

// Spec §5.3: "--" ends flag parsing. Before M1d-d, a flag after the "--"
// was still parsed once a positional had been peeled off, so
// `tag -- B --force` set --force.
func TestParseInterspersedStopsAtDoubleDash(t *testing.T) {
	for _, c := range []struct {
		name      string
		args      []string
		wantPos   []string
		wantForce bool
	}{
		{"flag_after_double_dash_is_positional", []string{"--", "B", "--force"}, []string{"B", "--force"}, false},
		{"flag_before_double_dash_still_counts", []string{"--force", "--", "B"}, []string{"B"}, true},
		{"positional_then_double_dash", []string{"B", "--", "--force"}, []string{"B", "--force"}, false},
		{"positional_flag_double_dash", []string{"B", "--force", "--", "-x"}, []string{"B", "-x"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(new(bytes.Buffer))
			force := fs.Bool("force", false, "")
			pos, err := parseInterspersed(fs, c.args)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pos, c.wantPos) || *force != c.wantForce {
				t.Errorf("positional=%q force=%v, want %q %v", pos, *force, c.wantPos, c.wantForce)
			}
		})
	}
}

// positionals is the hand-parsed commands' rule (remote, rotate, own): a
// token starting with "-" before "--" is a stray flag, never a name or an
// on|off; the first "--" is dropped and everything after it is positional.
func TestPositionals(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		want    []string
		wantErr string
	}{
		{"plain", []string{"B", "off"}, []string{"B", "off"}, ""},
		{"none", nil, []string{}, ""},
		{"stray_long_flag", []string{"B", "--force"}, nil, `unexpected flag "--force"`},
		{"stray_short_flag_first", []string{"-x", "B"}, nil, `unexpected flag "-x"`},
		{"lone_dash", []string{"-"}, nil, `unexpected flag "-"`},
		{"double_dash_ends_flags", []string{"--", "B", "-x"}, []string{"B", "-x"}, ""},
		{"double_dash_after_a_name", []string{"B", "--", "off"}, []string{"B", "off"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := positionals(c.args)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, c.want) {
				t.Fatalf("positionals(%q) = %q, %v; want %q", c.args, got, err, c.want)
			}
		})
	}
}
