package brand

import "testing"

func TestMarks(t *testing.T) {
	if Mark != "c»" || MarkASCII != "c>>" || MarkOneCell != "»" {
		t.Fatalf("marks: %q %q %q", Mark, MarkASCII, MarkOneCell)
	}
}

func TestPaint(t *testing.T) {
	cases := []struct {
		label string
		mode  ColorMode
		want  string
	}{
		{"", None, "x"},
		{"dev", None, "x"},
		{"", Color256, "\x1b[1;38;5;33mx\x1b[0m"},
		{"dev", Color256, "\x1b[1;38;5;214mx\x1b[0m"},
		{"", TrueColor, "\x1b[1;38;2;0;135;255mx\x1b[0m"},
		{"dev", TrueColor, "\x1b[1;38;2;255;175;0mx\x1b[0m"},
	}
	for _, c := range cases {
		if got := Paint("x", c.label, c.mode); got != c.want {
			t.Errorf("Paint(label=%q, mode=%d) = %q, want %q", c.label, c.mode, got, c.want)
		}
	}
}

func TestPaintLimited(t *testing.T) {
	if got := PaintLimited("x", None); got != "x" {
		t.Errorf("None: %q", got)
	}
	if got := PaintLimited("x", Color256); got != "\x1b[1;38;5;160mx\x1b[0m" {
		t.Errorf("256: %q", got)
	}
	if got := PaintLimited("x", TrueColor); got != "\x1b[1;38;2;215;0;0mx\x1b[0m" {
		t.Errorf("true: %q", got)
	}
}

func TestModeFromEnv(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want ColorMode
	}{
		{nil, Color256},
		{map[string]string{"NO_COLOR": "1"}, None},
		{map[string]string{"NO_COLOR": ""}, Color256},
		{map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}, None},
		{map[string]string{"COLORTERM": "truecolor"}, TrueColor},
		{map[string]string{"COLORTERM": "24bit"}, TrueColor},
		{map[string]string{"COLORTERM": "other"}, Color256},
	}
	for _, c := range cases {
		got := ModeFromEnv(func(k string) string { return c.env[k] })
		if got != c.want {
			t.Errorf("env %v = %d, want %d", c.env, got, c.want)
		}
	}
}
