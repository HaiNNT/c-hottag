package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// usageLedgerCheck is the usage ledger's health row (R203): $CHOTTAG_HOME/usage
// is writable and when transcripts were last imported. Never a problem: the
// ledger is an optional record, so a trouble is info with a hint. The check
// reads modes and times only, never a ledger line, and writes nothing.
func usageLedgerCheck() Check {
	return Check{ID: "usage-ledger", Detect: func(e *Env) Finding {
		dir := filepath.Join(e.Home, "usage")
		di, err := os.Stat(dir)
		if os.IsNotExist(err) {
			return Finding{Status: StatusOK, Detail: "no usage ledger yet; the daemon creates it when it starts"}
		}
		if err != nil {
			return Finding{Status: StatusInfo, Detail: fmt.Sprintf("usage ledger unreadable: %v", err), Hint: "rm -r " + dir + " (token history is rebuilt from Claude Code's transcripts on the next chottag stats, but utilization history is lost for good)"}
		}
		if !di.IsDir() || di.Mode().Perm()&0o200 == 0 {
			return Finding{Status: StatusInfo, Detail: "usage ledger directory is not writable, so no usage is being recorded", Hint: "chmod u+w " + dir}
		}
		fi, err := os.Stat(filepath.Join(dir, "import.json"))
		if err != nil {
			return Finding{Status: StatusInfo, Detail: "usage ledger is writable; no history imported yet", Hint: "chottag stats"}
		}
		return Finding{Status: StatusOK, Detail: "usage ledger is writable; last transcript import " + fi.ModTime().Local().Format(time.DateTime)}
	}}
}
