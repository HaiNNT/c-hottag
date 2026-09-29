package doctor

import (
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// Env is every seam a check uses (spec §2.1). internal/cli's newDoctorEnv
// fills it with the real functions, and tests fill it over temp dirs and
// fakes. So no test binds a port, runs claude, touches the Keychain or uses
// the network. A check never calls a cli function directly: cli imports
// this package.
type Env struct {
	Home       string // $CHOTTAG_HOME, absolute (cli.home())
	UserHome   string // $HOME: where the shell rc lives
	Shell      string // $SHELL
	PATH       string // this process's PATH
	Executable string // os.Executable() of the running chottag
	// TempDirs are the directories the bin check (I3, F173's sibling for
	// bin/) refuses to provision links under — a `go run` temp binary
	// running doctor, most obviously — since those links would dangle the
	// moment that binary is cleaned up. Production wires os.TempDir(),
	// "/tmp" and "/private/tmp" (F176: os.TempDir() alone missed a scratch
	// binary the user hit under /private/tmp on macOS). nil/empty (a test
	// that never sets it) makes that refusal inert: doctor package tests
	// do not accidentally trip it just because go test's own binary
	// happens to live under a temp dir too.
	TempDirs []string

	SetupDirs []string // cli.setupDirs: the tree under Home

	ProvisionBin  func(exe, binDir string) error                        // cli.provisionBinSymlinks
	SameFile      func(a, b string) (bool, error)                       // cli.sameFile
	RCPathFor     func(shell, userHome string) string                   // cli.rcPathFor; "" = unrecognised shell
	RCBlock       func(binDir string) string                            // cli.rcBlock
	WriteRCBlock  func(rcPath, binDir string) error                     // cli.writeRCBlock
	ResolveClaude func(pathEnv, selfDir, cached string) (string, error) // shim.ResolveClaude
	// ClaudeVersion runs claudePath --version and returns its stdout
	// (cli.runClaudeVersion: 5s timeout, 4096 bytes). It is doctor's only
	// subprocess (M2c row 15), and it is reached only through here:
	// doctor itself never imports os/exec.
	ClaudeVersion func(claudePath string) (string, error)

	ProbeHealth func(port int) bool // shim.ProbeHealth's first result
	// DaemonVersion is shim.ProbeHealth's pair, both results: whether a
	// daemon answers on port, and its health document's Version (public
	// release design §2.4). A separate seam from ProbeHealth, not the same
	// call reused, so a test can steer the version reported independently
	// of whether portCheck/daemonCheck's own probe reads healthy.
	DaemonVersion func(port int) (running bool, version string)
	// DaemonIdentity is cli.doctorIdentity (F221, part 1 T11): the same
	// VerifyHealth-based judgement `status` shows, one of "none",
	// "verified", "legacy", "mismatch" or "unknown" (a daemon answers but
	// ca/proxy.secret is unreadable, so it cannot be verified). A separate
	// seam from ProbeHealth/DaemonVersion, since it needs proxyauth's
	// secret too — doctor's own daemon-identity row is the one place that
	// judgement is surfaced report-only, never something --fix acts on.
	DaemonIdentity func(port int) string
	// SelfVersion is this running chottag's own version (cli.Version):
	// what daemonVersionCheck compares DaemonVersion's version against.
	SelfVersion  string
	Inspect      func() (daemonlock.Status, error)    // daemonlock.Inspect(Home)
	LiveSessions func() (int, error)                  // len(cli.liveSessions(Home)); prunes dead entries, so --fix only
	Listen       func(addr string) (io.Closer, error) // net.Listen("tcp", addr): the free-port probe
	Now          func() time.Time
}

// BinDir is where bin/chottag and bin/claude live.
func (e *Env) BinDir() string { return filepath.Join(e.Home, "bin") }

// Store is Home's state.json. Detect only ever Loads it: Update also
// creates state.lock, which is a write.
func (e *Env) Store() store.Store { return store.Store{Dir: e.Home} }

// State loads state.json: store.Default() when it is missing, an error when
// it is unreadable (a check returns that as Internal, exit 1).
func (e *Env) State() (store.State, error) { return e.Store().Load() }

// registered reports whether name is an account in st, by the exact,
// case-insensitive match store.Add uses. Never a prefix (F15). "" never
// matches.
func registered(st store.State, name string) bool {
	if name == "" {
		return false
	}
	for _, a := range st.Accounts {
		if strings.EqualFold(a.Name, name) {
			return true
		}
	}
	return false
}
