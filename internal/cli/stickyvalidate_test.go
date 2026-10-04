package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/stickyval"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// R160 / F267: a session's mitm validate keeps one account for its life.

var validateDecision = router.Decision{Class: router.Serving, StickySession: true}

const (
	valSidOne = "1111111111111111aaaaaaaaaaaaaaaa"
	valSidTwo = "2222222222222222bbbbbbbbbbbbbbbb"
)

// stickyTokens is a token source whose slots are fresh, stale (Token refuses,
// Await refreshes it), stale for good (Await cannot refresh) or logged out.
type stickyTokens struct {
	state map[string]string // dir -> "ok", "stale", "stale-fail", "login"; absent = login
	name  map[string]string // dir -> account name, for the token text
	waits []string
}

func (f *stickyTokens) Token(_ context.Context, dir string) (string, creds.Status, bool) {
	switch f.state[dir] {
	case "ok":
		return "tok-" + f.name[dir], creds.Status{State: creds.StateOK}, true
	case "stale", "stale-fail":
		return "", creds.Status{State: creds.StateStale}, false
	}
	return "", creds.Status{State: creds.StateNeedsLogin}, false
}

func (f *stickyTokens) Await(ctx context.Context, dir string) (string, creds.Status, bool) {
	if tok, st, ok := f.Token(ctx, dir); ok || st.State == creds.StateNeedsLogin {
		return tok, st, ok
	}
	f.waits = append(f.waits, f.name[dir])
	if f.state[dir] == "stale" {
		f.state[dir] = "ok"
		return "tok-" + f.name[dir], creds.Status{State: creds.StateOK}, true
	}
	return "", creds.Status{State: creds.StateStale}, false
}

type stickyRig struct {
	*poolChooseRig
	toks   *stickyTokens
	events *[]selector.Event
	path   string
}

// newStickyRig is the pool rig (personal serves C, default serves D) with a
// sticky map under the rig's home and tokens the test can take away.
func newStickyRig(t *testing.T) *stickyRig {
	t.Helper()
	r := newSpreadRig(t, "A", "B", "C", "D")
	r.st.Accounts[0].NoRotate = false
	r.st.Policy = ""
	withPools(t, &r.st)
	for i := range r.st.Accounts {
		r.st.Accounts[i].Email = strings.ToLower(r.st.Accounts[i].Name) + "@example.com"
	}
	toks := &stickyTokens{state: map[string]string{}, name: map[string]string{}}
	for _, a := range r.st.Accounts {
		toks.state[a.Dir], toks.name[a.Dir] = "ok", a.Name
	}
	st := func() (store.State, error) { return r.st, nil }
	events := &[]selector.Event{}
	sel := selector.New(selector.Config{State: st, Tokens: toks, Owners: fakeOwners{}, OnEvent: func(e selector.Event) { *events = append(*events, e) }})
	log := newSyncBuf()
	ch := &chooser{sel: sel, state: st, spread: r.eng, now: func() time.Time { return r.now }, log: log}
	s := &stickyRig{poolChooseRig: &poolChooseRig{spreadRig: r, ch: ch, log: log}, toks: toks, events: events,
		path: filepath.Join(os.Getenv("CHOTTAG_HOME"), "run", "validate-sessions.json")}
	s.open(ch)
	return s
}

func (s *stickyRig) open(ch *chooser) {
	s.t.Helper()
	m, err := stickyval.Open(s.path)
	if err != nil {
		s.t.Fatal(err)
	}
	ch.sticky = m
}

func (s *stickyRig) serve(pool, account string) {
	s.t.Helper()
	mustPools(s.t, &s.st, func(st *store.State) error { return st.SetPoolServing(pool, account) })
}

func (s *stickyRig) account(name string) store.Account {
	for _, a := range s.st.Accounts {
		if a.Name == name {
			return a
		}
	}
	s.t.Fatalf("no account %s", name)
	return store.Account{}
}

func TestStickyValidateKeepsItsAccountWhenTheServingAccountMoves(t *testing.T) {
	s := newStickyRig(t)
	ctx := poolCtx("personal", valSidOne)
	if got := s.choose(ctx, validateDecision); got != "C" {
		t.Fatalf("first validate went out as %q, want the serving account C", got)
	}
	s.serve("personal", "D")
	if got := s.choose(ctx, validateDecision); got != "C" {
		t.Fatalf("validate after the pool moved to D went out as %q, want the sticky C", got)
	}
	if got := s.choose(poolCtx("personal", valSidTwo), validateDecision); got != "D" {
		t.Fatalf("another session's first validate went out as %q, want the serving D", got)
	}
	if got := s.choose(ctx, servingDecision); got != "D" {
		t.Fatalf("an inference request went out as %q, want the serving D", got)
	}
	if strings.Contains(s.log.String(), "validate for session") {
		t.Fatalf("a kept account logged a move: %q", s.log.String())
	}
}

func TestStickyValidateSurvivesADaemonRestart(t *testing.T) {
	s := newStickyRig(t)
	ctx := poolCtx("personal", valSidOne)
	s.choose(ctx, validateDecision)
	s.serve("personal", "D")

	restarted := &chooser{sel: s.ch.sel, state: s.ch.state, spread: s.ch.spread, now: s.ch.now, log: newSyncBuf()}
	s.open(restarted)
	acct, _, _, ok := restarted.Choose(ctx, validateDecision, "")
	if !ok || acct != "C" {
		t.Fatalf("after a restart validate went out as %q (ok=%v), want C", acct, ok)
	}
}

func TestStickyValidateFallsBackAndLogsWhyTheAccountCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*stickyRig)
		reason string
	}{
		{"removed", func(s *stickyRig) {
			mustPools(s.t, &s.st, func(st *store.State) error { return st.Remove("C") })
		}, "C is no longer registered"},
		{"needs login", func(s *stickyRig) { s.toks.state[s.account("C").Dir] = "login" }, "C needs login"},
		{"refresh fails", func(s *stickyRig) { s.toks.state[s.account("C").Dir] = "stale-fail" }, "C could not refresh its login"},
		{"rotation off", func(s *stickyRig) {
			for i := range s.st.Accounts {
				if s.st.Accounts[i].Name == "C" {
					s.st.Accounts[i].NoRotate = true
				}
			}
		}, "C has rotation off"},
		{"logged in as another", func(s *stickyRig) {
			for i := range s.st.Accounts {
				if s.st.Accounts[i].Name == "C" {
					s.st.Accounts[i].Email = "bob@example.com"
				}
			}
		}, "C was logged in again as another account"},
		{"other pool", func(s *stickyRig) {
			mustPools(s.t, &s.st, func(st *store.State) error { return st.LeavePool("C", "personal") })
		}, "C is not in pool personal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStickyRig(t)
			ctx := poolCtx("personal", valSidOne)
			s.choose(ctx, validateDecision)
			s.serve("personal", "D")
			tc.break_(s)
			if got := s.choose(ctx, validateDecision); got != "D" {
				t.Fatalf("validate went out as %q, want the serving D", got)
			}
			want := "validate for session 11111111 moved from C to D: " + tc.reason + "\n"
			if !strings.Contains(s.log.String(), want) {
				t.Fatalf("log %q lacks %q", s.log.String(), want)
			}
			// D is the session's account now: no more moves, no more lines.
			before := s.log.String()
			if got := s.choose(ctx, validateDecision); got != "D" {
				t.Fatalf("second validate went out as %q, want D", got)
			}
			if after := s.log.String(); strings.Count(after, "moved from") != strings.Count(before, "moved from") {
				t.Fatalf("the move was logged twice: %q", after)
			}
		})
	}
}

func TestStickyValidateWithoutASessionIdIsUnchanged(t *testing.T) {
	s := newStickyRig(t)
	if got := s.choose(context.Background(), validateDecision); got != "D" {
		t.Fatalf("unidentified validate went out as %q, want the default serving D", got)
	}
	s.serve(store.DefaultPool, "C")
	if got := s.choose(context.Background(), validateDecision); got != "C" {
		t.Fatalf("unidentified validate went out as %q, want the new serving C", got)
	}
	if n := s.ch.sticky.Len(); n != 0 {
		t.Fatalf("%d sessions recorded for callers without a sid", n)
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatalf("a file was written without a sid: %v", err)
	}
}

func TestStickyValidateStoresNoSecrets(t *testing.T) {
	s := newStickyRig(t)
	ctx := poolCtx("personal", valSidOne)
	s.choose(ctx, validateDecision)
	s.serve("personal", "D")
	s.toks.state[s.account("C").Dir] = "login"
	s.choose(ctx, validateDecision)
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"file": string(data), "log": s.log.String()} {
		if strings.Contains(text, "tok-") {
			t.Errorf("the %s holds a token: %q", name, text)
		}
	}
	if strings.Contains(s.log.String(), valSidOne) {
		t.Errorf("the log holds the whole session id: %q", s.log.String())
	}
	if info, err := os.Stat(s.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, err %v; want 0600", info.Mode(), err)
	}
}

func TestStickyValidateRefreshesAStaleStickyTokenInsteadOfMovingTheSession(t *testing.T) {
	s := newStickyRig(t)
	ctx := poolCtx("personal", valSidOne)
	s.choose(ctx, validateDecision)
	s.serve("personal", "D") // C is no longer warmed by the pool's roles
	s.toks.state[s.account("C").Dir] = "stale"
	s.now = s.now.Add(6 * time.Hour)
	if got := s.choose(ctx, validateDecision); got != "C" {
		t.Fatalf("validate hours later went out as %q, want C after a refresh", got)
	}
	if len(s.toks.waits) != 1 || s.toks.waits[0] != "C" {
		t.Fatalf("refresh waits = %v, want one wait on C", s.toks.waits)
	}
	if strings.Contains(s.log.String(), "moved from") {
		t.Fatalf("a refreshable account was given up on: %q", s.log.String())
	}
	if len(*s.events) != 0 {
		t.Fatalf("the sticky path emitted selector events %+v (a false Home-login line)", *s.events)
	}
}

func TestStickyValidateReRecordsAfterTheEntryWasDropped(t *testing.T) {
	s := newStickyRig(t)
	ctx := poolCtx("personal", valSidOne)
	s.choose(ctx, validateDecision)
	s.serve("personal", "D")
	s.now = s.now.Add(stickyval.MaxAge + time.Hour)
	if got := s.choose(ctx, validateDecision); got != "D" {
		t.Fatalf("validate after the record expired went out as %q, want D", got)
	}
	want := "validate for session 11111111 moved from C to D: C's record was dropped"
	if !strings.Contains(s.log.String(), want) {
		t.Fatalf("log %q lacks %q", s.log.String(), want)
	}
}

func TestStickyValidateAShortSidDoesNotPanic(t *testing.T) {
	s := newStickyRig(t)
	ctx := poolCtx("personal", "abc")
	s.choose(ctx, validateDecision)
	s.serve("personal", "D")
	s.toks.state[s.account("C").Dir] = "login"
	if got := s.choose(ctx, validateDecision); got != "D" {
		t.Fatalf("got %q, want D", got)
	}
	if !strings.Contains(s.log.String(), "session abc moved") {
		t.Fatalf("log %q", s.log.String())
	}
}

func TestStickyValidateConcurrentSessionsAreRaceFree(t *testing.T) {
	s := newStickyRig(t)
	// The selector's tokens and the rig's log are not the point: the map is.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("%032d", i)
			for j := 0; j < 20; j++ {
				if err := s.ch.sticky.Set(sid, "C", "c@example.com", s.now.Add(time.Duration(j)*2*time.Hour)); err != nil {
					t.Error(err)
				}
				s.ch.sticky.Get(sid, s.now)
				s.ch.sticky.Accounts(s.now, time.Hour)
			}
		}(i)
	}
	wg.Wait()
	m, err := stickyval.Open(s.path)
	if err != nil || m.Len() != 16 {
		t.Fatalf("reopened map: %d sessions, %v; want 16", m.Len(), err)
	}
}

func TestWireStickyValidateOpensAndAttachesTheMap(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "run", "validate-sessions.json")
	seed, err := stickyval.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Set(valSidOne, "C", "", timeNow()); err != nil {
		t.Fatal(err)
	}
	ch, log := &chooser{}, newSyncBuf()
	accounts := wireStickyValidate(ch, home, log)
	if ch.sticky == nil {
		t.Fatal("the map was not attached to the chooser")
	}
	if got, ok := ch.sticky.Get(valSidOne, timeNow()); !ok || got != "C" {
		t.Fatalf("the attached map lost the saved session: %q, %v", got, ok)
	}
	if got := accounts(); len(got) != 1 || got[0] != "C" {
		t.Fatalf("warm accounts = %v, want [C]", got)
	}
	if log.String() != "" {
		t.Fatalf("a good file logged %q", log.String())
	}
}

func TestWireStickyValidateReportsADamagedFileAndStartsEmpty(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "run", "validate-sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch, log := &chooser{}, newSyncBuf()
	wireStickyValidate(ch, home, log)
	if ch.sticky == nil || ch.sticky.Len() != 0 {
		t.Fatal("a damaged file did not leave an empty attached map")
	}
	if !strings.Contains(log.String(), "validate answers start afresh") {
		t.Fatalf("log %q lacks the damaged-file line", log.String())
	}
}

func TestDaemonOpensTheValidateSessionsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "run", "validate-sessions.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	<-errb.done
	sig <- os.Interrupt
	<-codeCh
	if !strings.Contains(errb.String(), "validate answers start afresh") {
		t.Fatalf("daemon log %q lacks the damaged validate-sessions line: the daemon does not open the file", errb.String())
	}
}

func TestStickyValidateNotInPoolRetryDoesNotWaitTwice(t *testing.T) {
	s := newStickyRig(t)
	ctx := poolCtx("personal", valSidOne)
	s.choose(ctx, validateDecision) // records C
	s.toks.state[s.account("C").Dir] = "stale-fail"
	// The pool's serving account is outside it: route refuses with "not in
	// pool", which makes choose try once more on fresh state.
	p := s.st.Pools["personal"]
	p.Serving = "A"
	s.st.Pools["personal"] = p
	s.ch.refresh = func() {}
	ch, _ := s.ch.choose(ctx, validateDecision, "")
	if ch.Account != "" {
		t.Fatalf("choice %+v, want a refusal", ch)
	}
	if len(s.toks.waits) != 1 {
		t.Fatalf("sticky waits = %v, want exactly one for the two attempts", s.toks.waits)
	}
}
