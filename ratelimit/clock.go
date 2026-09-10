package ratelimit

import "time"

// Clock reports the current time. The service takes one so that tests can
// advance time deterministically instead of sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock returns the clock backed by the real wall clock.
func SystemClock() Clock { return systemClock{} }
