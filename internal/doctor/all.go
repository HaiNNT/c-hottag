package doctor

// All is every check in spec §2.2's order. names are the registered
// accounts in registration order, one token check each.
func All(names []string) []Check {
	checks := InstallChecks()
	checks = append(checks, StateChecks()...)
	return append(checks, ReportChecks(names)...)
}
