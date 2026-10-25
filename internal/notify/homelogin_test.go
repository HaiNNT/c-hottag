package notify

import (
	"reflect"
	"testing"
)

// R183: the text of the notice for Home's login being refused is held exactly.
func TestHomeLoginRefusedNotice(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.HomeLoginRefused()
	want := [][2]string{{
		"chottag: Claude Code's own login was refused",
		`Claude Code's own login (Home) was refused: sessions will say "Login expired". Run /login in one session; chottag's accounts are fine.`,
	}}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
}
