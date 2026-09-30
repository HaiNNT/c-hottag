// Package brand holds the chottag mark and the terminal colours that go with
// it. It is pure: no I/O.
package brand

import "fmt"

const (
	// Mark is the mark as terminal text (U+00BB is one cell).
	Mark = "c»"
	// MarkASCII is the fallback for terminals that cannot show Mark.
	MarkASCII = "c>>"
	// MarkOneCell is the mark where only one cell is free.
	MarkOneCell = "»"
)

// ColorMode says how much colour a terminal surface may use.
type ColorMode int

const (
	None ColorMode = iota
	Color256
	TrueColor
)

// ModeFromEnv picks the colour mode from the environment: a non-empty
// NO_COLOR means none, COLORTERM truecolor or 24bit means 24-bit, and
// anything else means 256 colours.
func ModeFromEnv(getenv func(string) string) ColorMode {
	if getenv("NO_COLOR") != "" {
		return None
	}
	switch getenv("COLORTERM") {
	case "truecolor", "24bit":
		return TrueColor
	}
	return Color256
}

func sgr(text string, mode ColorMode, c256 int, r, g, b int) string {
	switch mode {
	case TrueColor:
		return fmt.Sprintf("\x1b[1;38;2;%d;%d;%dm%s\x1b[0m", r, g, b, text)
	case Color256:
		return fmt.Sprintf("\x1b[1;38;5;%dm%s\x1b[0m", c256, text)
	}
	return text
}

// Paint wraps text in bold colour: blue for prod (label ""), amber for any
// other label. With mode None it returns text unchanged.
func Paint(text, label string, mode ColorMode) string {
	if label == "" {
		return sgr(text, mode, 33, 0x00, 0x87, 0xFF)
	}
	return sgr(text, mode, 214, 0xFF, 0xAF, 0x00)
}

// PaintLimited wraps text in bold red, for a limited account.
func PaintLimited(text string, mode ColorMode) string {
	return sgr(text, mode, 160, 0xD7, 0x00, 0x00)
}
