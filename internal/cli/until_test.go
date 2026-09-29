package cli

import (
	"testing"
	"time"
)

// F167: a limit's reset time printed bare ("18:00") reads as today, even
// when the reset is days away. untilText must add the date whenever t
// isn't on now's local calendar day.
func TestUntilText(t *testing.T) {
	loc := time.Local
	cases := []struct {
		name string
		now  time.Time
		t    time.Time
		want string
	}{
		{
			name: "same day, 5 minutes ahead",
			now:  time.Date(2026, 9, 24, 17, 55, 0, 0, loc),
			t:    time.Date(2026, 9, 24, 18, 0, 0, 0, loc),
			want: "18:00",
		},
		{
			name: "tomorrow 00:30 when now is 23:50",
			now:  time.Date(2026, 9, 24, 23, 50, 0, 0, loc),
			t:    time.Date(2026, 9, 25, 0, 30, 0, 0, loc),
			want: "Sep 25 00:30",
		},
		{
			name: "2 days ahead",
			now:  time.Date(2026, 9, 24, 17, 55, 0, 0, loc),
			t:    time.Date(2026, 9, 26, 18, 0, 0, 0, loc),
			want: "Sep 26 18:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := untilText(tc.t, tc.now); got != tc.want {
				t.Errorf("untilText(%v, %v) = %q, want %q", tc.t, tc.now, got, tc.want)
			}
		})
	}
}
