package sessname

import (
	"strings"
	"testing"
)

func TestParseModelReply(t *testing.T) {
	ok := func(r string) string { return `{"subtype":"success","is_error":false,"result":` + r + `}` }
	cases := []struct{ name, in, want string }{
		{"plain", ok(`"Add upload retry"`), "Add upload retry"},
		{"quoted", ok(`"\"Add upload retry\""`), "Add upload retry"},
		{"backticks", ok("\"`Fix billing test`\""), "Fix billing test"},
		{"trailing newline", ok(`"Fix billing test\n"`), "Fix billing test"},
		{"two lines", ok(`"Fix billing\ntest"`), ""},
		{"empty", ok(`" "`), ""},
		{"too long", ok(`"` + strings.Repeat("a", 61) + `"`), ""},
		{"nine words", ok(`"a b c d e f g h i"`), ""},
		{"api error", ok(`"API Error: failed"`), ""},
		{"echo", ok(`"Give a 3 to 6 word title"`), ""},
		{"is_error", `{"subtype":"success","is_error":true,"result":"Fix it"}`, ""},
		{"missing is_error", `{"subtype":"success","result":"Fix it"}`, ""},
		{"subtype", `{"subtype":"error","is_error":false,"result":"Fix it"}`, ""},
		{"not json", "Fix it", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseModelReply([]byte(c.in)); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestModelStdin(t *testing.T) {
	head := "Give a 3 to 6 word title for this task. Reply with the title only.\n\n"
	cases := []struct{ name, prompt, want string }{
		{"short", "Add a retry to the upload client", "Add a retry to the upload client"},
		{"exact", strings.Repeat("a", 1000), strings.Repeat("a", 1000)},
		{"cut", strings.Repeat("a", 1001), strings.Repeat("a", 1000)},
		{"runes", strings.Repeat("é", 1200), strings.Repeat("é", 1000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ModelStdin(c.prompt); got != head+c.want {
				t.Errorf("shape wrong (%d bytes)", len(got))
			}
		})
	}
}
