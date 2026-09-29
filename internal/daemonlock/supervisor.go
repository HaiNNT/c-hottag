package daemonlock

import "strconv"

// detectSupervisor reports which service manager started this process, if
// any: ("systemd", ""), ("launchd", <job label>), or ("", "").
//
// Every variable a supervisor sets is also inherited by everything started
// from inside what it supervises (F137). Every macOS Terminal shell carries
// XPC_SERVICE_NAME=0. A terminal emulator that is itself a systemd user
// unit gives its shells INVOCATION_ID. The shim passes its whole
// environment to the daemon it spawns. So "is the variable set" would mark
// nearly every daemon as supervised, and `daemon stop` would refuse (exit
// 3) forever. Each rule therefore also checks that the variable was set
// for THIS process:
//
//   - systemd sets SYSTEMD_EXEC_PID to the pid it exec'd (since v248).
//     When that is present it must be ours. When it is absent (an older
//     systemd), INVOCATION_ID alone decides. internal/shim's
//     daemonSpawnEnv strips both from a spawned daemon, so that fallback
//     cannot misfire on the shim's own path.
//   - A launchd job is launchd's direct child, so its parent pid is 1, and
//     its XPC_SERVICE_NAME is its job label, never "0".
func detectSupervisor(getenv func(string) string, pid, ppid int) (supervisor, label string) {
	if getenv("INVOCATION_ID") != "" {
		if execPID := getenv("SYSTEMD_EXEC_PID"); execPID == "" || execPID == strconv.Itoa(pid) {
			return "systemd", ""
		}
	}
	if name := getenv("XPC_SERVICE_NAME"); name != "" && name != "0" && ppid == 1 {
		return "launchd", name
	}
	return "", ""
}
