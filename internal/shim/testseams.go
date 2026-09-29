package shim

// SetSeamsForTest swaps execFn and spawnFn — the shim's only irreversible
// acts (shim.go) — for exec and spawn, and returns a func that restores
// whatever was installed at the moment of the call (production's own
// values, or, inside a test binary that has already swapped them, whatever
// a previous swap or a TestMain default left behind).
//
// It exists so a package OTHER than shim can drive shim.Run without really
// exec'ing or forking anything. shim's own tests reach execFn/spawnFn
// directly (they are unexported package-level vars); a cross-package test
// cannot, and an export_test.go helper would not help either — it compiles
// only into shim's own test binary, not into another package's (F103). This
// is a normal, non-`_test.go` file so the exported symbol is reachable from
// internal/cli's test binary at all.
//
// Production code never calls this: nothing outside a test imports it for
// any other reason, and the seams it swaps already default to their real
// production values (syscallExec, spawnDaemon) on every ordinary run.
func SetSeamsForTest(exec func(bin string, args, env []string) error, spawn func(exe, home, upstream string) error) (restore func()) {
	origExec, origSpawn := execFn, spawnFn
	execFn, spawnFn = exec, spawn
	return func() { execFn, spawnFn = origExec, origSpawn }
}
