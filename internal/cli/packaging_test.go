package cli_test

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLaunchdPlistIsWellFormedAndInvokesDaemonRun keeps the shipped plist
// honest: a malformed plist fails at load time on the user's machine, where
// nobody is watching.
func TestLaunchdPlistIsWellFormedAndInvokesDaemonRun(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "launchd", "com.chottag.daemon.plist"))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := xml.Unmarshal(b, &v); err != nil {
		t.Fatalf("plist is not well-formed XML: %v", err)
	}
	s := string(b)
	for _, want := range []string{"<string>daemon</string>", "<string>run</string>"} {
		if !strings.Contains(s, want) {
			t.Errorf("plist is missing %q", want)
		}
	}
	// Key presence alone doesn't pin the restart model: <key>KeepAlive</key>
	// followed by <false/> would still contain the substring "KeepAlive".
	// plistKeyIsTrue checks the value that immediately follows the key.
	for _, key := range []string{"RunAtLoad", "KeepAlive"} {
		if !plistKeyIsTrue(s, key) {
			t.Errorf("plist key %q is missing or not <true/>", key)
		}
	}
	if strings.Contains(s, "$HOME") {
		t.Error("plist uses $HOME, which launchd does not expand; use the __HOME__ placeholder the README replaces")
	}
}

// plistKeyIsTrue reports whether <key>name</key> is immediately followed
// (ignoring whitespace) by <true/> in a plist's XML source.
func plistKeyIsTrue(plist, name string) bool {
	marker := "<key>" + name + "</key>"
	i := strings.Index(plist, marker)
	if i < 0 {
		return false
	}
	rest := strings.TrimLeft(plist[i+len(marker):], " \t\r\n")
	return strings.HasPrefix(rest, "<true/>")
}

func TestSystemdUnitRestartsAlwaysAndInvokesDaemonRun(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", "chottag.service"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"Restart=always", "RestartSec=2", "daemon run", "WantedBy=default.target"} {
		if !strings.Contains(s, want) {
			t.Errorf("unit is missing %q", want)
		}
	}
}
