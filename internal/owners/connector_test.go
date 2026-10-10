package owners_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
)

func openTemp(t *testing.T) (*owners.Map, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestConnectorSharedIDPrefersTheRemoteListerButLookupStaysFirstWins(t *testing.T) {
	m, p := openTemp(t)
	c := router.KindConnector
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.Record(c, []string{"mcpsrv_test1"}, "bob@example.com", t0))
	must(m.Record(c, []string{"mcpsrv_test1"}, "alice@example.com", t0.Add(time.Second)))
	must(m.Record(c, []string{"mcpsrv_only_bob"}, "bob@example.com", t0))
	if got, _ := m.Lookup(c, "mcpsrv_test1"); got != "bob@example.com" {
		t.Fatalf("Lookup = %q, want the first lister", got)
	}
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "alice@example.com" {
		t.Fatalf("remote alice listed it: got %q", got)
	}
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "bob@example.com"); got != "bob@example.com" {
		t.Fatalf("remote bob listed it: got %q", got)
	}
	if got, ok := m.LookupPreferring(c, "mcpsrv_only_bob", "alice@example.com"); !ok || got != "bob@example.com" {
		t.Fatalf("only bob listed it: got %q %v", got, ok)
	}
	if _, ok := m.LookupPreferring(c, "mcpsrv_nobody", "alice@example.com"); ok {
		t.Fatal("an unknown id has no owner")
	}
	m.Close()

	// The listers survive a restart.
	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if got, _ := m2.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "alice@example.com" {
		t.Fatalf("after reopen: %q", got)
	}
}

func TestConnectorStaleEntryHealsOnTheRemotesNextListing(t *testing.T) {
	// An owners.json from before R179: the connector is bob's alone.
	p := filepath.Join(t.TempDir(), "owners.json")
	old := `{"connector:mcpsrv_test1":{"account":"bob@example.com","at":"2026-09-20T00:00:00Z"}}`
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := router.KindConnector
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "bob@example.com" {
		t.Fatalf("old entry keeps its account: %q", got)
	}
	if err := m.Record(c, []string{"mcpsrv_test1"}, "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "alice@example.com" {
		t.Fatalf("not healed: %q", got)
	}
}

func TestConnectorOwnPinWinsOverTheRemote(t *testing.T) {
	m, p := openTemp(t)
	c := router.KindConnector
	if err := m.Record(c, []string{"mcpsrv_test1"}, "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Reassign(c, "mcpsrv_test1", "bob@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "bob@example.com" {
		t.Fatalf("pin lost to the remote: %q", got)
	}
	m.Close()
	// The Tx path (what `chottag own` uses) pins too.
	if err := owners.Edit(p, func(tx *owners.Tx) error {
		tx.Reassign(c, "mcpsrv_test1", "carol@example.com", t0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if got, _ := m2.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "carol@example.com" {
		t.Fatalf("Tx pin lost to the remote: %q", got)
	}
}

func TestArtifactsStayFirstWinsAndListersAreNotCounted(t *testing.T) {
	m, p := openTemp(t)
	a := router.KindArtifact
	if err := m.Record(a, []string{"slug1"}, "bob@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(a, []string{"slug1"}, "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.LookupPreferring(a, "slug1", "alice@example.com"); got != "bob@example.com" {
		t.Fatalf("artifact is first-wins: %q", got)
	}
	if err := m.Record(router.KindConnector, []string{"mcpsrv_test1"}, "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	m.Close()
	counts, err := owners.CountByAccount(p)
	if err != nil {
		t.Fatal(err)
	}
	if counts["alice@example.com"] != 1 || counts["bob@example.com"] != 1 {
		t.Fatalf("counts = %v, want one object each", counts)
	}
}

func TestConnectorListerRowsFollowForgetAndRename(t *testing.T) {
	c := router.KindConnector
	m, p := openTemp(t)
	if err := m.Record(c, []string{"mcpsrv_test1"}, "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(c, []string{"mcpsrv_test1"}, "bob@example.com", t0); err != nil {
		t.Fatal(err)
	}
	// rename alice -> carol, then a new account takes the name alice.
	if _, err := m.RenameAccount("alice@example.com", "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	for _, l := range m.Listers(c, "mcpsrv_test1") {
		if l == "alice@example.com" {
			t.Fatal("a lister row still names the old spelling")
		}
	}
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "carol@example.com"); got != "carol@example.com" {
		t.Fatalf("renamed lister lost: %q", got)
	}
	if err := m.Record(c, []string{"mcpsrv_test1"}, "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "alice@example.com" {
		t.Fatalf("the new alice listing: %q", got)
	}
	// Forget drops the lister rows too.
	if err := m.Forget("bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if l := m.Listers(c, "mcpsrv_test1"); len(l) != 2 || l[0] != "alice@example.com" || l[1] != "carol@example.com" {
		t.Fatalf("listers = %v", l)
	}
	m.Close()
	// The Tx rename re-keys as well.
	if err := owners.Edit(p, func(tx *owners.Tx) error {
		tx.RenameAccount("carol@example.com", "dave@example.com")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if got, _ := m2.LookupPreferring(c, "mcpsrv_test1", "dave@example.com"); got != "dave@example.com" {
		t.Fatalf("Tx rename: %q", got)
	}
	for _, l := range m2.Listers(c, "mcpsrv_test1") {
		if l == "carol@example.com" {
			t.Fatal("a lister row still names the old spelling")
		}
	}
}

func TestConnectorPinHoldsAgainstALaterListingAndRelistingWritesNothing(t *testing.T) {
	c := router.KindConnector
	m, _ := openTemp(t)
	defer m.Close()
	if err := m.Record(c, []string{"mcpsrv_test1"}, "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Reassign(c, "mcpsrv_test1", "bob@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(c, []string{"mcpsrv_test1"}, "carol@example.com", t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.LookupPreferring(c, "mcpsrv_test1", "alice@example.com"); got != "bob@example.com" {
		t.Fatalf("pin lost: %q", got)
	}
	w := owners.Writes(m)
	if err := m.Record(c, []string{"mcpsrv_test1"}, "carol@example.com", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if owners.Writes(m) != w {
		t.Fatal("relisting by the same account wrote the file")
	}
}
