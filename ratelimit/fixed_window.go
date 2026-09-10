package ratelimit

import "time"

// fixedWindow counts requests inside a wall-clock window and resets the count
// when the window rolls over.
//
// It is offered as an alternative because it is the easiest limiter to reason
// about during an incident: "you get N per minute, the counter resets on the
// minute". The trade-off is the boundary effect. A client can spend its whole
// budget at the very end of one window and again at the start of the next,
// so up to 2x the configured limit can land in a short span. That is a known
// property, not a bug. Use the token bucket if it matters.
type fixedWindow struct {
	rule   string
	limit  int
	window time.Duration

	windowStart time.Time
	count       int
}

func newFixedWindow(rule Rule) *fixedWindow {
	return &fixedWindow{
		rule:   rule.Name(),
		limit:  rule.Requests,
		window: rule.Per,
	}
}

func (w *fixedWindow) allow(now time.Time) Decision {
	w.roll(now)

	decision := Decision{
		Rule:   w.rule,
		Limit:  w.limit,
		Window: w.window,
	}

	if w.count < w.limit {
		w.count++
		decision.Allowed = true
		decision.Remaining = w.limit - w.count
		return decision
	}

	resetAt := w.windowStart.Add(w.window)
	decision.RetryAfter = resetAt.Sub(now)
	decision.ResetAt = resetAt
	return decision
}

func (w *fixedWindow) replenished(now time.Time) bool {
	w.roll(now)
	return w.count == 0
}

// roll advances to the current window, zeroing the counter if the previous one
// has expired. Windows are anchored to the first request rather than to
// absolute time, which keeps the arithmetic obvious and the tests readable.
func (w *fixedWindow) roll(now time.Time) {
	if w.windowStart.IsZero() || !now.Before(w.windowStart.Add(w.window)) {
		w.windowStart = now
		w.count = 0
	}
}
