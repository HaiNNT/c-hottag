package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// globalFlags are the flags chottag accepts anywhere on its command line
// (spec §5.3). They are stripped by splitGlobal before dispatch, so no
// command's own FlagSet ever sees them.
type globalFlags struct {
	json bool
}

// globalFlagTable is the pre-pass's table of global flags. --json is its
// only row; --yes deliberately stays a logout flag (R39, spec §5). Every row
// is a boolean flag: `--name`, `--name=true|false`, or Go's single-dash
// spelling `-name` (status's own FlagSet accepted `-json` before M1d-d).
var globalFlagTable = []struct {
	name string
	set  func(g *globalFlags, v bool)
}{
	{"json", func(g *globalFlags, v bool) { g.json = v }},
}

// splitGlobal removes every global-flag token that appears before the
// first literal "--", from any position, and returns the flags it found
// and the remaining arguments in their original order. "--" itself and
// everything after it are kept, so the command's own parser stops there
// too: `chottag trace mark -- --json` keeps its text. A flag VALUE spelled
// literally --json (`login --claude --json A`) is taken as the global flag;
// spec §5.3 documents and accepts that. The last occurrence wins, as in the
// flag package. It runs for `chottag` only: the `claude` shim forwards its
// arguments to Claude Code untouched (cli.Run never calls this for it).
func splitGlobal(args []string) (globalFlags, []string, error) {
	var g globalFlags
	rest := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return g, append(rest, args[i:]...), nil
		}
		set, value, hasValue, ok := globalFlag(a)
		if !ok {
			rest = append(rest, a)
			continue
		}
		v := true
		if hasValue {
			b, err := strconv.ParseBool(value)
			if err != nil {
				name, _, _ := strings.Cut(a, "=")
				return globalFlags{}, nil, fmt.Errorf("invalid value %q for %s: want true or false", value, name)
			}
			v = b
		}
		set(&g, v)
	}
	return g, rest, nil
}

// globalFlag reports whether tok spells a row of globalFlagTable, with or
// without "=value".
func globalFlag(tok string) (set func(*globalFlags, bool), value string, hasValue, ok bool) {
	name, found := strings.CutPrefix(tok, "--")
	if !found {
		if name, found = strings.CutPrefix(tok, "-"); !found {
			return nil, "", false, false
		}
	}
	name, value, hasValue = strings.Cut(name, "=")
	for _, row := range globalFlagTable {
		if row.name == name {
			return row.set, value, hasValue, true
		}
	}
	return nil, "", false, false
}
