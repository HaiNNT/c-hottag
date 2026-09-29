package tracelog

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var authSchemeAllowlist = map[string]bool{
	"basic":     true,
	"digest":    true,
	"bearer":    true,
	"negotiate": true,
	"ntlm":      true,
}

// AuthKind names the kind of credential a request carries, never its value.
func AuthKind(authorization string, hasAPIKey bool) string {
	if authorization == "" {
		if hasAPIKey {
			return "x-api-key"
		}
		return "none"
	}
	scheme, cred, found := strings.Cut(authorization, " ")
	lowerScheme := strings.ToLower(scheme)
	if !found || !authSchemeAllowlist[lowerScheme] {
		if IsSecret(authorization) || IsSecret(cred) {
			return "secret"
		}
		return "other"
	}
	if !strings.EqualFold(scheme, "Bearer") {
		return lowerScheme
	}
	switch {
	case strings.HasPrefix(cred, "sk-ant-oat"):
		return "oauth-access"
	case strings.HasPrefix(cred, "sk-ant-ort"):
		return "oauth-refresh"
	case strings.HasPrefix(cred, "sk-ant-api"):
		return "api-key"
	case strings.HasPrefix(cred, "eyJ"):
		return "jwt"
	default:
		return "bearer-other"
	}
}

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	idCharRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	digitsRe = regexp.MustCompile(`^[0-9]+$`)
)

// IsSecret reports values that must never be logged or hashed.
func IsSecret(s string) bool {
	return strings.HasPrefix(s, "sk-ant-") || strings.HasPrefix(s, "eyJ") || len(s) >= 100
}

// IsID reports values that look like object identifiers.
func IsID(s string) bool {
	if IsSecret(s) {
		return false
	}
	if uuidRe.MatchString(s) || digitsRe.MatchString(s) {
		return true
	}
	return idCharRe.MatchString(s) && strings.ContainsAny(s, "0123456789") &&
		strings.IndexFunc(s, func(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }) >= 0
}

var (
	idKeyLongHexRe   = regexp.MustCompile(`^[0-9a-fA-F]{12,}$`)
	idKeyLongDigitRe = regexp.MustCompile(`^[0-9]{6,}$`)
	// idKeyAlnumRe matches a ULID/base62-style id, optionally type-prefixed
	// (e.g. "mcpsrv_01H…"), capturing the alphanumeric tail.
	idKeyAlnumRe = regexp.MustCompile(`^(?:[a-z]{2,16}_)?([A-Za-z0-9]{16,})$`)
)

// isIDKey reports whether a JSON object key looks like an identifier (a
// UUID, a path/URI/email-like value containing "/" or "@", a long hex or
// digit run, or a ULID/base62 id optionally type-prefixed) rather than a
// stable field or feature-flag name. Shapes are types-only, and object
// keys are collapsed through this rule before their value is shaped.
func isIDKey(k string) bool {
	if strings.ContainsAny(k, "/@") {
		return true
	}
	if uuidRe.MatchString(k) {
		return true
	}
	if idKeyLongHexRe.MatchString(k) || idKeyLongDigitRe.MatchString(k) {
		return true
	}
	if m := idKeyAlnumRe.FindStringSubmatch(k); m != nil {
		tail := m[1]
		digits, hasLetter := 0, false
		for _, r := range tail {
			switch {
			case r >= '0' && r <= '9':
				digits++
			case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
				hasLetter = true
			}
		}
		return digits >= 3 && hasLetter
	}
	return false
}

// idPrefixRe matches an id's lower-case type prefix, e.g. "cse_" in "cse_01H…".
var idPrefixRe = regexp.MustCompile(`^([a-z]{2,16}_)(.+)$`)

// HashID is a short, stable, non-reversible handle for correlating ids. A
// lower-case type prefix such as "cse_" stays in clear and only the rest is
// hashed, so ids that differ only by prefix (cse_X vs session_X) can be
// recognised as the same object.
func HashID(s string) string {
	if m := idPrefixRe.FindStringSubmatch(s); m != nil {
		return m[1] + hash8(m[2])
	}
	return hash8(s)
}

func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// SplitIDLabel splits a HashID result into its clear prefix ("" if none) and
// its hash.
func SplitIDLabel(label string) (prefix, hash string) {
	if i := strings.LastIndexByte(label, '_'); i >= 0 {
		return label[:i+1], label[i+1:]
	}
	return "", label
}

// TemplatePath replaces id-like segments with {id} and secret-like ones with
// {secret}, returning the hashes of the replaced ids in order.
func TemplatePath(p string) (string, []string) {
	segs := strings.Split(p, "/")
	var ids []string
	for i, s := range segs {
		switch {
		case s == "":
		case IsSecret(s):
			segs[i] = "{secret}"
		case IsID(s):
			segs[i] = "{id}"
			ids = append(ids, HashID(s))
		}
	}
	return strings.Join(segs, "/"), ids
}

// queryValueAllowlist names the only query keys whose value may be logged
// inline as "k=v"; every other key is logged bare (name only).
var queryValueAllowlist = map[string]bool{
	"beta":  true,
	"limit": true,
}

// QueryKeys lists query keys, sorted. A value is only ever inlined as "k=v"
// (e.g. "beta=true") for allowlisted keys; every other key is logged bare,
// and any value that looks like a secret is never inlined regardless. A key
// itself is redacted to "{secret}" if it looks like a secret: a raw token
// passed with no "=" (e.g. "?sk-ant-oat01-...") is the query key, not a
// value, but must be caught all the same.
func QueryKeys(raw string) []string {
	q, _ := url.ParseQuery(raw)
	out := make([]string, 0, len(q))
	for k, vs := range q {
		if IsSecret(k) {
			out = append(out, "{secret}")
			continue
		}
		v := ""
		if len(vs) > 0 {
			v = vs[0]
		}
		if v != "" && queryValueAllowlist[k] && !IsSecret(v) {
			out = append(out, k+"="+v)
		} else {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
