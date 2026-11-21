package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// refusedRow is name's row carrying a login-refused mark made at at.
func refusedRow(name string, at time.Time) status.Account {
	row := freshRow(name, 10, 10, eligNow)
	row.Token = creds.StateOK
	f := status.File{Accounts: []status.Account{row}}
	f.SetLoginRefused(name, 403, at, time.Time{})
	return f.Accounts[0]
}

func (r *spreadRig) refuse(name string, on bool) {
	if !on {
		r.rows[name] = freshRow(name, 10, 10, r.now)
		return
	}
	row := refusedRow(name, r.now)
	if row.Usage != nil {
		u := *row.Usage
		u.UpdatedAt = r.now
		row.Usage = &u
	}
	r.rows[name] = row
}

func TestPlanAccountsReadsLoginRefusedAndALaterLoginSupersedesIt(t *testing.T) {
	mark := eligNow.Add(-time.Hour)
	for _, c := range []struct {
		name       string
		loggedInAt time.Time
		want       bool
	}{
		{"no login since", time.Time{}, true},
		{"login before the mark", mark.Add(-time.Minute), true},
		{"login after the mark", mark.Add(time.Minute), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := store.Default()
			st.Accounts = []store.Account{{Name: "B", Dir: "/slots/B", LoggedInAt: c.loggedInAt}}
			f := &status.File{Accounts: []status.Account{refusedRow("B", mark)}}
			got := planAccounts(&st, f, eligNow)
			if got[0].LoginRefused != c.want || got[0].LoginBlocked() != c.want {
				t.Fatalf("LoginRefused = %v, want %v", got[0].LoginRefused, c.want)
			}
		})
	}
}

func TestNextSkipsALoginRefusedAccountEvenWithForce(t *testing.T) {
	st := store.Default()
	for _, n := range []string{"A", "B", "C"} {
		st.Accounts = append(st.Accounts, store.Account{Name: n, Dir: "/slots/" + n})
	}
	st.Serving = "A"
	f := &status.File{Accounts: []status.Account{refusedRow("B", eligNow.Add(-time.Minute))}}
	for _, force := range []bool{false, true} {
		got, skips, _, err := nextCandidate(&st, f, eligNow, force)
		if err != nil {
			t.Fatal(err)
		}
		if force {
			// force ignores everything but rotation, as for needs login.
			if got.Name != "B" {
				t.Fatalf("--force next = %s, want B", got.Name)
			}
			continue
		}
		if got.Name != "C" || len(skips) != 1 || skips[0].Name != "B" || skips[0].Reason != autoswitch.ReasonLoginRefused {
			t.Fatalf("next = %s, skips %+v; want C skipping B as login refused", got.Name, skips)
		}
	}
}

func TestAutoSwitchLineNamesALoginRefusal(t *testing.T) {
	d := autoswitch.Decision{From: "A", Target: "B", Trigger: autoswitch.TriggerRefused}
	p := autoswitch.Preset(autoswitch.ModeCacheOptimize)
	if got, want := autoSwitchLine(d, p), "chottag: auto-switch A -> B (login refused, cache-optimize)"; got != want {
		t.Fatalf("autoSwitchLine = %q, want %q", got, want)
	}
}

func TestSerialTickSwitchesOffARefusedServingAccount(t *testing.T) {
	r := newAutoRig(t, refusedRow("A", eligNow))
	r.as.tick(eligNow.Add(time.Minute))
	if got := r.serving(); got == "A" || got == "" {
		t.Fatalf("serving = %q, want a switch off the refused A", got)
	}
	if p := r.published(); p.LastSwitch == nil || p.LastSwitch.Trigger != autoswitch.TriggerRefused || p.LastSwitch.From != "A" {
		t.Fatalf("lastSwitch = %+v, want trigger refused from A", p.LastSwitch)
	}
	if n := countLines(r.log.String(), "(login refused, "); n != 1 {
		t.Fatalf("log = %q, want one login-refused auto-switch line", r.log.String())
	}
	if got := r.notices(); len(got) != 1 {
		t.Fatalf("notices = %+v, want one switch notice", got)
	}
}

func TestSerialTickNeverSwitchesOntoARefusedTarget(t *testing.T) {
	rows := append(onlyB(eligNow), refusedRow("A", eligNow), refusedRow("B", eligNow))
	r := newAutoRig(t, rows...)
	r.as.tick(eligNow.Add(time.Minute))
	if got := r.serving(); got == "B" {
		t.Fatal("switched onto the refused B")
	}
}

func TestSpreadNeverPlacesOnALoginRefusedAccount(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	r.refuse("work", true)
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		if got := r.place(sid); got == "work" {
			t.Fatalf("%s placed on the refused work", sid)
		}
	}
}

func TestSpreadMovesOffALoginRefusedAccountAtOnce(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	r.place("s1") // B
	r.usage("B", 97, 10)
	r.eng.mark("B", "B at its 5h switch point", r.now)
	if got := r.place("s1"); got != "work" {
		t.Fatalf("s1 on %q, want work", got)
	}
	// Inside the move window, work is refused: a hard move, not a wait.
	r.advance(2 * time.Minute)
	r.usage("B", 10, 10)
	r.refuse("work", true)
	got := r.place("s1")
	if got == "work" || got == "B" {
		t.Fatalf("s1 on %q: must leave the refused work at once and not return to B", got)
	}
}

func TestSpreadMarkIgnoresARefusedAccountAsTarget(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	r.refuse("work", true)
	res := r.eng.mark("B", "B at its 5h switch point", r.now)
	for _, to := range res.To {
		if to == "work" {
			t.Fatalf("mark offered the refused work as a target: %+v", res)
		}
	}
}

func TestChooserFallbackRanksARefusedAccountLast(t *testing.T) {
	c := newChooseRig(t, "A", "B", "work")
	c.st.Serving = "A"
	c.limit("B")
	c.refuse("work", true)
	if got := c.choose(idCtx("s1"), servingDecision); got != "B" {
		t.Fatalf("fallback went to %q: a refused account is worse than a limited one", got)
	}
}

func TestMoveDroppedWhenTargetLoginRefused(t *testing.T) {
	r := newSpreadRig(t)
	r.place("s1")
	r.refuse("work", true)
	r.move("s1", "work")
	if got := r.place("s1"); got != "B" {
		t.Fatalf("s1 = %q, want to stay on B", got)
	}
	if !strings.Contains(r.log.String(), "dropped") {
		t.Fatalf("log = %q, want a dropped line", r.log.String())
	}
}
