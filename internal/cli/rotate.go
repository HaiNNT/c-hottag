package cli

import (
	"fmt"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const rotateUsage = "usage: chottag rotate <name> [on|off]"

// rotateResult is `rotate --json`'s fields (spec §5.3): the account, its
// setting after the command, and how many accounts are now in rotation.
type rotateResult struct {
	Account    string `json:"account"`
	Rotate     bool   `json:"rotate"`
	InRotation int    `json:"inRotation"`
}

// runRotate includes or excludes an account from `next` and auto-switch
// (spec §5, R29). It is Account.NoRotate's first production writer: the
// field, Rotates() and the status --json projection all shipped in M1b with
// nothing to set them.
//
// `rotate <name>` with no verb prints the current setting, symmetric with
// `remote [<name>]`. It takes no flags: a stray one is exit 2, never a name
// or an on|off (spec §5.3).
func runRotate(args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, rotateUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(args) == 0 || len(args) > 2 {
		return r.Usage(rotateUsage)
	}
	var want bool
	if len(args) == 2 {
		switch args[1] {
		case "on":
			want = true
		case "off":
			want = false
		default:
			return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("rotate takes on or off, not %q", args[1]), nil)
		}
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}

	if len(args) == 1 {
		st, err := s.Load()
		if err != nil {
			return r.FailErr(err)
		}
		a, err := st.Find(args[0])
		if err != nil {
			return r.FailErr(err)
		}
		r.Text("%s: rotate %s\n", a.Name, onOff(a.Rotates()))
		return r.OK(rotateResult{Account: a.Name, Rotate: a.Rotates(), InRotation: inRotation(st)})
	}

	var name string
	var count int
	if _, err := s.Update(func(st *store.State) error {
		a, err := st.Find(args[0])
		if err != nil {
			return err
		}
		name = a.Name
		for i := range st.Accounts {
			if strings.EqualFold(st.Accounts[i].Name, a.Name) {
				st.Accounts[i].NoRotate = !want
			}
		}
		count = inRotation(*st)
		return nil
	}); err != nil {
		return r.FailErr(err)
	}

	if count == 0 {
		// A warning, not a refusal: `tag` warns rather than refusing too,
		// and refusing here would make the setting depend on the order the
		// user happens to apply it in.
		r.Warn(warnNoRotationLeft, "chottag: warning: no account left in rotation; `chottag next` will have nothing to switch to")
	}
	r.Text("%s: rotate %s\n", name, onOff(want))
	return r.OK(rotateResult{Account: name, Rotate: want, InRotation: count})
}

// inRotation counts the accounts `next` may pick.
func inRotation(st store.State) int {
	n := 0
	for _, a := range st.Accounts {
		if a.Rotates() {
			n++
		}
	}
	return n
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
