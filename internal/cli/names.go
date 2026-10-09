package cli

import (
	"fmt"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const namesUsage = "usage: chottag names [on|off]"

// namesResult is `names --json`'s one field: the mode after the command.
type namesResult struct {
	Names string `json:"names"`
}

// namesText is the one-line description of a mode.
func namesText(mode string) string {
	switch mode {
	case store.NamesOff:
		return "names: off"
	}
	return "names: on (branch, then Claude's title)"
}

// runNames shows or sets session naming (R174), shaped like `notify`. The
// name-session hook reads the mode on each prompt, so a change needs no
// restart.
func runNames(args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, namesUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(args) > 1 {
		return r.Usage(namesUsage)
	}
	if len(args) == 1 {
		switch args[0] {
		case store.NamesOn, store.NamesOff:
		case "model":
			return r.Fail(exit.Usage, codeUsage, "the model topic was removed in 0.10.2; session names stay on", nil)
		default:
			return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("names takes on or off, not %q", args[0]), nil)
		}
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}
	var st store.State
	if len(args) == 0 {
		st, err = s.Load()
	} else {
		st, err = s.Update(func(st *store.State) error {
			st.SetNames(args[0])
			return nil
		})
	}
	if err != nil {
		return r.FailErr(err)
	}
	r.Text("%s\n", namesText(st.NamesMode()))
	return r.OK(namesResult{Names: st.NamesMode()})
}
