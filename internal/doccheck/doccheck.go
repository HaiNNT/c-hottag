// Package doccheck reads the project's Markdown docs for the tests that keep
// them true to the code (part 3, the docs pack): headings and their sections,
// fenced blocks, inline code, links, GitHub heading anchors, and a comparison
// of a documented JSON example with the Go type it documents.
//
// It is test support. No product package imports it, so it never reaches the
// chottag binary.
package doccheck

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Section is one ATX heading ("#" to "######") outside fenced code, with the
// lines after it up to the next heading of the same or a higher level.
type Section struct {
	Level int
	Title string // the heading text, trimmed; inline code keeps its backticks
	Line  int    // 1-based line number of the heading
	Body  string // up to the next heading of the same or a higher level
	Lead  string // Body up to its first subheading: the section's own text
}

// isFence reports whether line opens or closes a fenced block. The docs use
// ``` fences only.
func isFence(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "```")
}

// Sections returns every heading of md, in order.
func Sections(md string) []Section {
	lines := strings.Split(md, "\n")
	type head struct {
		level, idx int
		title      string
	}
	var heads []head
	fenced := false
	for i, l := range lines {
		if isFence(l) {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		n := 0
		for n < len(l) && l[n] == '#' {
			n++
		}
		if n == 0 || n > 6 || n >= len(l) || l[n] != ' ' {
			continue
		}
		title := strings.TrimSpace(l[n:])
		title = strings.TrimSpace(strings.TrimRight(title, "#"))
		heads = append(heads, head{level: n, idx: i, title: title})
	}
	out := make([]Section, 0, len(heads))
	for k, h := range heads {
		end := len(lines)
		for _, next := range heads[k+1:] {
			if next.level <= h.level {
				end = next.idx
				break
			}
		}
		lead := end
		if k+1 < len(heads) && heads[k+1].idx < end {
			lead = heads[k+1].idx
		}
		out = append(out, Section{
			Level: h.level,
			Title: h.title,
			Line:  h.idx + 1,
			Body:  strings.Join(lines[h.idx+1:end], "\n"),
			Lead:  strings.Join(lines[h.idx+1:lead], "\n"),
		})
	}
	return out
}

// Find returns the first section at level whose Title is title.
func Find(md string, level int, title string) (Section, bool) {
	for _, s := range Sections(md) {
		if s.Level == level && s.Title == title {
			return s, true
		}
	}
	return Section{}, false
}

// Fenced returns the bodies of md's fenced blocks whose info string starts
// with lang ("json", "sh", "mermaid"), in order; lang "" takes every block.
func Fenced(md, lang string) []string {
	var out, cur []string
	in, take := false, false
	for _, l := range strings.Split(md, "\n") {
		if isFence(l) {
			if in {
				if take {
					out = append(out, strings.Join(cur, "\n"))
				}
				in, cur = false, nil
				continue
			}
			in = true
			info := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "```"))
			take = lang == "" || (len(info) > 0 && info[0] == lang)
			continue
		}
		if in {
			cur = append(cur, l)
		}
	}
	return out
}

// inlineCode is one inline code span inside a paragraph. A span may wrap a
// line (CommonMark folds the line break to a space when it renders), so it
// is matched against a whole paragraph, never one source line at a time.
var inlineCode = regexp.MustCompile("`([^`]+)`")

// lineBreak is a span's own line break with the indentation around it:
// what CommonMark folds to one space.
var lineBreak = regexp.MustCompile(`[ \t]*\n[ \t]*`)

// paragraph is a run of non-blank lines outside fenced blocks.
type paragraph struct {
	lines []string
	first int // 1-based line number of lines[0]
}

// walk splits md into fenced blocks and paragraphs, in order. fence gets
// each fenced block's info string and its lines; para gets each paragraph.
func walk(md string, fence func(info string, lines []string), para func(paragraph)) {
	var cur paragraph
	flush := func() {
		if len(cur.lines) > 0 {
			para(cur)
		}
		cur = paragraph{}
	}
	var info string
	var body []string
	fenced := false
	for i, l := range strings.Split(md, "\n") {
		if isFence(l) {
			flush()
			if fenced {
				fence(info, body)
				body = nil
			} else {
				info = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "```"))
			}
			fenced = !fenced
			continue
		}
		if fenced {
			body = append(body, l)
			continue
		}
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		if len(cur.lines) == 0 {
			cur.first = i + 1
		}
		cur.lines = append(cur.lines, l)
	}
	if fenced {
		fence(info, body) // an unclosed fence runs to the end, as CommonMark reads it
	}
	flush()
}

// Code returns the lines of md's fenced blocks and the text of its inline
// code spans: where a command a reader runs is written. A ```mermaid fence
// is a diagram, not a command a reader runs, so its lines are skipped;
// every other fence (json included) is still returned in full. A span that
// wraps a line comes back with that line break folded to one space.
func Code(md string) []string {
	var out []string
	walk(md, func(info string, lines []string) {
		if f := strings.Fields(info); len(f) > 0 && f[0] == "mermaid" {
			return
		}
		out = append(out, lines...)
	}, func(p paragraph) {
		for _, m := range inlineCode.FindAllStringSubmatch(strings.Join(p.lines, "\n"), -1) {
			out = append(out, lineBreak.ReplaceAllString(m[1], " "))
		}
	})
	return out
}

// Link is one inline Markdown link or image outside code. A link's text and
// target must sit on one line; the docs never break one.
type Link struct {
	Text   string
	Target string
	Line   int // 1-based
	Image  bool
}

var linkRE = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// Links returns md's inline links and images outside fenced blocks and
// inline code spans, in order. Spans are found a paragraph at a time, the
// same way Code finds them, so a span that wraps a line hides the link-like
// text inside it, and never the real link after it.
func Links(md string) []Link {
	var out []Link
	walk(md, func(string, []string) {}, func(p paragraph) {
		text := []byte(strings.Join(p.lines, "\n"))
		for _, loc := range inlineCode.FindAllIndex(text, -1) {
			for i := loc[0]; i < loc[1]; i++ {
				if text[i] != '\n' {
					text[i] = ' '
				}
			}
		}
		for i, l := range strings.Split(string(text), "\n") {
			for _, m := range linkRE.FindAllStringSubmatch(l, -1) {
				out = append(out, Link{Text: m[2], Target: m[3], Line: p.first + i, Image: m[1] == "!"})
			}
		}
	})
	return out
}

// Anchor is the id GitHub gives a heading: lower case; every character that
// is not a letter, a digit, a space, a hyphen or an underscore dropped; each
// space turned into a hyphen. MkDocs' default slug agrees for the plain
// ASCII headings these docs use.
func Anchor(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Anchors returns every heading anchor of md. A repeated anchor gets
// GitHub's "-1", "-2", … suffix, in order.
func Anchors(md string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	for _, s := range Sections(md) {
		a := Anchor(s.Title)
		if n := seen[a]; n > 0 {
			out[fmt.Sprintf("%s-%d", a, n)] = true
		} else {
			out[a] = true
		}
		seen[a]++
	}
	return out
}

var (
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// leaf reports whether Shape accepts any JSON value for t: a type that
// marshals itself (time.Time included), and every kind that is not a
// struct, map, slice, array or pointer.
func leaf(t reflect.Type) bool {
	for _, m := range []reflect.Type{marshalerType, textMarshalerType} {
		if t.Implements(m) || reflect.PointerTo(t).Implements(m) {
			return true
		}
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array, reflect.Pointer:
		return false
	}
	return true
}

type field struct {
	name string
	typ  reflect.Type
}

// jsonFields lists t's fields as encoding/json names them: the json tag's
// name, else the Go name; "-" and unexported fields skipped; an untagged
// embedded struct's fields promoted.
func jsonFields(t reflect.Type) []field {
	var out []field
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			et := ft
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				out = append(out, jsonFields(et)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, field{name: name, typ: ft})
	}
	return out
}

func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a bool"
	}
	return fmt.Sprintf("%T", v)
}

func at(path string) string {
	if path == "" {
		return "(document)"
	}
	return path
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// Shape compares doc, a JSON example decoded by encoding/json into an any,
// with the Go type t, and returns one line per difference, each starting
// with the path of the value it is about (e.g. "accounts[0].usage"):
//   - A struct is an object whose keys are exactly its JSON field names. A
//     missing key is an "undocumented field", an extra one "no such field".
//     hidden holds "Type.field" entries (the Go type's name, the JSON
//     field's name) that the command never outputs: neither required nor
//     allowed.
//   - A pointer is its element. A slice or an array is a JSON array; one
//     whose elements are not leaves must show at least one element, and
//     every element shown is checked. A map is an object whose values are
//     each checked; its keys are free.
//   - Leaves take any JSON value, null included.
//   - A null where an object or an array is wanted is a difference: show it.
func Shape(doc any, t reflect.Type, hidden map[string]bool) []string {
	var out []string
	shape(doc, t, "", hidden, &out)
	return out
}

func shape(doc any, t reflect.Type, path string, hidden map[string]bool, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if leaf(t) {
		return
	}
	if (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && t.Elem().Kind() == reflect.Uint8 {
		return // []byte marshals as a string
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := doc.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: want an object (%s), got %s", at(path), t.Name(), kindOf(doc)))
			return
		}
		known := map[string]bool{}
		for _, f := range jsonFields(t) {
			known[f.name] = true
			if hidden[t.Name()+"."+f.name] {
				continue
			}
			v, ok := obj[f.name]
			if !ok {
				*out = append(*out, fmt.Sprintf("%s: undocumented field %q (%s)", at(path), f.name, t.Name()))
				continue
			}
			shape(v, f.typ, join(path, f.name), hidden, out)
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch {
			case !known[k]:
				*out = append(*out, fmt.Sprintf("%s: no such field %q (%s)", at(path), k, t.Name()))
			case hidden[t.Name()+"."+k]:
				*out = append(*out, fmt.Sprintf("%s: field %q is never output (%s)", at(path), k, t.Name()))
			}
		}
	case reflect.Map:
		obj, ok := doc.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: want an object (%s), got %s", at(path), t, kindOf(doc)))
			return
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			shape(obj[k], t.Elem(), join(path, k), hidden, out)
		}
	case reflect.Slice, reflect.Array:
		arr, ok := doc.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: want an array (%s), got %s", at(path), t, kindOf(doc)))
			return
		}
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if len(arr) == 0 && !leaf(elem) {
			*out = append(*out, fmt.Sprintf("%s: empty array: show one element (%s)", at(path), t))
		}
		for i, v := range arr {
			shape(v, t.Elem(), fmt.Sprintf("%s[%d]", path, i), hidden, out)
		}
	}
}
