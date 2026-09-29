package cli

import (
	"flag"
	"fmt"
	"strings"
)

// parseInterspersed parses args against fs, accepting the command's
// positional argument either before or after its flags — `login NAME
// --claude PATH` and `login --claude PATH NAME` must both work (F2), and so
// must the --force/--yes equivalents for `logout`. Go's flag package stops
// at the first non-flag token and returns everything from there, including
// any flag that follows it, as Args() — a single fs.Parse(args) would
// therefore silently leave a trailing --claude/--force/--yes unparsed
// whenever the caller writes the name first, exactly the order every
// command's own usage text shows.
//
// It re-parses whatever fs.Args() leaves after each pass, peeling off one
// leading positional at a time, until a pass leaves nothing behind. login and
// logout still enforce "exactly one positional" themselves afterwards — this
// only makes ordering not matter, not how many names are allowed.
func parseInterspersed(fs *flag.FlagSet, args []string) (positional []string, err error) {
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		remaining := fs.Args()
		if len(remaining) == 0 {
			return positional, nil
		}
		// fs.Parse stops either at a non-flag token, which it leaves first
		// in remaining, or right after consuming a literal "--". In the
		// second case "--" ended flag parsing (spec §5.3), so every
		// remaining token is positional. (A flag VALUE spelled "--",
		// `--claude --`, looks the same here; that edge is accepted.)
		if consumed := len(rest) - len(remaining); consumed > 0 && rest[consumed-1] == "--" {
			return append(positional, remaining...), nil
		}
		positional = append(positional, remaining[0])
		rest = remaining[1:]
	}
}

// positionals is the flag rule for the hand-parsed commands (remote,
// rotate, own), which take no flags (spec §5.3). A token that starts with
// "-" before a literal "--" is a stray flag and an error, never an account
// name or an on|off. The first "--" is dropped, and everything after it is
// positional. Account names cannot start with "-" (store.ValidName), so
// nothing legitimate is refused.
func positionals(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i+1:]...), nil
		}
		if strings.HasPrefix(a, "-") {
			return nil, fmt.Errorf("unexpected flag %q", a)
		}
		out = append(out, a)
	}
	return out, nil
}
