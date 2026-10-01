package doccheck

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const sample = "# Title\n\nintro `chottag status`\n\n## A\n\ntext [b](#b) ![pic](x.png)\n\n```sh\n# not a heading\nchottag tag A\n```\n\n### A.1\n\n```json\n{\"k\": 1}\n```\n\n## B\n\n`[not](a-link.md)` and [real](docs/x.md)\n"

func TestSectionsSplitsByLevelAndSkipsFences(t *testing.T) {
	var got []string
	for _, s := range Sections(sample) {
		got = append(got, strings.Repeat("#", s.Level)+" "+s.Title)
	}
	want := []string{"# Title", "## A", "### A.1", "## B"}
	if !slices.Equal(got, want) {
		t.Fatalf("headings %q, want %q", got, want)
	}
	a, ok := Find(sample, 2, "A")
	if !ok || !strings.Contains(a.Body, "### A.1") || strings.Contains(a.Body, "## B") {
		t.Errorf("section A body %q: want it to hold A.1 and stop at B", a.Body)
	}
	if strings.Contains(a.Lead, "A.1") || !strings.Contains(a.Lead, "chottag tag A") {
		t.Errorf("section A lead %q: want its own text only", a.Lead)
	}
	if a.Line != 5 {
		t.Errorf("section A at line %d, want 5", a.Line)
	}
}

func TestFencedPicksTheLanguage(t *testing.T) {
	if got := Fenced(sample, "json"); len(got) != 1 || got[0] != "{\"k\": 1}" {
		t.Errorf("json blocks %q", got)
	}
	if got := Fenced(sample, ""); len(got) != 2 {
		t.Errorf("all blocks %q, want 2", got)
	}
}

func TestCodeReturnsFencesAndSpans(t *testing.T) {
	got := Code(sample)
	for _, want := range []string{"chottag status", "chottag tag A", "# not a heading", "[not](a-link.md)"} {
		if !slices.Contains(got, want) {
			t.Errorf("Code lacks %q: %q", want, got)
		}
	}
}

// TestCodeSkipsMermaidFences: a mermaid fence is a diagram, so a label such
// as "chottag shim" inside one is never read as a command; other fences,
// json included, keep their current treatment.
func TestCodeSkipsMermaidFences(t *testing.T) {
	md := "# T\n\n```mermaid\nflowchart LR\n  shim[\"chottag shim\"] --> daemon\n```\n\n```json\n{\"chottag\": 1}\n```\n\n```sh\nchottag tag A\n```\n"
	got := Code(md)
	for _, unwanted := range []string{"flowchart LR", "chottag shim", "shim[\"chottag shim\"] --> daemon"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("Code kept a mermaid line %q: %q", unwanted, got)
		}
	}
	for _, want := range []string{"{\"chottag\": 1}", "chottag tag A"} {
		if !slices.Contains(got, want) {
			t.Errorf("Code lacks non-mermaid fence line %q: %q", want, got)
		}
	}
}

// TestCodeFindsSpansThatWrapALine: CommonMark folds a code span's own line
// break to a space, so a span split across two source lines (finding 13)
// must still be found.
func TestCodeFindsSpansThatWrapALine(t *testing.T) {
	md := "# T\n\nsee `serving: X   remote:\n  Y` for the status line.\n"
	got := Code(md)
	want := "serving: X   remote: Y"
	if !slices.Contains(got, want) {
		t.Errorf("Code lacks the wrapped span %q: %q", want, got)
	}
}

func TestLinksSkipCode(t *testing.T) {
	var got []string
	for _, l := range Links(sample) {
		s := l.Target
		if l.Image {
			s = "!" + s
		}
		got = append(got, s)
	}
	want := []string{"#b", "!x.png", "docs/x.md"}
	if !slices.Equal(got, want) {
		t.Errorf("links %q, want %q", got, want)
	}
}

// TestLinksAndCodeAgreeOnSpansThatWrapALine: Links finds code spans the
// way Code does, a paragraph at a time (part 5). A span that wraps a line
// hides the link-like text inside it, and a per-line scan that paired the
// wrong backticks must not hide the real link after it.
func TestLinksAndCodeAgreeOnSpansThatWrapALine(t *testing.T) {
	md := "# T\n\nsee `a [fake](fake.md)\n  b` and [real](real.md)\n\ntick `one\ntwo` then [kept](kept.md) and `three`\n"
	var got []string
	for _, l := range Links(md) {
		got = append(got, fmt.Sprintf("%s:%d", l.Target, l.Line))
	}
	want := []string{"real.md:4", "kept.md:7"}
	if !slices.Equal(got, want) {
		t.Errorf("Links = %q, want %q", got, want)
	}
	code := Code(md)
	for _, w := range []string{"a [fake](fake.md) b", "one two", "three"} {
		if !slices.Contains(code, w) {
			t.Errorf("Code lacks %q: %q", w, code)
		}
	}
}

func TestAnchorMatchesGitHub(t *testing.T) {
	for in, want := range map[string]string{
		"`chottag tag`":        "chottag-tag",
		"What it does":         "what-it-does",
		"5h and 7d":            "5h-and-7d",
		"state.json":           "statejson",
		"For Claude Code":      "for-claude-code",
		"Errors: what to do":   "errors-what-to-do",
		"`chottag daemon run`": "chottag-daemon-run",
	} {
		if got := Anchor(in); got != want {
			t.Errorf("Anchor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnchorsNumbersDuplicates(t *testing.T) {
	got := Anchors("## Flags\n\n## Flags\n\n## Flags\n")
	for _, want := range []string{"flags", "flags-1", "flags-2"} {
		if !got[want] {
			t.Errorf("Anchors lacks %q: %v", want, got)
		}
	}
}

type inner struct {
	A string    `json:"a"`
	T time.Time `json:"t,omitzero"`
}

type Embedded struct {
	E int `json:"e"`
}

type outer struct {
	Embedded
	Name   string            `json:"name"`
	Ptr    *inner            `json:"ptr,omitempty"`
	List   []inner           `json:"list"`
	Tags   []string          `json:"tags"`
	ByName map[string]inner  `json:"byName"`
	Free   map[string]string `json:"free"`
	Secret string            `json:"-"`
	Dir    string            `json:"dir,omitempty"`
	//lint:ignore U1000 unexported on purpose: Shape must never ask for it
	lower int
}

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestShapeAcceptsACompleteExample(t *testing.T) {
	doc := decode(t, `{"e":1,"name":"x","ptr":{"a":"","t":"2026-01-01T00:00:00Z"},"list":[{"a":"","t":null}],"tags":[],"byName":{"k":{"a":"","t":""}},"free":{},"dir":""}`)
	if diffs := Shape(doc, reflect.TypeFor[outer](), nil); len(diffs) != 0 {
		t.Errorf("unexpected differences: %q", diffs)
	}
}

func TestShapeReportsMissingAndExtraFields(t *testing.T) {
	doc := decode(t, `{"e":1,"name":"x","ptr":{"a":"","t":"","bogus":1},"list":[{"t":""}],"tags":[],"byName":{},"free":{},"dir":"","extra":true}`)
	got := strings.Join(Shape(doc, reflect.TypeFor[outer](), nil), "\n")
	for _, want := range []string{
		`(document): no such field "extra" (outer)`,
		`ptr: no such field "bogus" (inner)`,
		`list[0]: undocumented field "a" (inner)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("differences lack %q:\n%s", want, got)
		}
	}
}

func TestShapeWantsObjectsArraysAndOneElement(t *testing.T) {
	doc := decode(t, `{"e":1,"name":"x","ptr":null,"list":[],"tags":"no","byName":{},"free":{},"dir":""}`)
	got := strings.Join(Shape(doc, reflect.TypeFor[outer](), nil), "\n")
	for _, want := range []string{"ptr: want an object", "list: empty array: show one element", "tags: want an array"} {
		if !strings.Contains(got, want) {
			t.Errorf("differences lack %q:\n%s", want, got)
		}
	}
}

func TestShapeHiddenFieldsAreNeitherRequiredNorAllowed(t *testing.T) {
	hidden := map[string]bool{"outer.dir": true}
	ok := decode(t, `{"e":1,"name":"x","ptr":{"a":"","t":""},"list":[{"a":"","t":""}],"tags":[],"byName":{},"free":{}}`)
	if diffs := Shape(ok, reflect.TypeFor[outer](), hidden); len(diffs) != 0 {
		t.Errorf("a hidden field was required: %q", diffs)
	}
	shown := decode(t, `{"e":1,"name":"x","ptr":{"a":"","t":""},"list":[{"a":"","t":""}],"tags":[],"byName":{},"free":{},"dir":"/x"}`)
	if diffs := strings.Join(Shape(shown, reflect.TypeFor[outer](), hidden), "\n"); !strings.Contains(diffs, `field "dir" is never output`) {
		t.Errorf("a shown hidden field passed: %q", diffs)
	}
}

type strictEmbedded struct {
	E inner `json:"e"`
}

type shadowOuter struct {
	strictEmbedded
	E string `json:"e"`
}

func TestShapeAnOuterFieldShadowsAPromotedOne(t *testing.T) {
	doc := decode(t, `{"e":"text"}`)
	if diffs := Shape(doc, reflect.TypeFor[shadowOuter](), nil); len(diffs) != 0 {
		t.Errorf("shadowed field was asked for twice: %q", diffs)
	}
}
