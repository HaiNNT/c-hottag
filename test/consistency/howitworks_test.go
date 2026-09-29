package consistency

import (
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// TestHowItWorksDrawsTheRequestPath: the page has a Mermaid diagram (P3-R4:
// no image files) and names every part of the request path and every
// route class the router uses.
func TestHowItWorksDrawsTheRequestPath(t *testing.T) {
	const page = "docs/how-it-works.md"
	if len(doccheck.Fenced(read(t, page), "mermaid")) == 0 {
		t.Errorf("%s has no ```mermaid diagram", page)
	}
	requirePhrases(t, page,
		"CLAUDE_CONFIG_DIR", "HTTPS_PROXY", "NODE_EXTRA_CA_CERTS", "127.0.0.1",
		"api.anthropic.com", "mcp-proxy.anthropic.com", "owners.json", "chottag own",
		"CHOTTAG_BYPASS",
	)
	classes := stringConsts(t, "internal/router", "Class")
	if len(classes) == 0 {
		t.Fatal("internal/router declares no Class constants")
	}
	text := read(t, page)
	for _, c := range classes {
		if !strings.Contains(text, "`"+c+"`") {
			t.Errorf("%s does not name the route class `%s`", page, c)
		}
	}
}

// TestSecurityDocSummarisesTheModel: docs/security.md is the reader's
// summary; SECURITY.md stays the policy, linked by its absolute URL.
func TestSecurityDocSummarisesTheModel(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	requirePhrases(t, "docs/security.md",
		"https://github.com/"+repo+"/blob/main/SECURITY.md",
		"127.0.0.1", "proxy.secret", "407", "421", "ca.key", "anthropic.com",
		"proxy.jsonl", "private vulnerability reporting", "gh attestation verify",
		"claude.ai", "claude.com", "name-constrained",
		"## Limits", "system trust store",
	)
}
