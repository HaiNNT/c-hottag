package cli

import "time"

// untilText renders a limit's reset time for display. A bare "15:04" reads
// as today; F167 found `tag` warning "limited until 18:00" for a reset two
// days out, while `status` (status.go) always spells the date. untilText
// keeps the short form when it can't mislead — t on now's local calendar
// day — and otherwise uses status's own "Jan 2 15:04" layout.
func untilText(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
