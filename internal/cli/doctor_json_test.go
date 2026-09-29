package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

func init() {
	registerJSONCases(
		jsonCase{
			name: "doctor on a healthy install", command: "doctor",
			setup: func(t *testing.T) []string { doctorInstall(t); return []string{"doctor"} },
			check: func(t *testing.T, doc map[string]any) {
				checks, _ := doc["checks"].([]any)
				if doc["problems"] != float64(0) || len(checks) != len(doctorIDs) {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "doctor with a missing bin link", command: "doctor",
			setup: func(t *testing.T) []string {
				h, _, _ := doctorInstall(t)
				if err := os.Remove(filepath.Join(h, "bin", "claude")); err != nil {
					t.Fatal(err)
				}
				return []string{"doctor"}
			},
			wantExit: exit.UserAction, wantCode: codeDoctorProblems,
		},
	)
}
