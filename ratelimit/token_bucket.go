package ratelimit

import (
	"time"

	"golang.org/x/time/rate"
)

// tokenBucket refills at a steady rate up to a fixed capacity. A request
// spends one token, or is rejected when the bucket is empty.
//
// It is the default because it has no window boundary to game and because a
// rejected request can be told exactly how long it needs to wait: the time it
// takes to refill the tokens it is short by.
type tokenBucket struct {
	limiter *rate.Limiter
	rule    string
	limit   int
	window  time.Duration
	rps     float64
}

func newTokenBucket(rule Rule) *tokenBucket {
	rps := rule.ratePerSecond()
	return &tokenBucket{
		limiter: rate.NewLimiter(rate.Limit(rps), rule.Burst),
		rule:    rule.Name(),
		limit:   rule.Burst,
		window:  rule.Per,
		rps:     rps,
	}
}

func (b *tokenBucket) allow(now time.Time) Decision {
	decision := Decision{
		Rule:   b.rule,
		Limit:  b.limit,
		Window: b.window,
	}

	// Read the token count before spending, so that a denial can report the
	// exact deficit. AllowN leaves the limiter untouched when it returns
	// false, so this never consumes budget on a rejected request.
	tokens := b.limiter.TokensAt(now)

	if b.limiter.AllowN(now, 1) {
		decision.Allowed = true
		decision.Remaining = int(b.limiter.TokensAt(now))
		return decision
	}

	// Short by (1 - tokens), refilling at rps tokens per second.
	retryAfter := time.Duration((1 - tokens) / b.rps * float64(time.Second))
	if retryAfter <= 0 {
		retryAfter = time.Duration(float64(time.Second) / b.rps)
	}

	decision.RetryAfter = retryAfter
	decision.ResetAt = now.Add(retryAfter)
	return decision
}

func (b *tokenBucket) replenished(now time.Time) bool {
	return b.limiter.TokensAt(now) >= float64(b.limiter.Burst())
}
