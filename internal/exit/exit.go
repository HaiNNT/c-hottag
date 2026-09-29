// Package exit holds chottag's process exit codes.
//
// It is its own package rather than a block in internal/cli because
// shim.Run returns one of these too, and a package's unexported identifiers
// are unreachable from another package (F103) — the same reason
// store.State.ResolvedPort lives in store rather than cli.
package exit

// The four codes, spec §5. A command returns UserAction when nothing failed
// but the user must decide something; that is not an error and must not be
// reported as one.
const (
	OK         = 0
	Error      = 1
	Usage      = 2
	UserAction = 3
)
