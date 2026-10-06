package creds

import "time"

// SetNowForTest swaps the backstop clock and returns a restore func.
func SetNowForTest(f func() time.Time) func() {
	old := nowFunc
	nowFunc = f
	return func() { nowFunc = old }
}
