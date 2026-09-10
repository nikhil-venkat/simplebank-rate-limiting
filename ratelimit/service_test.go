package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// countAllowed spends n checks against addr and reports how many were allowed.
func countAllowed(service *Service, addr string, n int) int {
	allowed := 0
	for i := 0; i < n; i++ {
		if service.Allow(addr).Allowed {
			allowed++
		}
	}
	return allowed
}

func TestTokenBucketAllowsBurstThenDenies(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 3), WithClock(clock))

	for i := 0; i < 3; i++ {
		decision := service.Allow("1.2.3.4:1000")
		require.True(t, decision.Allowed, "request %d should be allowed within the burst", i+1)
	}

	decision := service.Allow("1.2.3.4:1000")
	require.False(t, decision.Allowed)
	require.Equal(t, 0, decision.Remaining)
	require.Greater(t, decision.RetryAfter, time.Duration(0))
	require.Equal(t, "default", decision.Rule)
}

func TestTokenBucketRefillsOverTime(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	// 10 per second means one token every 100ms.
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 2), WithClock(clock))

	require.Equal(t, 2, countAllowed(service, "1.2.3.4:1000", 2))
	require.False(t, service.Allow("1.2.3.4:1000").Allowed)

	clock.Advance(100 * time.Millisecond)
	require.True(t, service.Allow("1.2.3.4:1000").Allowed, "one token should have refilled")
	require.False(t, service.Allow("1.2.3.4:1000").Allowed, "only one token should have refilled")
}

func TestTokenBucketRetryAfterMatchesRefillTime(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 1), WithClock(clock))

	require.True(t, service.Allow("1.2.3.4:1000").Allowed)

	decision := service.Allow("1.2.3.4:1000")
	require.False(t, decision.Allowed)
	// Empty bucket refilling at 10/s: a full token is 100ms away.
	require.InDelta(t, float64(100*time.Millisecond), float64(decision.RetryAfter), float64(time.Millisecond))

	// Waiting exactly as long as we were told must actually work. If it did
	// not, the Retry-After we hand clients would send them into a retry loop.
	clock.Advance(decision.RetryAfter)
	require.True(t, service.Allow("1.2.3.4:1000").Allowed)
}

func TestDeniedRequestDoesNotConsumeBudget(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 1), WithClock(clock))

	require.True(t, service.Allow("1.2.3.4:1000").Allowed)

	// Hammer the empty bucket. If denials consumed tokens, the client would be
	// punished for retrying and could never recover on schedule.
	for i := 0; i < 50; i++ {
		require.False(t, service.Allow("1.2.3.4:1000").Allowed)
	}

	clock.Advance(100 * time.Millisecond)
	require.True(t, service.Allow("1.2.3.4:1000").Allowed)
}

func TestFixedWindowResetsAtBoundary(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmFixedWindow, 3, time.Second, 0), WithClock(clock))

	require.Equal(t, 3, countAllowed(service, "1.2.3.4:1000", 5))

	clock.Advance(time.Second)
	require.Equal(t, 3, countAllowed(service, "1.2.3.4:1000", 5), "counter should reset with the window")
}

func TestFixedWindowBoundaryBurstIsExpected(t *testing.T) {
	t.Parallel()

	// This documents the known trade-off rather than asserting a bug. A client
	// can spend a full budget at the end of one window and another at the
	// start of the next, so 2x the limit lands back to back. Anyone who
	// "fixes" this should have to update this test on purpose.
	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmFixedWindow, 3, time.Second, 0), WithClock(clock))

	require.Equal(t, 3, countAllowed(service, "1.2.3.4:1000", 3))
	clock.Advance(time.Second)
	require.Equal(t, 3, countAllowed(service, "1.2.3.4:1000", 3))
}

func TestFixedWindowReportsRetryAfter(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmFixedWindow, 1, time.Minute, 0), WithClock(clock))

	require.True(t, service.Allow("1.2.3.4:1000").Allowed)
	clock.Advance(20 * time.Second)

	decision := service.Allow("1.2.3.4:1000")
	require.False(t, decision.Allowed)
	require.Equal(t, 40*time.Second, decision.RetryAfter)
}

func TestPerIPIsolation(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 2), WithClock(clock))

	require.Equal(t, 2, countAllowed(service, "1.2.3.4:1000", 3))
	require.True(t, service.Allow("5.6.7.8:1000").Allowed, "a different IP has its own budget")
}

func TestPortIsStrippedFromBucketKey(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 2), WithClock(clock))

	// Every HTTP request can arrive on a fresh source port. If the port were
	// part of the key, each request would get its own bucket and the limiter
	// would never deny anything.
	require.True(t, service.Allow("1.2.3.4:1000").Allowed)
	require.True(t, service.Allow("1.2.3.4:2000").Allowed)
	require.False(t, service.Allow("1.2.3.4:3000").Allowed)
	require.Equal(t, 1, service.TrackedIPs())
}

func TestIPv6MappedAddressSharesBucket(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 1), WithClock(clock))

	require.True(t, service.Allow("127.0.0.1:1000").Allowed)
	require.False(t, service.Allow("[::ffff:127.0.0.1]:2000").Allowed, "the v6-mapped form is the same client")
	require.Equal(t, 1, service.TrackedIPs())
}

func TestIPv6AddressIsLimited(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 2), WithClock(clock))

	require.Equal(t, 2, countAllowed(service, "[2001:db8::1]:1000", 3))
	require.True(t, service.Allow("[2001:db8::2]:1000").Allowed)
}

func TestUnparseableAddressFailsOpen(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 1, time.Minute, 1), WithClock(clock))

	for i := 0; i < 5; i++ {
		decision := service.Allow("not-an-address")
		require.True(t, decision.Allowed, "a peer we cannot identify must not be dropped")
		require.Equal(t, reasonUnknownPeer, decision.Rule)
	}
}

func TestDisabledLimiterAllowsEverything(t *testing.T) {
	t.Parallel()

	service := NewService(&Config{Enabled: false}, WithClock(newFakeClock()))

	for i := 0; i < 100; i++ {
		decision := service.Allow("1.2.3.4:1000")
		require.True(t, decision.Allowed)
		require.Equal(t, reasonDisabled, decision.Rule)
	}
	require.Equal(t, 0, service.TrackedIPs())
}

func TestExemptIPIsNeverLimited(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled: true,
		Default: Rule{Requests: 1, Per: time.Minute, Burst: 1},
		Exempt:  []string{"10.0.0.0/8"},
	}
	require.NoError(t, config.Validate())

	service := NewService(config, WithClock(newFakeClock()))

	require.Equal(t, 100, countAllowed(service, "10.1.2.3:1000", 100))
	require.Equal(t, 1, countAllowed(service, "192.0.2.1:1000", 100), "a non-exempt IP is still limited")
}

func TestListedOnlyEnforcement(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled:     true,
		Enforcement: EnforcementListedOnly,
		Rules:       []Rule{{IP: "203.0.113.42", Requests: 2, Per: time.Minute, Burst: 2}},
	}
	require.NoError(t, config.Validate())

	service := NewService(config, WithClock(newFakeClock()))

	require.Equal(t, 2, countAllowed(service, "203.0.113.42:1000", 10), "a listed IP is limited")

	decision := service.Allow("198.51.100.9:1000")
	require.True(t, decision.Allowed, "an unlisted IP passes in listed_only mode")
	require.Equal(t, reasonUnlisted, decision.Rule)
}

func TestPerIPRuleOverridesDefault(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled: true,
		Default: Rule{Requests: 100, Per: time.Minute, Burst: 100},
		Rules:   []Rule{{IP: "203.0.113.42", Requests: 1, Per: time.Minute, Burst: 1}},
	}
	require.NoError(t, config.Validate())

	service := NewService(config, WithClock(newFakeClock()))

	require.Equal(t, 1, countAllowed(service, "203.0.113.42:1000", 10))
	require.Equal(t, 10, countAllowed(service, "198.51.100.9:1000", 10))
}

func TestSweeperReclaimsIdleBuckets(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 2), WithClock(clock))

	require.True(t, service.Allow("1.2.3.4:1000").Allowed)
	require.Equal(t, 1, service.TrackedIPs())

	require.Equal(t, 0, service.Sweep(), "a partly spent bucket still holds state")
	require.Equal(t, 1, service.TrackedIPs())

	// Long enough to refill completely, so there is nothing left to remember.
	clock.Advance(time.Second)
	require.Equal(t, 1, service.Sweep())
	require.Equal(t, 0, service.TrackedIPs())
}

func TestMaxTrackedIPsEvictsOldest(t *testing.T) {
	t.Parallel()

	config := &Config{
		Enabled:       true,
		Default:       Rule{Requests: 10, Per: time.Second, Burst: 5},
		MaxTrackedIPs: 10,
	}
	require.NoError(t, config.Validate())

	clock := newFakeClock()
	service := NewService(config, WithClock(clock))

	// The bucket map is an attack surface of its own: without a cap, a large
	// enough source range exhausts memory.
	for i := 1; i <= 50; i++ {
		service.Allow(fmt.Sprintf("198.51.100.%d:1000", i))
		clock.Advance(time.Millisecond)
	}

	require.LessOrEqual(t, service.TrackedIPs(), 10)
}

func TestRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 5),
		WithClock(newFakeClock()), WithSweepInterval(time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		require.Fail(t, "Run did not return after its context was cancelled")
	}
}

func TestConcurrentAccessNeverExceedsBurst(t *testing.T) {
	t.Parallel()

	const burst = 20
	const goroutines = 50
	const perGoroutine = 20

	// A frozen clock means no refill can happen mid-test, so the total number
	// of allowed requests must be exactly the burst. Run under -race.
	clock := newFakeClock()
	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, burst), WithClock(clock))

	var mu sync.Mutex
	allowed := 0

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if service.Allow("1.2.3.4:1000").Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	require.Equal(t, burst, allowed, "concurrent callers must not be able to overspend the bucket")
}

func TestConcurrentDistinctIPs(t *testing.T) {
	t.Parallel()

	service := NewService(testConfig(AlgorithmTokenBucket, 10, time.Second, 1), WithClock(newFakeClock()))

	// Exercises the create-on-first-sight path from many goroutines at once.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			service.Allow(fmt.Sprintf("198.51.100.%d:1000", i%256))
		}(i)
	}
	wg.Wait()

	require.LessOrEqual(t, service.TrackedIPs(), 100)
	require.Greater(t, service.TrackedIPs(), 0)
}
