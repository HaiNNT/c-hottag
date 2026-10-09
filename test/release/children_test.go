package release

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// F232: a test that started `go test` over a tree containing itself
// recursed until the owner killed ~500 processes. A first version of this
// check used regexps over *_test.go source text; a review found it missed
// 14 of 15 planted escape forms and never looked outside *_test.go at all.
// A second, go/ast version fixed that, but a re-review found its selfExec
// guard check only asked that SOME function in the file read the guard's
// key — so gutting the one if that actually gated a spawn (`if dir != ""`
// to `if false`), while leaving the now-dead `os.Getenv` read standing,
// still passed, and so did a guard neutralised while an unrelated function
// elsewhere in the file happened to read the same key for something else.
// A third review found the same class of gap survived in the two sites
// that spawn `go test <pkgs>` directly (goTool, the public-snapshot
// test): their guard was still only "some os.Getenv call reads the key
// before some spawn in the same function," never that an if actually
// gated on it and ended the path — deleting the public-snapshot test's
// whole `if nested { t.Skip(...) }` block, or turning goTool's
// `t.Fatalf` into `t.Logf`, both still passed.
//
// This version parses every .go file in the tree (not only *_test.go)
// with go/ast and finds every call to exec.Command, exec.CommandContext,
// os.StartProcess, syscall.Exec and syscall.ForkExec, and every
// exec.Cmd{Path: ...} composite literal, resolving package identifiers
// through each file's own import aliases. A call whose program argument
// is a plain string literal naming a small, tree-derived SAFE tool (git,
// sh, awk, python3, bun, gh, dash, shellcheck, …) is allowed outright —
// unless it is sh/bash running a -c (or -lc, -xc, …) script that itself
// names a go subcommand or a .test binary, or a -c script that is not a
// plain literal at all (concatenated or otherwise computed, so this scan
// cannot read it), either of which is flagged like any other spawn site.
// A variable's origin is resolved from EVERY assignment to it in its
// function, not only the first, and classified by the worst of them (a
// variable that is "git" on one branch and "go" on another is a go-tool
// spawn). Everything not SAFE — "go" itself, a variable, a const, a
// GOROOT-joined path, os.Args[0], os.Executable(), a struct field, … —
// is a SPAWN SITE, and must match an entry in goSpawnAllowlist, keyed by
// "relative/file.go#EnclosingFunc". An unmatched site fails, naming
// file:line:func; a table entry no live site matches also fails, so the
// table cannot go stale in either direction.
//
// Every allowlist entry names a KIND:
//   - buildOnly: every literal go-subcommand argument at that site must be
//     "build" or "vet"; "test" or "run" fails outright (a buildOnly site
//     must never silently grow into a test-spawning one).
//   - selfExec: a child that could re-run `go test`/`go run` over a tree
//     containing itself, or re-exec this test binary. The entry names a
//     GUARD token (an env-var name, or the Go identifier holding one).
//     Both halves of the guard are checked. First, the key must be set in
//     THIS child's environment. The spawn must be assigned to a variable
//     X, and X.Env (after that assignment, before X is reassigned), or
//     the exec.Cmd literal's own Env: field, must hold a "KEY=..."
//     element (a fourth review found a third fix round had dropped this
//     half; a fifth found a function-wide version let a second, bare
//     `exec.Command(os.Args[0], ...).Run()` ride on a sibling cmd's
//     Env). Only a go-tool site whose literal subcommand is neither test
//     nor run (`go env GOCACHE`, which starts no test binary) is checked
//     at function scope instead. Second, the os.Getenv branch must gate
//     and end the path, as follows.
//     When the spawn names a `-test.run=^Name$` helper, the scan resolves
//     Name to that FuncDecl anywhere in the same package and requires its
//     body to START with an if testing os.Getenv(guard) — directly, or
//     through one variable assigned on the immediately preceding
//     statement — whose own body ends the helper path (return,
//     t.Fatal/Fatalf/FailNow, t.Skip/Skipf/SkipNow, or os.Exit); when
//     Name is the spawning function itself, that if must sit before the
//     spawn. A spawn with no -test.run at all (goTool's and the public-
//     snapshot test's `go test <pkgs>`, which run a fresh test binary,
//     not this one) is checked the same way but directly against its own
//     enclosing function, anywhere before the spawn rather than only the
//     function's first statement (goTool leads with t.Helper(); the
//     snapshot test's guard-if follows an unrelated error check) — an if
//     testing os.Getenv(guard) directly, or a BARE (never a comparison)
//     variable an earlier assignment tainted by reading it, whose body
//     ends the path. The bare-identifier restriction on that indirect
//     form is deliberate: the snapshot test's `nested, problem :=
//     snapshotGuardState(os.Getenv(guard), marker)` taints both names,
//     but only `if nested {` (bare) counts as the guard — `if problem !=
//     "" { t.Fatal(problem) }` (a comparison) does not, so deleting the
//     real guard cannot hide behind that unrelated check surviving. Any
//     -test.run argument must be exactly one, in "-test.run=^Name$" form
//     (never a bare "-test.run" flag with a separate value, and never a
//     second flag that could override it). guard == "" is reserved for a
//     self-exec whose only -test.run is the empty, unanchored-nothing
//     "^$" (runs no test, so nothing to guard).
//   - externalTool: a spawn that is not the go tool and not this test
//     binary — a real CLI (claude, gh, osascript, a built chottag, a
//     sandboxed shell, an unreadable -c script). No further static check
//     applies.
//
// The goal, per the controller's design, is a tripwire against honest
// mistakes (an agent-written test reaching for the go tool), not a proof
// against an adversary rewriting this very file.

type spawnKind int

const (
	kindBuildOnly spawnKind = iota
	kindSelfExec
	kindExternalTool
)

func (k spawnKind) String() string {
	switch k {
	case kindBuildOnly:
		return "buildOnly"
	case kindSelfExec:
		return "selfExec"
	case kindExternalTool:
		return "externalTool"
	default:
		return "unknown"
	}
}

// allowEntry is one permitted shape for the spawn call(s) inside one
// enclosing function. guard is the GUARD token for kindSelfExec ("" for
// the no-real-test "^$" case); it is ignored for the other kinds.
type allowEntry struct {
	kind  spawnKind
	guard string
}

// goSpawnAllowlist is keyed by "relative/file.go#EnclosingFunc". A
// function may hold more than one entry when it spawns more than one kind
// of child (TestReleaseLdflagsStampTheBuiltBinary builds with `go build`,
// buildOnly, and then separately runs the binary it just built,
// externalTool).
var goSpawnAllowlist = map[string][]allowEntry{
	"cmd/chottag/buildcheck_test.go#goTool": {
		{kindSelfExec, "goToolChildEnv"},
	},
	"test/leakscan/publicsnapshot_test.go#TestSimulatedPublicSnapshotPassesItsOwnGitDependentChecks": {
		{kindSelfExec, "snapshotGuardEnv"},
	},
	"test/release/release_test.go#TestReleaseLdflagsStampTheBuiltBinary": {
		{kindBuildOnly, ""},
		{kindExternalTool, ""},
	},
	"test/installsh/harness_test.go#prepare": {
		{kindBuildOnly, ""},
	},
	"test/installsh/harness_test.go#exec": {
		{kindExternalTool, ""}, // (*sandbox).exec: runs install.sh under a shell with only fake tools on PATH
	},
	"test/devenv/shell_test.go#TestShellKeepsDevBinFirst": {
		{kindExternalTool, ""}, // runs scripts/dev-env under a temp HOME and a temp sandbox root
	},
	"internal/cli/proxy_run_test.go#TestRunProxyExitsWithCode130OnASecondSignal": {
		{kindSelfExec, "CHOTTAG_T8_HELPER"},
	},
	"internal/cli/daemon_helpers_test.go#startHelperProcess": {
		{kindSelfExec, "helperModeEnv"},
	},
	"internal/daemonlock/daemonlock_test.go#startHelper": {
		{kindSelfExec, "helperModeEnv"},
	},
	"internal/daemonlock/daemonlock_test.go#deadPID": {
		{kindSelfExec, ""}, // "-test.run=^$": a throwaway child that runs no test at all
	},
	"internal/owners/owners_test.go#TestTwoProcessesSerialiseOnOwnersLock": {
		{kindSelfExec, "OWNERS_LOCK_HELPER_DIR"},
	},
	"internal/proxy/proxytest/harness_test.go#TestRecordsFailsLoudlyOnAnUnreadableLog": {
		{kindSelfExec, "PROXYTEST_UNREADABLE_LOG_HELPER"},
	},
	"internal/proxy/proxytest/harness_test.go#TestRecordsGivesUpAtTheHangGuard": {
		{kindSelfExec, "PROXYTEST_HANG_GUARD_HELPER"},
	},
	"internal/notify/notify.go#runCommand": {
		{kindExternalTool, ""}, // osascript
	},
	"internal/refresh/claude.go#execRun": {
		{kindExternalTool, ""}, // claude
	},
	"internal/cli/accounts.go#slotEmail": {
		{kindExternalTool, ""}, // claude
	},
	"internal/cli/update.go#execUpdateChild": {
		{kindExternalTool, ""}, // the just-replaced chottag binary
	},
	"internal/cli/updateloop.go#runAutoUpdateChild": {
		{kindExternalTool, ""}, // the installed chottag, running `update --version <tag> --no-restart --json`
	},
	"internal/cli/restartloop.go#spawnDetached": {
		{kindExternalTool, ""}, // the installed chottag, running `daemon restart --force --json` detached (R126)
	},
	"internal/cli/resume.go#realCmuxRun": {
		{kindExternalTool, ""}, // cmux, behind the cmuxRun seam (M11)
	},
	"internal/cli/doctor.go#runClaudeVersion": {
		{kindExternalTool, ""}, // claude
	},
	"internal/cli/login.go#runClaudeAuth": {
		{kindExternalTool, ""}, // claude
	},
	"internal/creds/creds.go#ExecRunner": {
		{kindExternalTool, ""}, // claude, via the product's generic runner
	},
	"internal/shim/shim.go#syscallExec": {
		{kindExternalTool, ""}, // claude, replacing this process
	},
	"internal/shim/shim.go#spawnDaemon": {
		{kindExternalTool, ""}, // this same chottag binary, re-launched as the daemon: production self-relaunch, not the go tool or this test binary, so selfExec's guard/-test.run machinery does not apply
	},
	"internal/cli/proxyauth_wiring_test.go#TestEnvLinesEvalToTheSecretURLWithoutPrintingIt": {
		{kindExternalTool, ""}, // fix round 2 item B: a computed (concatenated) sh -c script, not a literal this scan can read, so it needs an entry even though it never touches go
	},
}

// safeBase is the set of program basenames a literal string argument (or a
// variable proven, in the same function, to come from
// exec.LookPath(<one of these>)) may name without an allowlist entry.
// Derived from what the tree spawns today; "go" is deliberately excluded
// even though it would otherwise qualify, because catching every go-tool
// spawn is this whole check's point.
var safeBase = map[string]bool{
	"git": true, "sh": true, "bash": true, "awk": true, "python3": true,
	"bun": true, "gh": true, "dash": true, "shellcheck": true,
}

// shDashCFlag matches any shell flag ending in "c" that can carry a
// script argument (-c, -lc, -xc, …), not only a bare "-c".
var shDashCFlag = regexp.MustCompile(`^-[a-z]*c$`)

// shGoEscape matches a -c script that itself starts the go tool or names
// a compiled test binary — the one way a SAFE sh/bash literal must still
// be flagged (design step A's exception).
var shGoEscape = regexp.MustCompile(`\bgo\s+(test|run|build|vet)\b|\.test\b`)

// testRunPrefix is a literal argument shaped "-test.run=...".
const testRunPrefix = "-test.run="

// anchoredTestRun is testRunPrefix's only acceptable value: exactly one
// helper name (or none, for "^$", which runs no test).
var anchoredTestRun = regexp.MustCompile(`^-test\.run=\^[A-Za-z0-9_]*\$$`)

type category int

const (
	catSafe category = iota
	catOther
	catSelfBinary
	catGoTool
)

// catRank orders category by how strong a claim it makes: classifying a
// variable by the WORST of all its assignments means the assignment
// giving the highest rank wins.
func catRank(c category) int { return int(c) }

// spawnSite is one call this scan must account for: a call whose program
// argument did not classify as catSafe.
type spawnSite struct {
	relFile  string
	funcName string
	line     int
	pos      token.Pos     // the spawn's own position, for a same-function before/after check
	node     ast.Expr      // the spawn call or exec.Cmd literal itself, for siteSetsGuardEnv
	fd       *ast.FuncDecl // the spawn's own enclosing function, for spawningFuncGuardOK
	args     []ast.Expr
	cat      category
	why      string // human-readable reason the classifier gave, for error text
}

// goFiles lists every .go file under the repo root, skipping vendor,
// testdata and dot-directories (.git, and .claude/worktrees' other
// checkouts).
func goFiles(t *testing.T) []string {
	t.Helper()
	r := root(t)
	var out []string
	err := filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != r && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			rel, _ := filepath.Rel(r, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// pkgAliases maps a file's local package identifiers to their canonical
// import paths (a file importing `x "os/exec"` maps "x" -> "os/exec"),
// so the scan is not fooled by an aliased import. An unaliased import's
// default identifier (its path's last component) resolves the same way
// through the fallback in is(), so lookups work whether or not this map
// even has an entry.
type pkgAliases map[string]string

func aliasesOf(file *ast.File) pkgAliases {
	out := pkgAliases{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil && imp.Name.Name != "_" && imp.Name.Name != "." {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

// is reports whether ident refers to canonical (e.g. "os/exec") in this
// file: through a recorded import (aliased or not), or, absent one (a
// dot-import, or this map not covering the file at hand), by falling
// back to the identifier matching canonical's own default name.
func (a pkgAliases) is(ident *ast.Ident, canonical string) bool {
	if ident == nil {
		return false
	}
	if p, ok := a[ident.Name]; ok {
		return p == canonical
	}
	return ident.Name == canonical[strings.LastIndex(canonical, "/")+1:]
}

// spawnFuncSelector reports whether call is one of the process-spawning
// functions this scan tracks, and which argument holds the program name.
func spawnFuncSelector(call *ast.CallExpr, aliases pkgAliases) (programIdx int, ok bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return 0, false
	}
	switch {
	case aliases.is(pkg, "os/exec") && sel.Sel.Name == "Command":
		return 0, true
	case aliases.is(pkg, "os/exec") && sel.Sel.Name == "CommandContext":
		return 1, true
	case aliases.is(pkg, "os") && sel.Sel.Name == "StartProcess":
		return 0, true
	case aliases.is(pkg, "syscall") && sel.Sel.Name == "Exec":
		return 0, true
	case aliases.is(pkg, "syscall") && sel.Sel.Name == "ForkExec":
		return 0, true
	}
	return 0, false
}

// isExecCmdType reports whether t is exec.Cmd (a composite literal's
// type), e.g. in `exec.Cmd{Path: p}`.
func isExecCmdType(t ast.Expr, aliases pkgAliases) bool {
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && aliases.is(pkg, "os/exec") && sel.Sel.Name == "Cmd"
}

// cmdCompositeFields returns exec.Cmd{...}'s Path and Args keyed fields,
// nil when either is absent.
func cmdCompositeFields(lit *ast.CompositeLit) (path, args ast.Expr) {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Path":
			path = kv.Value
		case "Args":
			args = kv.Value
		}
	}
	return path, args
}

// argsAfterProgram returns exec.Cmd.Args' conventional argv[1:] (argv[0]
// repeats the program name) from a `[]string{...}` composite literal, or
// nil if args is not one.
func argsAfterProgram(args ast.Expr) []ast.Expr {
	if args == nil {
		return nil
	}
	lit, ok := args.(*ast.CompositeLit)
	if !ok || len(lit.Elts) == 0 {
		return nil
	}
	return lit.Elts[1:]
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func isGorootCall(e ast.Expr, aliases pkgAliases) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && aliases.is(pkg, "runtime") && sel.Sel.Name == "GOROOT"
}

func isOSArgs0(e ast.Expr, aliases pkgAliases) bool {
	idx, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	sel, ok := idx.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || !aliases.is(pkg, "os") || sel.Sel.Name != "Args" {
		return false
	}
	lit, ok := idx.Index.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

// classifyOrigin looks at a value being assigned and reports what kind of
// "where did this program name come from" it is, for the variable
// tracking classifyProgram does: exec.LookPath(<lit>), a bare string
// literal, os.Executable(), filepath.Join(runtime.GOROOT(), ...), or
// os.Args[0].
func classifyOrigin(e ast.Expr, aliases pkgAliases) (kind, lit string) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if s, ok := stringLit(v); ok {
			return "literal", s
		}
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok {
			return "", ""
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", ""
		}
		switch {
		case aliases.is(pkg, "os/exec") && sel.Sel.Name == "LookPath" && len(v.Args) == 1:
			if s, ok := stringLit(v.Args[0]); ok {
				return "lookpath", s
			}
		case aliases.is(pkg, "os") && sel.Sel.Name == "Executable":
			return "executable", ""
		case aliases.is(pkg, "path/filepath") && sel.Sel.Name == "Join" && len(v.Args) > 0 && isGorootCall(v.Args[0], aliases):
			return "goroot", ""
		}
	case *ast.IndexExpr:
		if isOSArgs0(v, aliases) {
			return "selfargs", ""
		}
	}
	return "", ""
}

// originsOf searches fn's body for EVERY assignment (":=" or "=") that
// gives name a value, and classifies each one's origin — not only the
// first, so a variable safe on one assignment and "go" on another is
// still caught (fix round 2 item B). Parameters (never assigned) and
// anything this scan does not specifically recognise contribute nothing.
func originsOf(fn *ast.FuncDecl, name string, aliases pkgAliases) [][2]string {
	var out [][2]string
	if fn == nil || fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name {
				continue
			}
			var rhs ast.Expr
			switch {
			case len(as.Rhs) == len(as.Lhs):
				rhs = as.Rhs[i]
			case len(as.Rhs) == 1:
				rhs = as.Rhs[0] // multi-value assignment, e.g. `x, err := f()`
			default:
				continue
			}
			if k, l := classifyOrigin(rhs, aliases); k != "" {
				out = append(out, [2]string{k, l})
			}
		}
		return true
	})
	return out
}

func classifyLiteral(s string) (category, string) {
	base := filepath.Base(s)
	if base == "go" {
		return catGoTool, "literal naming \"go\" (" + strconv.Quote(s) + ")"
	}
	if safeBase[base] {
		return catSafe, s
	}
	return catOther, "literal " + strconv.Quote(s)
}

func classifyByOriginKind(kind, lit string) (category, string) {
	switch kind {
	case "lookpath", "literal":
		return classifyLiteral(lit)
	case "executable":
		return catSelfBinary, "os.Executable()"
	case "selfargs":
		return catSelfBinary, "os.Args[0]"
	case "goroot":
		return catGoTool, "filepath.Join(runtime.GOROOT(), ...)"
	}
	return catOther, "variable of unknown origin"
}

// classifyProgram classifies a spawn call's program argument. fn is the
// call's enclosing top-level function, used to resolve a local
// variable's origin (by the WORST of all its assignments — item B);
// fileConsts holds this file's single-string-literal consts, for a
// variable that turns out to be one of those instead (only when it has
// no local assignment of its own to resolve from).
func classifyProgram(fn *ast.FuncDecl, fileConsts map[string]string, aliases pkgAliases, e ast.Expr) (category, string) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if s, ok := stringLit(v); ok {
			return classifyLiteral(s)
		}
	case *ast.Ident:
		origins := originsOf(fn, v.Name, aliases)
		if len(origins) > 0 {
			worstCat, worstWhy := catSafe, ""
			for _, o := range origins {
				cat, why := classifyByOriginKind(o[0], o[1])
				if catRank(cat) > catRank(worstCat) {
					worstCat, worstWhy = cat, why
				}
			}
			return worstCat, worstWhy
		}
		if s, ok := fileConsts[v.Name]; ok {
			return classifyLiteral(s)
		}
		return catOther, "variable " + v.Name + " (origin not tracked)"
	case *ast.IndexExpr:
		if isOSArgs0(v, aliases) {
			return catSelfBinary, "os.Args[0]"
		}
	case *ast.CallExpr:
		if kind, _ := classifyOrigin(v, aliases); kind != "" {
			return classifyByOriginKind(kind, "")
		}
		return catOther, "call expression"
	case *ast.SelectorExpr:
		return catOther, "field " + v.Sel.Name
	}
	return catOther, "unrecognised expression"
}

// shCScriptEscapes reports whether call passes a shell "-c"-family flag
// (programLit is the shell's own literal name: sh, bash or dash) whose
// script argument either is not a plain string literal at all (item B:
// concatenated or otherwise computed, so this scan cannot read it — a
// spawn site on its own) or is one that starts a go subcommand or names
// a .test binary.
func shCScriptEscapes(call *ast.CallExpr, programIdx int) (escapes bool, why string) {
	for i := programIdx + 1; i+1 < len(call.Args); i++ {
		flag, ok := stringLit(call.Args[i])
		if !ok || !shDashCFlag.MatchString(flag) {
			continue
		}
		script, ok := stringLit(call.Args[i+1])
		if !ok {
			return true, "shell " + flag + " script is not a plain string literal (concatenated or computed): this scan cannot read it"
		}
		if shGoEscape.MatchString(script) {
			return true, "shell " + flag + " script names a go subcommand or a .test binary"
		}
	}
	return false, ""
}

// fileConstsOf collects every top-level `const NAME = "literal"` in file.
func fileConstsOf(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				if s, ok := stringLit(vs.Values[i]); ok {
					out[name.Name] = s
				}
			}
		}
	}
	return out
}

// collectSpawnSites parses file and returns every spawn call, and every
// exec.Cmd{Path: ...} composite literal, whose program argument is not
// catSafe, attributed to its enclosing top-level function (a closure's
// calls are attributed to the FuncDecl containing it, matching how the
// allowlist names things).
func collectSpawnSites(t *testing.T, fset *token.FileSet, relFile string, file *ast.File) []spawnSite {
	t.Helper()
	consts := fileConstsOf(file)
	aliases := aliasesOf(file)
	var sites []spawnSite
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		funcName := fd.Name.Name
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				programIdx, ok := spawnFuncSelector(node, aliases)
				if !ok || len(node.Args) <= programIdx {
					return true
				}
				cat, why := classifyProgram(fd, consts, aliases, node.Args[programIdx])
				if progLit, isLit := stringLit(node.Args[programIdx]); cat == catSafe && isLit {
					if base := filepath.Base(progLit); base == "sh" || base == "bash" || base == "dash" {
						if escapes, escWhy := shCScriptEscapes(node, programIdx); escapes {
							cat, why = catOther, escWhy
						}
					}
				}
				if cat == catSafe {
					return true
				}
				sites = append(sites, spawnSite{
					relFile:  relFile,
					funcName: funcName,
					line:     fset.Position(node.Pos()).Line,
					pos:      node.Pos(),
					node:     node,
					fd:       fd,
					args:     node.Args[programIdx+1:],
					cat:      cat,
					why:      why,
				})
			case *ast.CompositeLit:
				if !isExecCmdType(node.Type, aliases) {
					return true
				}
				pathExpr, argsExpr := cmdCompositeFields(node)
				if pathExpr == nil {
					return true
				}
				cat, why := classifyProgram(fd, consts, aliases, pathExpr)
				if cat == catSafe {
					return true
				}
				sites = append(sites, spawnSite{
					relFile:  relFile,
					funcName: funcName,
					line:     fset.Position(node.Pos()).Line,
					pos:      node.Pos(),
					node:     node,
					fd:       fd,
					args:     argsAfterProgram(argsExpr),
					cat:      cat,
					why:      why + " (exec.Cmd{Path: ...} composite literal)",
				})
			}
			return true
		})
	}
	return sites
}

// checkTestRunArgs inspects a spawn site's literal arguments for
// -test.run flags. ok is false if: the flag appears in the illegal
// "-test.run", "value" two-argument form; it appears more than once (the
// second-flag trick); or its one value is not anchored "^Name$". value is
// that one anchored argument, e.g. "-test.run=^$".
func checkTestRunArgs(site spawnSite) (ok bool, value, msg string) {
	count, anchoredCount := 0, 0
	for _, a := range site.args {
		s, ok := stringLit(a)
		if !ok {
			continue
		}
		if s == "-test.run" {
			return false, "", "-test.run passed as a separate argument from its value (the space form): use one \"-test.run=^Name$\" argument"
		}
		if strings.HasPrefix(s, testRunPrefix) {
			count++
			if anchoredTestRun.MatchString(s) {
				anchoredCount++
				value = s
			}
		}
	}
	switch {
	case count == 0:
		return true, "", "" // no -test.run at all: a plain `go <subcommand> <pkgs>` child, not a self-exec-by-flag
	case count > 1:
		return false, "", "passes more than one -test.run flag: a later one can override an earlier anchored one (the second-flag trick)"
	case anchoredCount != 1:
		return false, "", "-test.run is not anchored to a single helper name (\"^Name$\")"
	default:
		return true, value, ""
	}
}

// extractTestRunName pulls "Name" out of an anchored "-test.run=^Name$"
// value ("" for the empty "^$" case, which runs no test at all, and for
// "" itself, meaning no -test.run argument was present).
func extractTestRunName(value string) string {
	name := strings.TrimPrefix(value, testRunPrefix)
	name = strings.TrimPrefix(name, "^")
	name = strings.TrimSuffix(name, "$")
	return name
}

func isGetenvGuardCall(call *ast.CallExpr, guard string, aliases pkgAliases) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || !aliases.is(pkg, "os") || sel.Sel.Name != "Getenv" || len(call.Args) != 1 {
		return false
	}
	if s, ok := stringLit(call.Args[0]); ok {
		return s == guard
	}
	id, ok := call.Args[0].(*ast.Ident)
	return ok && id.Name == guard
}

// exprContainsGetenvGuard reports whether e calls os.Getenv(guard)
// anywhere within it (not only as e itself), e.g. inside
// snapshotGuardState(os.Getenv(guard), marker).
func exprContainsGetenvGuard(e ast.Expr, guard string, aliases pkgAliases) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && isGetenvGuardCall(call, guard, aliases) {
			found = true
			return false
		}
		return true
	})
	return found
}

// bareOrNegatedIdent reports whether cond is exactly an identifier (`v`)
// or its negation (`!v`), and returns that identifier's name — the one
// shape a tainted variable's guard-if is trusted in (see
// funcGuardsBeforeSpawn): a comparison like `problem != ""` does NOT
// qualify, even when problem happens to share an assignment with a
// tainted variable, so a sibling return value from the same call (an
// error string alongside a real guard bool) cannot be mistaken for the
// guard itself.
func bareOrNegatedIdent(cond ast.Expr) (string, bool) {
	switch c := cond.(type) {
	case *ast.Ident:
		return c.Name, true
	case *ast.UnaryExpr:
		if c.Op == token.NOT {
			if id, ok := c.X.(*ast.Ident); ok {
				return id.Name, true
			}
		}
	}
	return "", false
}

// funcGuardsBeforeSpawn reports whether fd contains, at its top level and
// strictly before spawnPos, an if statement that gates on guard and whose
// body ends the path (helperEndsPath): either its condition calls
// os.Getenv(guard) directly (anywhere in the expression, at any nesting —
// goTool's `if os.Getenv(goToolChildEnv) != "" {`), or its condition is
// bare tainted-variable(-or-negation) — where "tainted" means an earlier
// top-level assignment's right-hand side calls os.Getenv(guard) somewhere
// in it (the public-snapshot test's `nested, problem :=
// snapshotGuardState(os.Getenv(guard), marker)` then `if nested {`). The
// bare-identifier requirement on the indirect case is what stops
// `problem` — tainted by the very same assignment, but tested only via
// `if problem != "" { t.Fatal(problem) }`, a wholly different check — from
// standing in for the real guard once it is deleted.
func funcGuardsBeforeSpawn(fd *ast.FuncDecl, aliases pkgAliases, guard string, spawnPos token.Pos) (ifStmt *ast.IfStmt, ok bool) {
	if fd == nil || fd.Body == nil {
		return nil, false
	}
	tainted := map[string]bool{}
	for _, stmt := range fd.Body.List {
		if stmt.Pos() >= spawnPos {
			break
		}
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, isID := lhs.(*ast.Ident)
				if !isID {
					continue
				}
				var rhs ast.Expr
				switch {
				case len(s.Rhs) == len(s.Lhs):
					rhs = s.Rhs[i]
				case len(s.Rhs) == 1:
					rhs = s.Rhs[0] // multi-value assignment, e.g. `nested, problem := f(...)`
				}
				if rhs != nil && exprContainsGetenvGuard(rhs, guard, aliases) {
					tainted[id.Name] = true
				}
			}
		case *ast.IfStmt:
			direct := exprContainsGetenvGuard(s.Cond, guard, aliases)
			indirect := false
			if name, isBare := bareOrNegatedIdent(s.Cond); isBare {
				indirect = tainted[name]
			}
			if (direct || indirect) && blockEndsHelperPath(s.Body, aliases) {
				return s, true
			}
		}
	}
	return nil, false
}

// spawningFuncGuardOK requires site's own enclosing function to guard
// itself: fix round 3's rule, reusing helperGuardOK's structure but
// applied directly to the spawning function (site.fd) rather than a
// -test.run-resolved helper, for the two sites — goTool, the public-
// snapshot test — that spawn `go test <pkgs>` with no -test.run name to
// resolve at all (a fresh test binary, not this one).
func spawningFuncGuardOK(site spawnSite, aliases pkgAliases, guard string) (bool, string) {
	if _, ok := funcGuardsBeforeSpawn(site.fd, aliases, guard, site.pos); !ok {
		return false, fmt.Sprintf("%s must have an if — testing os.Getenv(%q) directly, or a bare tainted variable assigned from an expression reading it — before its spawn, whose body ends the path (return, t.Fatal/Fatalf/FailNow, t.Skip/Skipf/SkipNow, or os.Exit)", site.funcName, guard)
	}
	return true, ""
}

// packageConsts merges fileConstsOf over every parsed file in relFile's
// directory, so a guard identifier declared in a sibling file still
// resolves to the env key it holds.
func packageConsts(files map[string]*ast.File, relFile string) map[string]string {
	dir := filepath.Dir(relFile)
	out := map[string]string{}
	for rel, f := range files {
		if filepath.Dir(rel) != dir {
			continue
		}
		for name, v := range fileConstsOf(f) {
			out[name] = v
		}
	}
	return out
}

// flattenConcat returns the operands of a left-associated `a + b + c`
// string concatenation in order ([e] for anything that is not one).
func flattenConcat(e ast.Expr) []ast.Expr {
	if be, ok := e.(*ast.BinaryExpr); ok && be.Op == token.ADD {
		return append(flattenConcat(be.X), flattenConcat(be.Y)...)
	}
	return []ast.Expr{e}
}

// envKeyChecker decides whether an env expression, inside fd, sets guard's
// key: selfExec's half (i). An element sets the key when it is a string
// literal starting "KEY=", or a concatenation whose first operand names
// the key (the guard identifier, any const holding the key, or the key
// literal) followed by a literal starting "=". An identifier inside the
// expression (the public-snapshot test's runEnv) is followed to every
// assignment to it in fd. A bare mention that sets nothing, such as
// withoutEnv(env, KEY), which strips the key, never counts.
type envKeyChecker struct {
	guard, key string
	consts     map[string]string
	assigns    map[string][]ast.Expr
}

func newEnvKeyChecker(fd *ast.FuncDecl, guard string, consts map[string]string) *envKeyChecker {
	c := &envKeyChecker{guard: guard, key: guard, consts: consts, assigns: map[string][]ast.Expr{}}
	if v, ok := consts[guard]; ok {
		c.key = v
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if len(x.Rhs) == len(x.Lhs) {
					c.assigns[id.Name] = append(c.assigns[id.Name], x.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) == len(x.Values) {
				for i, name := range x.Names {
					c.assigns[name.Name] = append(c.assigns[name.Name], x.Values[i])
				}
			}
		}
		return true
	})
	return c
}

func (c *envKeyChecker) namesKey(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == c.guard || c.consts[x.Name] == c.key
	case *ast.BasicLit:
		s, ok := stringLit(x)
		return ok && s == c.key
	}
	return false
}

// setsKey reports whether e holds an element that sets the key.
func (c *envKeyChecker) setsKey(e ast.Expr) bool {
	return c.setsKeyVisited(e, map[string]bool{})
}

func (c *envKeyChecker) setsKeyVisited(e ast.Expr, visited map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.BasicLit:
			if s, ok := stringLit(x); ok && strings.HasPrefix(s, c.key+"=") {
				found = true
			}
		case *ast.BinaryExpr:
			ops := flattenConcat(x)
			if len(ops) >= 2 && c.namesKey(ops[0]) {
				if s, ok := stringLit(ops[1]); ok && strings.HasPrefix(s, "=") {
					found = true
				}
			}
		case *ast.Ident:
			if !visited[x.Name] {
				visited[x.Name] = true
				for _, rhs := range c.assigns[x.Name] {
					if c.setsKeyVisited(rhs, visited) {
						found = true
						break
					}
				}
			}
		}
		return !found
	})
	return found
}

// spawningFuncSetsGuardEnv is half (i) at function scope: some `X.Env =
// rhs` assignment, or `Env: rhs` composite field, anywhere in fd sets the
// key. It is used only for a go-tool site whose literal subcommand is
// neither test nor run (the public-snapshot test's `go env GOCACHE`),
// which starts no test binary; every other guarded site must pass the
// stricter siteSetsGuardEnv.
func spawningFuncSetsGuardEnv(fd *ast.FuncDecl, guard string, consts map[string]string) bool {
	if fd == nil || fd.Body == nil {
		return false
	}
	c := newEnvKeyChecker(fd, guard, consts)
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" && len(x.Rhs) == len(x.Lhs) && c.setsKey(x.Rhs[i]) {
					found = true
				}
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Env" && c.setsKey(x.Value) {
				found = true
			}
		}
		return !found
	})
	return found
}

// unwrapExpr strips parentheses and a leading & from e.
func unwrapExpr(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.UnaryExpr:
			if x.Op != token.AND {
				return e
			}
			e = x.X
		default:
			return e
		}
	}
}

// compositeEnv returns the Env: field's value in lit, or nil.
func compositeEnv(lit *ast.CompositeLit) ast.Expr {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Env" {
				return kv.Value
			}
		}
	}
	return nil
}

// siteSetsGuardEnv is half (i) tied to one spawn: site's own spawn
// expression must be assigned to a variable X, and fd must set X's
// environment, either with an `X.Env = rhs` assignment after the spawn's
// assignment and before any later assignment to X, or with an `Env:`
// field in the exec.Cmd literal itself. Either way the value must set the
// key (envKeyChecker). X is matched by the parser's object resolution, so
// a shadowing variable of the same name elsewhere does not count. An
// inline, unassigned spawn, such as `exec.Command(os.Args[0], ...).Run()`,
// fails: its child's environment cannot be the one a sibling cmd set
// (fix round 5: a second, bare self-spawn next to a guarded one otherwise
// passed the function-scoped check).
func siteSetsGuardEnv(site spawnSite, guard string, consts map[string]string) (bool, string) {
	fd := site.fd
	if fd == nil || fd.Body == nil || site.node == nil {
		return false, "has no enclosing function body to check"
	}
	c := newEnvKeyChecker(fd, guard, consts)
	if lit, ok := site.node.(*ast.CompositeLit); ok {
		if env := compositeEnv(lit); env != nil && c.setsKey(env) {
			return true, ""
		}
	}
	//lint:ignore SA1019 parser-resolved Obj is exact within one file, which is all this scan needs; go/types would add a type-check of every package
	var obj *ast.Object
	var assignPos token.Pos
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if obj != nil {
			return false
		}
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) != len(x.Rhs) {
				return true
			}
			for i, rhs := range x.Rhs {
				if unwrapExpr(rhs) == site.node {
					if id, ok := x.Lhs[i].(*ast.Ident); ok && id.Name != "_" && id.Obj != nil {
						obj, assignPos = id.Obj, x.Pos()
					}
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) != len(x.Values) {
				return true
			}
			for i, v := range x.Values {
				if unwrapExpr(v) == site.node && x.Names[i].Name != "_" && x.Names[i].Obj != nil {
					obj, assignPos = x.Names[i].Obj, x.Pos()
				}
			}
		}
		return true
	})
	if obj == nil {
		return false, fmt.Sprintf("spawns inline without assigning the command to a variable, so nothing can set %s in that child's environment: assign it (cmd := ...) and set cmd.Env", guard)
	}
	// The next assignment to X after the spawn's own ends the window in
	// which X.Env still belongs to this child.
	var nextAssign token.Pos
	var envs []ast.Expr
	var envPos []token.Pos
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				switch l := lhs.(type) {
				case *ast.Ident:
					if l.Obj == obj && x.Pos() > assignPos && (nextAssign == token.NoPos || x.Pos() < nextAssign) {
						nextAssign = x.Pos()
					}
				case *ast.SelectorExpr:
					if id, ok := l.X.(*ast.Ident); ok && id.Obj == obj && l.Sel.Name == "Env" && len(x.Rhs) == len(x.Lhs) {
						envs = append(envs, x.Rhs[i])
						envPos = append(envPos, x.Pos())
					}
				}
			}
		}
		return true
	})
	for i, env := range envs {
		if envPos[i] > assignPos && (nextAssign == token.NoPos || envPos[i] < nextAssign) && c.setsKey(env) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("never sets %s in this child's environment: its command variable gets no %s.Env (or Env: field) holding a \"KEY=...\" element for it between this spawn and the variable's next assignment, so the child never takes its guard branch", guard, obj.Name)
}

// needsSiteEnv reports whether site's env check must be tied to its own
// cmd (siteSetsGuardEnv). Only a go-tool site whose first argument is a
// literal subcommand other than test or run (`go env GOCACHE`) keeps the
// function-scoped check: it starts no test binary, so it cannot recurse.
func needsSiteEnv(site spawnSite) bool {
	if site.cat != catGoTool || len(site.args) == 0 {
		return true
	}
	sub, ok := stringLit(site.args[0])
	return !ok || sub == "test" || sub == "run"
}

// verifyBuildOnly fails t if any literal go-subcommand argument at site
// is "test" or "run", or if no literal "build"/"vet" argument is present
// to verify at all.
func verifyBuildOnly(t *testing.T, site spawnSite) {
	t.Helper()
	sawAllowed := false
	for _, a := range site.args {
		s, ok := stringLit(a)
		if !ok {
			continue
		}
		switch s {
		case "test", "run":
			t.Errorf("%s:%d (%s): buildOnly spawns `go %s`, which buildOnly forbids (only build/vet)", site.relFile, site.line, site.funcName, s)
			return
		case "build", "vet":
			sawAllowed = true
		}
	}
	if !sawAllowed {
		t.Errorf("%s:%d (%s): buildOnly entry, but no literal \"build\"/\"vet\" argument found to verify it", site.relFile, site.line, site.funcName)
	}
}

// processGroupMarkers are the runtime tells that a selfExec child spawning
// the go tool itself (catGoTool: `go test`/`go run`, which can fork
// compilers and test binaries of its own) puts it in its own process
// group and kills that whole group, not just it. A catSelfBinary child
// (this test binary re-execing one named helper of itself) is a single
// process with no such subtree, so this check does not apply there.
var processGroupMarkers = []string{"Setpgid: true", "syscall.Kill(-", "syscall.SIGKILL", "WaitDelay"}

// endsPathCalls are the *testing.T methods whose call ends a guard's
// if-body: the Skip family (a normal run isn't the recursion) and the
// Fatal family (goTool's re-entrancy tripwire: a nested run IS an error).
var endsPathCalls = map[string]bool{
	"Skip": true, "Skipf": true, "SkipNow": true,
	"Fatal": true, "Fatalf": true, "FailNow": true,
}

// helperEndsPath reports whether stmt is a terminal action for a guard's
// if-body: a bare return, a call to one of endsPathCalls, or os.Exit.
func helperEndsPath(stmt ast.Stmt, aliases pkgAliases) bool {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if endsPathCalls[sel.Sel.Name] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && aliases.is(id, "os") && sel.Sel.Name == "Exit" {
			return true
		}
	}
	return false
}

func blockEndsHelperPath(body *ast.BlockStmt, aliases pkgAliases) bool {
	if body == nil || len(body.List) == 0 {
		return false
	}
	return helperEndsPath(body.List[len(body.List)-1], aliases)
}

// conditionMentionsGuard reports whether cond calls os.Getenv(guard)
// directly, or references guardVar (the variable a preceding statement
// assigned from exactly that call).
func conditionMentionsGuard(cond ast.Expr, guard, guardVar string, aliases pkgAliases) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			if isGetenvGuardCall(x, guard, aliases) {
				found = true
				return false
			}
		case *ast.Ident:
			if guardVar != "" && x.Name == guardVar {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// helperGuardOK resolves helperName to a FuncDecl anywhere in the same
// package as site (every parsed file sharing its directory) and requires
// that function's body to START with an if — directly, or via one
// variable assigned on the immediately preceding statement — whose
// condition calls os.Getenv(guard), and whose body ends the helper path
// (return, t.Skip/Skipf/SkipNow, or os.Exit). When helperName is the
// spawning function itself, that if must sit before site's own spawn.
func helperGuardOK(files map[string]*ast.File, site spawnSite, guard, helperName string) (ok bool, msg string) {
	dir := filepath.Dir(site.relFile)
	var found *ast.FuncDecl
	var foundFile string
	for rel, f := range files {
		if filepath.Dir(rel) != dir {
			continue
		}
		for _, decl := range f.Decls {
			fd, isFn := decl.(*ast.FuncDecl)
			if isFn && fd.Recv == nil && fd.Name.Name == helperName && fd.Body != nil {
				found, foundFile = fd, rel
			}
		}
	}
	if found == nil {
		return false, fmt.Sprintf("cannot resolve helper func %q in this package (%s)", helperName, dir)
	}
	aliases := aliasesOf(files[foundFile])
	stmts := found.Body.List
	if len(stmts) == 0 {
		return false, fmt.Sprintf("%s is empty: it must start with a guard check", helperName)
	}
	var ifStmt *ast.IfStmt
	guardVar := ""
	switch first := stmts[0].(type) {
	case *ast.IfStmt:
		ifStmt = first
	case *ast.AssignStmt:
		if len(stmts) < 2 {
			return false, fmt.Sprintf("%s's first statement assigns a variable but is not followed by an if", helperName)
		}
		nextIf, isIf := stmts[1].(*ast.IfStmt)
		if !isIf {
			return false, fmt.Sprintf("%s's second statement must be the if reading the guard variable", helperName)
		}
		if len(first.Lhs) != 1 || len(first.Rhs) != 1 {
			return false, fmt.Sprintf("%s's first statement must assign exactly one variable from os.Getenv(%q)", helperName, guard)
		}
		id, isID := first.Lhs[0].(*ast.Ident)
		call, isCall := first.Rhs[0].(*ast.CallExpr)
		if !isID || !isCall || !isGetenvGuardCall(call, guard, aliases) {
			return false, fmt.Sprintf("%s's first statement must assign a variable from os.Getenv(%q)", helperName, guard)
		}
		guardVar = id.Name
		ifStmt = nextIf
	default:
		return false, fmt.Sprintf("%s must start with an if (or a variable assignment then an if) reading os.Getenv(%q)", helperName, guard)
	}
	if !conditionMentionsGuard(ifStmt.Cond, guard, guardVar, aliases) {
		return false, fmt.Sprintf("%s's leading if does not test os.Getenv(%q)", helperName, guard)
	}
	if !blockEndsHelperPath(ifStmt.Body, aliases) {
		return false, fmt.Sprintf("%s's leading if body does not end the helper path (return, t.Skip/Skipf/SkipNow, or os.Exit)", helperName)
	}
	if foundFile == site.relFile && helperName == site.funcName && ifStmt.Pos() >= site.pos {
		return false, fmt.Sprintf("%s spawns itself but its guard if comes after the spawn", helperName)
	}
	return true, ""
}

// verifySelfExec fails t if site's -test.run arguments, its process-group
// markers (catGoTool only), or either half of its guard, are missing.
// Half (i), the key set in the child's environment, is checked on the
// spawning function (spawningFuncSetsGuardEnv) for every guarded site.
// Half (ii), the os.Getenv branch that ends the path, is checked against
// the resolved helper for a spawn naming -test.run=^Name$ (helperGuardOK),
// and against the spawning function itself for a spawn with no -test.run
// at all, a `go test <pkgs>` child (spawningFuncGuardOK).
func verifySelfExec(t *testing.T, files map[string]*ast.File, src string, guard string, site spawnSite) {
	t.Helper()
	ok, value, msg := checkTestRunArgs(site)
	if !ok {
		t.Errorf("%s:%d (%s): %s", site.relFile, site.line, site.funcName, msg)
		return
	}
	if site.cat == catGoTool {
		for _, marker := range processGroupMarkers {
			if !strings.Contains(src, marker) {
				t.Errorf("%s:%d (%s): spawns the go tool but this file lacks %q (its own process group, killed as a group)", site.relFile, site.line, site.funcName, marker)
			}
		}
	}
	if guard == "" {
		if value != "-test.run=^$" {
			t.Errorf("%s:%d (%s): selfExec entry has guard \"\", which is only valid for a -test.run=^$ child that runs no test", site.relFile, site.line, site.funcName)
		}
		return
	}
	// Half (i), on both paths below: this site's own cmd sets the key, or,
	// for a go-tool site that starts no test binary, the function does.
	consts := packageConsts(files, site.relFile)
	if needsSiteEnv(site) {
		if ok, msg := siteSetsGuardEnv(site, guard, consts); !ok {
			t.Errorf("%s:%d (%s): %s", site.relFile, site.line, site.funcName, msg)
		}
	} else if !spawningFuncSetsGuardEnv(site.fd, guard, consts) {
		t.Errorf("%s:%d (%s): never sets %s in a child's environment: no Env assignment, Env: field or env slice built in this function holds a \"KEY=...\" element for it, so the child never takes its guard branch", site.relFile, site.line, site.funcName, guard)
	}
	if name := extractTestRunName(value); name != "" {
		if ok, msg := helperGuardOK(files, site, guard, name); !ok {
			t.Errorf("%s:%d (%s): %s", site.relFile, site.line, site.funcName, msg)
		}
		return
	}
	// No -test.run at all: goTool's and the public-snapshot test's own
	// `go test <pkgs>` spawn. Same structural rule as helperGuardOK,
	// applied directly to the spawning function itself (fix round 3).
	aliases := aliasesOf(files[site.relFile])
	if ok, msg := spawningFuncGuardOK(site, aliases, guard); !ok {
		t.Errorf("%s:%d (%s): %s", site.relFile, site.line, site.funcName, msg)
	}
}

// TestGoSpawnSitesAreAllowlistedAndGuarded is F232's gate check (P5-R16):
// every call in the tree that could start the go tool or re-exec this
// test binary must be named in goSpawnAllowlist with a kind the code at
// that site actually satisfies, and every table entry must still match a
// real site.
func TestGoSpawnSitesAreAllowlistedAndGuarded(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	srcs := map[string]string{}
	var sites []spawnSite
	for _, rel := range goFiles(t) {
		if rel == "test/release/children_test.go" {
			continue // this file's own allowlist/regexp text, not a call
		}
		abs := filepath.Join(root(t), rel)
		b, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		f, err := parser.ParseFile(fset, abs, b, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", rel, err)
		}
		files[rel] = f
		srcs[rel] = string(b)
		sites = append(sites, collectSpawnSites(t, fset, rel, f)...)
	}

	used := map[string][]bool{}
	for key, entries := range goSpawnAllowlist {
		used[key] = make([]bool, len(entries))
	}

	for _, site := range sites {
		key := site.relFile + "#" + site.funcName
		entries, ok := goSpawnAllowlist[key]
		if !ok {
			t.Errorf("%s:%d: %s starts the go tool or this test binary (%s) but %q is not in goSpawnAllowlist: give it a kind (and, for selfExec, a guard), then list it", site.relFile, site.line, site.funcName, site.why, key)
			continue
		}
		matched := false
		for i, e := range entries {
			switch {
			case site.cat == catGoTool && e.kind == kindBuildOnly:
				verifyBuildOnly(t, site)
				used[key][i], matched = true, true
			case (site.cat == catGoTool || site.cat == catSelfBinary) && e.kind == kindSelfExec:
				verifySelfExec(t, files, srcs[site.relFile], e.guard, site)
				used[key][i], matched = true, true
			case site.cat == catOther && e.kind == kindExternalTool:
				used[key][i], matched = true, true
			}
		}
		if !matched {
			t.Errorf("%s:%d (%s): %q has no entry whose kind fits this spawn (%s)", site.relFile, site.line, site.funcName, key, site.why)
		}
	}

	var keys []string
	for key := range goSpawnAllowlist {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for i, hit := range used[key] {
			if !hit {
				t.Errorf("goSpawnAllowlist lists %s (entry #%d, kind %s), which no longer matches any spawn site: remove it", key, i, goSpawnAllowlist[key][i].kind)
			}
		}
	}
}
