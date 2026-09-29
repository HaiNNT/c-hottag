package cli

// The one-document harness (spec §5.3 "Tests"). Every task registers its
// commands' cases from ITS OWN test file:
//
//	func init() { registerJSONCases(jsonCase{…}, …) }
//
// so no two parallel tasks ever edit a shared test file (R43). This file is
// created by M1d-d Task 2 and read-only afterwards. Task 8's completeness
// test compares the registered commands with cli.go's dispatch labels.

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

type jsonCase struct {
	name     string
	command  string
	setup    func(t *testing.T) []string
	wantExit int
	wantCode errCode
	jsonOnly bool
	check    func(t *testing.T, doc map[string]any)
}

var jsonCases []jsonCase

func registerJSONCases(cs ...jsonCase) { jsonCases = append(jsonCases, cs...) }

// runChottag drives cli.Run exactly as the binary does.
func runChottag(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run("chottag", args, &out, &errb)
	return code, out.String(), errb.String()
}

// decodeOneDocument fails the test unless stdout is exactly one JSON
// object followed by a newline, with nothing before or after it.
func decodeOneDocument(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(stdout, "{") || !strings.HasSuffix(stdout, "}\n") {
		t.Fatalf("stdout is not exactly one JSON object and a newline:\n%s", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("stdout holds more than one JSON document:\n%s", stdout)
	}
	return doc
}

// assertDocumentHeader checks spec §5.3's common header against the exit
// code: version is a number >= 1; ok == (exit == 0); warnings is an array
// (never null) of {code in the closed list, message}; a success carries no
// error; a failure carries error {code in the closed list, message without
// the "chottag: " prefix, exit == the process exit}.
func assertDocumentHeader(t *testing.T, doc map[string]any, code int) {
	t.Helper()
	if v, ok := doc["version"].(float64); !ok || v < 1 {
		t.Errorf("version = %v, want a number >= 1", doc["version"])
	}
	ok, isBool := doc["ok"].(bool)
	if !isBool {
		t.Fatalf("ok = %v, want a bool", doc["ok"])
	}
	if ok != (code == 0) {
		t.Errorf("ok = %v with exit %d; spec §5.3: ok is exit == 0", ok, code)
	}
	ws, isArr := doc["warnings"].([]any)
	if !isArr {
		t.Fatalf("warnings = %v, want an array, never null", doc["warnings"])
	}
	for _, w := range ws {
		m, _ := w.(map[string]any)
		c, _ := m["code"].(string)
		if !validWarnCode(warnCode(c)) {
			t.Errorf("warning code %q is not in report.go's closed list", c)
		}
		if msg, _ := m["message"].(string); msg == "" || strings.HasPrefix(msg, "chottag: ") {
			t.Errorf("warning message = %q, want non-empty and without the chottag: prefix", msg)
		}
	}
	e, hasErr := doc["error"]
	if ok {
		if hasErr {
			t.Errorf("a success document carries an error: %v", e)
		}
		return
	}
	em, isObj := e.(map[string]any)
	if !isObj {
		t.Fatalf("error = %v, want an object", e)
	}
	c, _ := em["code"].(string)
	if !validErrCode(errCode(c)) {
		t.Errorf("error.code %q is not in report.go's closed list", c)
	}
	if msg, _ := em["message"].(string); msg == "" || strings.HasPrefix(msg, "chottag: ") {
		t.Errorf("error.message = %q, want non-empty and without the chottag: prefix", msg)
	}
	if x, _ := em["exit"].(float64); int(x) != code {
		t.Errorf("error.exit = %v, want the process exit %d", em["exit"], code)
	}
}

// docError returns doc's error object, failing the test if there is none.
func docError(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	e, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("doc has no error object: %v", doc)
	}
	return e
}

// TestEveryRegisteredCommandWritesExactlyOneJSONDocument runs every
// registered case twice, each on a fresh setup: with --json appended
// (stdout is exactly one document, the header matches the exit code, the
// code is the expected one), and without it (same exit, and stdout is not
// a JSON document: the text path is untouched; the existing text tests pin
// its bytes).
func TestEveryRegisteredCommandWritesExactlyOneJSONDocument(t *testing.T) {
	if len(jsonCases) == 0 {
		t.Fatal("no JSON cases are registered")
	}
	seen := map[string]bool{}
	for _, c := range jsonCases {
		if seen[c.name] {
			t.Fatalf("two JSON cases are named %q", c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			t.Run("json", func(t *testing.T) {
				args := append(c.setup(t), "--json")
				code, out, errs := runChottag(t, args...)
				if code != c.wantExit {
					t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, c.wantExit, out, errs)
				}
				doc := decodeOneDocument(t, out)
				assertDocumentHeader(t, doc, code)
				if c.wantCode != "" {
					if got := docError(t, doc)["code"]; got != string(c.wantCode) {
						t.Errorf("error.code = %v, want %s", got, c.wantCode)
					}
				}
				if c.check != nil {
					c.check(t, doc)
				}
			})
			if c.jsonOnly {
				return
			}
			t.Run("text", func(t *testing.T) {
				code, out, errs := runChottag(t, c.setup(t)...)
				if code != c.wantExit {
					t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, c.wantExit, out, errs)
				}
				if trimmed := strings.TrimSpace(out); strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
					t.Errorf("text mode wrote a JSON document:\n%s", out)
				}
			})
		})
	}
}
