package proxy

import (
	"errors"
	"regexp"
)

// maxUserAgentLen bounds the User-Agent ClaudeCLIVersion reads. Claude
// Code's own is about 40 bytes ("claude-cli/2.1.282 (external, cli)"); a
// longer value is not one and is never parsed (ruling 2 / F184).
const maxUserAgentLen = 256

// claudeCLIUserAgent is M2c spec §3's anchored rule, with each version part
// bounded to 6 digits, so a crafted value can never put a long "version"
// into status.json (F24: persist an enum-like value or nothing).
var claudeCLIUserAgent = regexp.MustCompile(`^claude-cli/([0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6})[ (]`)

// ClaudeCLIVersion returns the Claude Code version a User-Agent names, and
// false for any value that is not Claude Code's. Only the captured version
// ever leaves this function.
func ClaudeCLIVersion(ua string) (string, bool) {
	if len(ua) > maxUserAgentLen {
		return "", false
	}
	m := claudeCLIUserAgent.FindStringSubmatch(ua)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// noteClaudeVersion hands a traced request's Claude Code version to
// OnClaudeVersion. A panic there is recovered and reported as a fixed
// string: it must never close the client's MITM connection, and the panic
// value may carry what the callback was given.
func (s *Server) noteClaudeVersion(ua string) {
	if s.cfg.OnClaudeVersion == nil {
		return
	}
	v, ok := ClaudeCLIVersion(ua)
	if !ok {
		return
	}
	defer func() {
		if recover() != nil && s.cfg.OnLogError != nil {
			s.cfg.OnLogError(errors.New("OnClaudeVersion callback panicked"))
		}
	}()
	s.cfg.OnClaudeVersion(v)
}
