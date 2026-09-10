package ratelimit

import "time"

// Decision is the outcome of a single rate limit check. It carries everything
// the HTTP and gRPC adapters need to build a response, so that neither adapter
// has to know which algorithm produced it.
type Decision struct {
	// Allowed reports whether the caller may proceed.
	Allowed bool
	// Rule names the config entry that produced this decision: an IP or CIDR
	// from the rules list, or one of "default", "exempt", "unlisted",
	// "disabled", "unknown-peer".
	Rule string
	// Limit is how many requests may be made back to back before the client
	// is throttled: the bucket capacity for token_bucket, the per-window
	// allowance for fixed_window. Remaining counts down from it, so the two
	// are always on the same scale.
	Limit int
	// Window is the period the configured rate applies to.
	Window time.Duration
	// Remaining is the budget left after this check. Always 0 when denied.
	Remaining int
	// RetryAfter is how long the caller must wait. Always 0 when allowed.
	RetryAfter time.Duration
	// ResetAt is when capacity returns. Zero when allowed.
	ResetAt time.Time
}

// bucket is one client's budget.
//
// Implementations do no locking of their own. The service serializes every
// call against a given bucket, which keeps the algorithms small enough to
// read in one sitting.
type bucket interface {
	// allow spends one unit of budget if there is any, and reports what
	// happened.
	allow(now time.Time) Decision
	// replenished reports whether the bucket is back to full capacity. A
	// replenished bucket carries no state worth keeping, so the sweeper is
	// free to drop it.
	replenished(now time.Time) bool
}

func newBucket(algorithm string, rule Rule) bucket {
	if algorithm == AlgorithmFixedWindow {
		return newFixedWindow(rule)
	}
	return newTokenBucket(rule)
}
