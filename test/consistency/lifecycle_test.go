package consistency

import "testing"

// TestUpdatingDocCoversTheUpdateFlow: check, install, restart, roll back,
// the repo it follows, what it verifies, and the plugin.
func TestUpdatingDocCoversTheUpdateFlow(t *testing.T) {
	requirePhrases(t, "docs/updating.md",
		"chottag update --check", "chottag update --restart", "chottag update --version",
		"--repo", "install.json", "checksums.txt", "gh attestation verify",
		"chottag daemon restart", "versions/", "claude plugin marketplace update",
	)
}

// TestUninstallDocRemovesEveryPiece: the daemon, the shim, the logins, the
// plugin, a status line and a service unit, plus the emergency exit.
func TestUninstallDocRemovesEveryPiece(t *testing.T) {
	requirePhrases(t, "docs/uninstall.md",
		"chottag daemon stop", "chottag uninstall", "chottag uninstall --purge",
		"`--purge` does not stop the daemon",
		"~/.chottag/versions", "CHOTTAG_HOME", "claude plugin uninstall chottag@c-hottag",
		"statusLine", "launchctl", "systemctl --user", "CHOTTAG_BYPASS=1 claude",
		"https://github.com/HaiNNT/c-hottag/blob/main/packaging/README.md",
	)
}
