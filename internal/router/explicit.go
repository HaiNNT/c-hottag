package router

// Explicit reports whether a rule other than the final default decides req:
// for the API host, any rule in apiRules (a request none matches is the
// Serving fallback); for the MCP proxy, the connector rule rather than the
// "^/" catch-all; for any other host, false. It shares Route's matching, so
// the two cannot disagree (R186).
func Explicit(req Request) bool {
	d, ru := match(req)
	if ru == nil || d.Fallback {
		return false
	}
	return !(HostOnly(req.Host) == MCPProxyHost && ru.re.String() == mcpCatchAll)
}

// mcpCatchAll is the pattern of mcpProxyRules' last, catch-all rule.
const mcpCatchAll = `^/`
