package shim

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProbeHealthRejectsEveryShapeThatIsNotOurDaemon makes the "only a
// document that unmarshals with Chottag true is proof" comment on
// probeHealth enforceable rather than aspirational: each rejection branch
// gets its own case, not just the Chottag:false one the shim tests already
// exercise indirectly (fix round 1, D9).
func TestProbeHealthRejectsEveryShapeThatIsNotOurDaemon(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    bool
	}{
		{
			name: "confirmed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"chottag":true}`))
			},
			want: true,
		},
		{
			name: "chottag false",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"chottag":false}`))
			},
			want: false,
		},
		{
			name: "non-200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"chottag":true}`))
			},
			want: false,
		},
		{
			name: "non-JSON body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`not json`))
			},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			confirmed, _ := ProbeHealth(mustPort(t, srv.URL))
			if confirmed != c.want {
				t.Errorf("probeHealth confirmed = %v, want %v", confirmed, c.want)
			}
		})
	}
}

// TestProbeHealthRejectsATransportError pins the fourth branch: nothing
// listening at all. The port comes from binding then immediately closing a
// real 127.0.0.1:0 listener, never a fixed one.
func TestProbeHealthRejectsATransportError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if confirmed, _ := ProbeHealth(port); confirmed {
		t.Error("probeHealth confirmed a port nothing is listening on")
	}
}
