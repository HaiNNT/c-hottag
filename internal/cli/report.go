package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// errCode is error.code in a --json error document (spec §5.3): a stable
// snake_case token that scripts and agents branch on (R44). The message
// beside it is for humans and must not be parsed. The list is CLOSED: a
// new code is added here, with a constant, or not at all.
type errCode string

const (
	codeUsage                     errCode = "usage"
	codeJSONUnsupported           errCode = "json_unsupported"
	codeInternal                  errCode = "internal"
	codeUnknownAccount            errCode = "unknown_account"
	codeAmbiguousAccount          errCode = "ambiguous_account"
	codeNoCandidate               errCode = "no_candidate"
	codeNotFound                  errCode = "not_found"
	codeLoginFailed               errCode = "login_failed"
	codeRoleHeld                  errCode = "role_held"
	codeOutOfTree                 errCode = "out_of_tree"
	codeConfirmationRequired      errCode = "confirmation_required"
	codeAborted                   errCode = "aborted"
	codeRevokeFailed              errCode = "revoke_failed"
	codeCredentialDeleteFailed    errCode = "credential_delete_failed"
	codeNoSlots                   errCode = "no_slots"
	codeNoAccounts                errCode = "no_accounts"
	codeNotChottagHome            errCode = "not_chottag_home"
	codePurgeFailed               errCode = "purge_failed"
	codeForeignDaemon             errCode = "foreign_daemon"
	codeUnhealthy                 errCode = "unhealthy"
	codeUnreadableRecord          errCode = "unreadable_record"
	codeStartFailed               errCode = "start_failed"
	codeSupervised                errCode = "supervised"
	codeLiveSessions              errCode = "live_sessions"
	codeSignalFailed              errCode = "signal_failed"
	codeStopTimeout               errCode = "stop_timeout"
	codeStopFailed                errCode = "stop_failed"
	codeSupervisorNoRelaunch      errCode = "supervisor_no_relaunch"
	codeSessionRegistryUnreadable errCode = "session_registry_unreadable"
	codeNameTaken                 errCode = "name_taken"
	codeDoctorProblems            errCode = "doctor_problems"
	codeUpdateFailed              errCode = "update_failed"
	codeUpdateInProgress          errCode = "update_in_progress"
)

// errCodes is the closed list. TestCodeConstantsMatchTheClosedLists keeps
// it and the constants above the same set.
var errCodes = []errCode{
	codeUsage, codeJSONUnsupported, codeInternal,
	codeUnknownAccount, codeAmbiguousAccount, codeNoCandidate, codeNotFound,
	codeLoginFailed, codeRoleHeld, codeOutOfTree, codeConfirmationRequired,
	codeAborted, codeRevokeFailed, codeCredentialDeleteFailed,
	codeNoSlots, codeNoAccounts, codeNotChottagHome, codePurgeFailed,
	codeForeignDaemon, codeUnhealthy, codeUnreadableRecord, codeStartFailed,
	codeSupervised, codeLiveSessions, codeSignalFailed, codeStopTimeout,
	codeStopFailed, codeSupervisorNoRelaunch, codeSessionRegistryUnreadable,
	codeNameTaken,
	codeDoctorProblems,
	codeUpdateFailed,
	codeUpdateInProgress,
}

// warnCode is a warnings[].code: a line that reports something beyond the
// result (spec §5.3). Closed, like errCode.
type warnCode string

const (
	warnLimited                   warnCode = "limited"
	warnOutOfRotation             warnCode = "out_of_rotation"
	warnNoRotationLeft            warnCode = "no_rotation_left"
	warnOwnersRecovered           warnCode = "owners_recovered"
	warnEmailRegistered           warnCode = "email_registered"
	warnRevokeFailed              warnCode = "revoke_failed"
	warnNameNoSlot                warnCode = "name_no_slot"
	warnLeftInPlace               warnCode = "left_in_place"
	warnSessionRegistryUnreadable warnCode = "session_registry_unreadable"
	warnLiveSessions              warnCode = "live_sessions"
	warnUpstreamChanged           warnCode = "upstream_changed"
	warnSupervisorRelaunch        warnCode = "supervisor_relaunch"
	warnNewDaemon                 warnCode = "new_daemon"
	warnRCNotWritten              warnCode = "rc_not_written"
	warnAdoptFailed               warnCode = "adopt_failed"
	warnUpdateDeferred            warnCode = "update_deferred"
	warnRestartFailed             warnCode = "restart_failed"
	warnPruneFailed               warnCode = "prune_failed"
	warnInstallRecord             warnCode = "install_record"
	warnAttestationSkipped        warnCode = "attestation_skipped"
	warnPreAttestation            warnCode = "pre_attestation"
	warnUpdateCache               warnCode = "update_cache"
)

var warnCodes = []warnCode{
	warnLimited, warnOutOfRotation, warnNoRotationLeft, warnOwnersRecovered,
	warnEmailRegistered, warnRevokeFailed, warnNameNoSlot, warnLeftInPlace,
	warnSessionRegistryUnreadable, warnLiveSessions, warnUpstreamChanged,
	warnSupervisorRelaunch, warnNewDaemon, warnRCNotWritten,
	warnAdoptFailed,
	warnUpdateDeferred, warnRestartFailed, warnPruneFailed, warnInstallRecord,
	warnAttestationSkipped, warnPreAttestation, warnUpdateCache,
}

func validErrCode(c errCode) bool   { return slices.Contains(errCodes, c) }
func validWarnCode(c warnCode) bool { return slices.Contains(warnCodes, c) }

// codeFor classifies an error a command is about to report as-is: an
// account lookup that found nothing or found two, or anything else, which
// is internal (an unreadable state.json, a failed write, $HOME unresolved).
func codeFor(err error) errCode {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return codeUnknownAccount
	case errors.Is(err, store.ErrAmbiguous):
		return codeAmbiguousAccount
	}
	return codeInternal
}

type warning struct {
	Code    warnCode `json:"code"`
	Message string   `json:"message"`
}

type failure struct {
	exit    int
	code    errCode
	message string
	details map[string]any
}

// reporter is one invocation's output (spec §5.3). Every command writes
// through it: Text for a human result line (suppressed under --json), Warn
// and TextWarn for lines beyond the result (also collected as warnings),
// and exactly one of OK or Fail. Text mode writes exactly what the command
// wrote before M1d-d; JSON mode additionally writes one document on stdout,
// and nothing else ever reaches stdout.
//
// It refuses a second document in BOTH modes, by panicking: in text mode
// no document is written, but a command that reports twice has a bug, and
// the text-mode tests are the bulk of the suite, so that is where the
// guard catches it.
type reporter struct {
	jsonMode bool
	stdout   io.Writer
	stderr   io.Writer
	warnings *[]warning // shared with Nested children
	written  bool
	nested   bool
	failed   *failure // nested only: the failure Relay reports
	result   any      // nested only: the fields OK recorded (setup reads adopt's)
}

func newReporter(jsonMode bool, stdout, stderr io.Writer) *reporter {
	return &reporter{jsonMode: jsonMode, stdout: stdout, stderr: stderr, warnings: &[]warning{}}
}

// JSON reports whether --json is on.
func (r *reporter) JSON() bool { return r.jsonMode }

// Stdout is where a command writes human result text: the real stdout in
// text mode, and nowhere in JSON mode, where stdout carries only the
// document.
func (r *reporter) Stdout() io.Writer {
	if r.jsonMode {
		return io.Discard
	}
	return r.stdout
}

// Stderr is the real stderr in both modes.
func (r *reporter) Stderr() io.Writer { return r.stderr }

// ChildStdout is the stdout to hand a child process that talks to the user
// (login's `claude auth login`, logout's `claude auth logout`): chottag's
// stdout in text mode, and stderr in JSON mode so the child's output can
// never land in front of the document. A browser flow still works, since
// the child writes its prompts to a terminal either way.
func (r *reporter) ChildStdout() io.Writer {
	if r.jsonMode {
		return r.stderr
	}
	return r.stdout
}

// Text writes a human result line; suppressed in JSON mode.
func (r *reporter) Text(format string, a ...any) {
	fmt.Fprintf(r.Stdout(), format, a...)
}

// Warn writes line (without its trailing newline) to stderr, exactly as the
// command did before M1d-d, and records it as a warning.
func (r *reporter) Warn(code warnCode, line string) {
	mustWarnCode(code)
	fmt.Fprintln(r.stderr, line)
	r.addWarning(code, line)
}

// TextWarn is Warn for a line the command prints on STDOUT in text mode
// (restart's live-sessions line, for one): there it stays, byte-identical;
// in JSON mode it is only a warning.
func (r *reporter) TextWarn(code warnCode, line string) {
	mustWarnCode(code)
	fmt.Fprintln(r.Stdout(), line)
	r.addWarning(code, line)
}

// RecordWarn records a warning whose text is already on stderr, because a
// nested command's own Fail printed it: text mode gains nothing, and JSON
// mode gains the warning (setup's adopt_failed, F152).
func (r *reporter) RecordWarn(code warnCode, msg string) {
	mustWarnCode(code)
	r.addWarning(code, msg)
}

func (r *reporter) addWarning(code warnCode, line string) {
	*r.warnings = append(*r.warnings, warning{Code: code, Message: humanMessage(line)})
}

// OK reports success. In JSON mode it writes the success document: the
// header (version, ok, warnings), then fields' own members in their
// order. fields may be nil. It returns exit.OK, or exit.Error with an
// internal document if fields cannot be encoded.
func (r *reporter) OK(fields any) int {
	r.claim()
	if r.nested {
		r.result = fields
		return exit.OK
	}
	if !r.jsonMode {
		return exit.OK
	}
	b, err := successDocument(*r.warnings, fields)
	if err != nil {
		msg := "internal error: " + err.Error()
		fmt.Fprintf(r.stderr, "chottag: %s\n", msg)
		r.stdout.Write(mustErrorDocument(*r.warnings, &failure{exit: exit.Error, code: codeInternal, message: msg}))
		return exit.Error
	}
	r.stdout.Write(b)
	return exit.OK
}

// Fail reports a failure: "chottag: <msg>" on stderr in both modes (the
// text every command printed before M1d-d), and in JSON mode the error
// document. It returns exitCode.
func (r *reporter) Fail(exitCode int, code errCode, msg string, details map[string]any) int {
	mustFailure(exitCode, code)
	fmt.Fprintf(r.stderr, "chottag: %s\n", msg)
	return r.FailNoText(exitCode, code, msg, details)
}

// FailNoText is Fail for a caller that has already written its own stderr
// text (a multi-line message, a usage line, the flag package's message).
func (r *reporter) FailNoText(exitCode int, code errCode, msg string, details map[string]any) int {
	mustFailure(exitCode, code)
	r.claim()
	f := &failure{exit: exitCode, code: code, message: msg, details: details}
	if r.nested {
		r.failed = f
		return exitCode
	}
	if r.jsonMode {
		r.stdout.Write(mustErrorDocument(*r.warnings, f))
	}
	return exitCode
}

// FailErr reports err as-is with exit 1: `chottag: <err>`, the most common
// failure line in the tree, classified by codeFor.
func (r *reporter) FailErr(err error) int {
	c := codeFor(err) // bound first: the AST pin accepts only a name here
	return r.Fail(exit.Error, c, err.Error(), nil)
}

// FlagError reports a FlagSet parse error. The flag package has already
// written its own message to the FlagSet's output (r.Stderr()).
func (r *reporter) FlagError(err error) int {
	return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
}

// Usage prints a usage line verbatim (it carries no "chottag: " prefix)
// and reports it as a usage error.
func (r *reporter) Usage(line string) int {
	fmt.Fprintln(r.stderr, line)
	return r.FailNoText(exit.Usage, codeUsage, line, nil)
}

// Nested returns a reporter for a command run inside another one (setup
// runs adopt). It shares the parent's writers and warnings, so its text
// and warnings appear exactly as if the parent wrote them, but its OK and
// Fail record rather than write a document: the parent writes the one
// document.
func (r *reporter) Nested() *reporter {
	return &reporter{jsonMode: r.jsonMode, stdout: r.stdout, stderr: r.stderr, warnings: r.warnings, nested: true}
}

// Relay reports n's recorded failure as r's own document without printing
// its text again (n already did), and returns its exit code. A nested
// command that returned code without reporting at all is relayed as usage
// for exit 2 and internal otherwise.
//
// code must be non-zero: Relay always reports a FAILURE (n.failed, when
// set, was already validated non-zero by mustFailure at the point n's own
// Fail/FailNoText recorded it; when n.failed is nil, the fallback below
// still calls FailNoText, whose own mustFailure panics if code is
// exit.OK). Callers only ever reach Relay after a nested command's
// non-zero return, never after a successful one.
func (r *reporter) Relay(n *reporter, code int) int {
	if f := n.failed; f != nil {
		return r.FailNoText(f.exit, f.code, f.message, f.details)
	}
	c := codeInternal
	if code == exit.Usage {
		c = codeUsage
	}
	return r.FailNoText(code, c, "the nested command failed; see stderr", nil)
}

// finish is cli.Run's last step: in JSON mode, a command that returned
// without writing a document gets the internal fallback and exit 1, so
// stdout under --json always holds exactly one document (spec §5.3).
func (r *reporter) finish(code int) int {
	if !r.jsonMode || r.written {
		return code
	}
	return r.Fail(exit.Error, codeInternal, "internal error: the command reported no result", map[string]any{"commandExit": code})
}

func (r *reporter) claim() {
	if r.written {
		panic("reporter: a second result document; a command reports exactly once (spec §5.3)")
	}
	r.written = true
}

func mustFailure(exitCode int, code errCode) {
	if !validErrCode(code) {
		panic(fmt.Sprintf("reporter: error code %q is not in report.go's closed list", code))
	}
	if exitCode == exit.OK {
		panic("reporter: a failure cannot exit 0")
	}
}

func mustWarnCode(code warnCode) {
	if !validWarnCode(code) {
		panic(fmt.Sprintf("reporter: warning code %q is not in report.go's closed list", code))
	}
}

// humanMessage is a stderr line as a document's message: without the
// "chottag: " prefix, and without a following "warning: " or "note: ".
func humanMessage(line string) string {
	msg := strings.TrimPrefix(line, "chottag: ")
	for _, p := range []string{"warning: ", "note: "} {
		if rest, ok := strings.CutPrefix(msg, p); ok {
			return rest
		}
	}
	return msg
}

// --- the document --------------------------------------------------------

type member struct {
	key   string
	value json.RawMessage
}

// successDocument renders {version, ok: true, warnings, <fields' members>}.
// fields' top-level "version", if any, becomes the header's version (so
// status's is not duplicated); otherwise the version is 1.
func successDocument(warnings []warning, fields any) ([]byte, error) {
	version := json.RawMessage("1")
	var body []member
	if fields != nil {
		b, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		ms, err := objectMembers(b)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			switch m.key {
			case "version":
				version = m.value
				continue
			case "ok", "warnings", "error":
				return nil, fmt.Errorf("result field %q collides with the document header", m.key)
			}
			body = append(body, m)
		}
	}
	return renderDocument(version, true, warnings, body)
}

// errorDocument renders {version: 1, ok: false, warnings, error: {code,
// message, exit, <details, sorted by key>}}.
func errorDocument(warnings []warning, f *failure) ([]byte, error) {
	code, err := json.Marshal(f.code)
	if err != nil {
		return nil, err
	}
	message, err := json.Marshal(f.message)
	if err != nil {
		return nil, err
	}
	fields := []member{{"code", code}, {"message", message}, {"exit", json.RawMessage(strconv.Itoa(f.exit))}}
	keys := make([]string, 0, len(f.details))
	for k := range f.details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "code", "message", "exit":
			return nil, fmt.Errorf("error detail %q collides with a reserved key", k)
		}
		v, err := json.Marshal(f.details[k])
		if err != nil {
			return nil, err
		}
		fields = append(fields, member{k, v})
	}
	obj, err := encodeObject(fields)
	if err != nil {
		return nil, err
	}
	return renderDocument(json.RawMessage("1"), false, warnings, []member{{"error", obj}})
}

func mustErrorDocument(warnings []warning, f *failure) []byte {
	b, err := errorDocument(warnings, f)
	if err != nil {
		panic(fmt.Sprintf("reporter: cannot encode an error document: %v", err))
	}
	return b
}

// renderDocument builds the compact object and indents it exactly as
// json.MarshalIndent(v, "", "  ") would: MarshalIndent is Marshal followed
// by the same indenter json.Indent runs.
func renderDocument(version json.RawMessage, ok bool, warnings []warning, body []member) ([]byte, error) {
	if warnings == nil {
		warnings = []warning{}
	}
	w, err := json.Marshal(warnings)
	if err != nil {
		return nil, err
	}
	head := []member{{"version", version}, {"ok", json.RawMessage(strconv.FormatBool(ok))}, {"warnings", w}}
	compact, err := encodeObject(append(head, body...))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// objectMembers splits a compact JSON object into its top-level members,
// in order, each value's bytes copied verbatim.
func objectMembers(b []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("a result must encode as a JSON object")
	}
	var ms []member
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("a result's key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		ms = append(ms, member{key, raw})
	}
	return ms, nil
}

func encodeObject(ms []member) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
