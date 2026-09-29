package proxy_test

import (
	"slices"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// The CA's name constraints must cover every host chottag intercepts,
// and trace run's default suffixes, or clients reject the leaves.
func TestCAConstraintsCoverTheInterceptedHosts(t *testing.T) {
	if !slices.Equal(proxy.DefaultTraceSuffixes, ca.PermittedDomains) {
		t.Fatalf("DefaultTraceSuffixes %v != ca.PermittedDomains %v", proxy.DefaultTraceSuffixes, ca.PermittedDomains)
	}
	a, _ := ca.LoadOrCreate(t.TempDir())
	for _, h := range router.Hosts {
		if !a.Permits(h) {
			t.Errorf("router host %s is outside the CA's constraints", h)
		}
	}
}
