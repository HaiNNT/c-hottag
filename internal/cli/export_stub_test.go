package cli

import "testing"

// StubDaemonNotifierForTest lets the external test package (cli_test) stub the
// daemon's notifier, as the internal tests do with stubDaemonNotifier.
func StubDaemonNotifierForTest(t *testing.T) { stubDaemonNotifier(t) }
