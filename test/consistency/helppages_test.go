package consistency

import "testing"

// TestTroubleshootingCoversTheCommonFailures (Review Focus 4): the
// emergency exit and the usual failures each have their fix.
func TestTroubleshootingCoversTheCommonFailures(t *testing.T) {
	requirePhrases(t, "docs/troubleshooting.md",
		"CHOTTAG_BYPASS=1 claude", "chottag doctor", "chottag doctor --fix",
		"needs-login", "passthrough:", "chottag trace on", "chottag daemon logs",
		"chottag daemon restart", "47821", "HTTPS_PROXY", "407", "/status",
	)
}

// TestFAQAnswersTheCommonQuestions pins the questions the docs review found
// unanswered (docs review §4).
func TestFAQAnswersTheCommonQuestions(t *testing.T) {
	requirePhrases(t, "docs/faq.md",
		"~/.claude", "CHOTTAG_HOME", "local CA", "Linux", "chottag auto off",
		"proxy.jsonl", "several machines",
	)
}

// TestKnownLimitationsListsTheSpecItems (spec §5): IDE and Desktop not
// routed, Max size, the RC banner, and the rest the docs review listed.
func TestKnownLimitationsListsTheSpecItems(t *testing.T) {
	requirePhrases(t, "docs/known-limitations.md",
		"IDE", "Desktop", "max5x", "chottag plan", "banner", "/status", "/usage",
		"prompt cache", "Linux", "route",
	)
}
