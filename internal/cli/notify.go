package cli

import (
	"fmt"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const notifyUsage = "usage: chottag notify [on|off]"

// notifyResult is `notify --json`'s one field: the setting after the command.
type notifyResult struct {
	Notify bool `json:"notify"`
}

// runNotify shows or sets the desktop-notification switch (M2 spec §4,
// D12). It is shaped like `rotate`: no verb prints the setting and writes
// nothing, `on`/`off` persist it to state.json, and it takes no flags. The
// daemon reads the switch each time a notice would fire, so a change
// applies without a restart.
func runNotify(args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, notifyUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(args) > 1 {
		return r.Usage(notifyUsage)
	}
	var want bool
	if len(args) == 1 {
		switch args[0] {
		case "on":
			want = true
		case "off":
			want = false
		default:
			return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("notify takes on or off, not %q", args[0]), nil)
		}
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}

	if len(args) == 0 {
		st, err := s.Load()
		if err != nil {
			return r.FailErr(err)
		}
		r.Text("notify %s\n", onOff(st.NotifyOn()))
		return r.OK(notifyResult{Notify: st.NotifyOn()})
	}

	if _, err := s.Update(func(st *store.State) error {
		st.SetNotify(want)
		return nil
	}); err != nil {
		return r.FailErr(err)
	}
	r.Text("notify %s\n", onOff(want))
	return r.OK(notifyResult{Notify: want})
}
