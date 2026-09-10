package ratelimit

import (
	"sync"
	"time"
)

// fakeClock lets tests advance time exactly, so no test ever sleeps. Sleeping
// makes the suite slow and turns a loaded CI runner into a flake source.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testConfig builds a validated config with one default budget and nothing else.
func testConfig(algorithm string, requests int, per time.Duration, burst int) *Config {
	config := &Config{
		Enabled:   true,
		Algorithm: algorithm,
		Default:   Rule{Requests: requests, Per: per, Burst: burst},
	}
	if err := config.Validate(); err != nil {
		panic(err)
	}
	return config
}
