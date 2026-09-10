package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Reasons reported in Decision.Rule when no budget was consulted.
const (
	reasonDisabled    = "disabled"
	reasonExempt      = "exempt"
	reasonUnlisted    = "unlisted"
	reasonUnknownPeer = "unknown-peer"
)

const defaultSweepInterval = time.Minute

// Service is the shared rate limiter. One instance is created at startup and
// handed to both the HTTP middleware and the gRPC interceptor, so that a
// client's budget is the same budget no matter which port it arrives on.
//
// Every API shares one bucket per IP. There is deliberately no per-endpoint
// or per-method state: budget spent on one endpoint is unavailable to the
// rest, and no endpoint gets special treatment.
type Service struct {
	config *Config
	clock  Clock

	mu      sync.RWMutex
	buckets map[string]*entry

	sweepInterval time.Duration
}

type entry struct {
	mu       sync.Mutex
	bucket   bucket
	lastSeen time.Time
}

// Option customizes a Service. Used by tests to inject a clock.
type Option func(*Service)

// WithClock replaces the wall clock, so tests can advance time instead of
// sleeping.
func WithClock(clock Clock) Option {
	return func(s *Service) { s.clock = clock }
}

// WithSweepInterval sets how often idle buckets are reclaimed.
func WithSweepInterval(interval time.Duration) Option {
	return func(s *Service) { s.sweepInterval = interval }
}

// NewService builds a rate limiter from an already validated config.
func NewService(config *Config, opts ...Option) *Service {
	service := &Service{
		config:        config,
		clock:         systemClock{},
		buckets:       make(map[string]*entry),
		sweepInterval: defaultSweepInterval,
	}

	for _, opt := range opts {
		opt(service)
	}

	return service
}

// Enabled reports whether the limiter will do anything at all.
func (s *Service) Enabled() bool { return s.config.Enabled }

// Algorithm names the limiting strategy in use.
func (s *Service) Algorithm() string { return s.config.Algorithm }

// Allow checks one request from addr, which may be any form of peer address:
// "1.2.3.4:5678", "[::1]:5678", or a bare IP.
//
// This is the single entry point. The HTTP middleware and the gRPC interceptor
// both call it and do nothing else.
func (s *Service) Allow(addr string) Decision {
	if !s.config.Enabled {
		return Decision{Allowed: true, Rule: reasonDisabled}
	}

	ip, key := NormalizeIP(addr)
	if ip == nil {
		// We could not tell who this is, so we cannot fairly limit them.
		// Fail open: a limiter that drops traffic it does not understand is
		// an outage waiting to happen. The caller logs this.
		return Decision{Allowed: true, Rule: reasonUnknownPeer}
	}

	rule, exempt, limited := s.config.match(ip)
	if exempt {
		return Decision{Allowed: true, Rule: reasonExempt}
	}
	if !limited {
		return Decision{Allowed: true, Rule: reasonUnlisted}
	}

	now := s.clock.Now()
	target := s.entryFor(key, rule, now)

	target.mu.Lock()
	defer target.mu.Unlock()

	target.lastSeen = now
	return target.bucket.allow(now)
}

// entryFor returns the bucket for key, creating it on first sight.
func (s *Service) entryFor(key string, rule Rule, now time.Time) *entry {
	s.mu.RLock()
	existing, ok := s.buckets[key]
	s.mu.RUnlock()
	if ok {
		return existing
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Another goroutine may have created it between the two locks.
	if existing, ok := s.buckets[key]; ok {
		return existing
	}

	// The bucket map is itself an attack surface: one entry per source IP
	// means a large enough source range can exhaust memory. Make room before
	// growing past the cap.
	if len(s.buckets) >= s.config.MaxTrackedIPs {
		s.evictOldestLocked(len(s.buckets) - s.config.MaxTrackedIPs + 1)
	}

	created := &entry{bucket: newBucket(s.config.Algorithm, rule), lastSeen: now}
	s.buckets[key] = created
	return created
}

// evictOldestLocked drops the n least recently seen buckets. s.mu must be held
// for writing.
func (s *Service) evictOldestLocked(n int) {
	for ; n > 0; n-- {
		var oldestKey string
		var oldestSeen time.Time

		for key, candidate := range s.buckets {
			if oldestKey == "" || candidate.lastSeen.Before(oldestSeen) {
				oldestKey, oldestSeen = key, candidate.lastSeen
			}
		}

		if oldestKey == "" {
			return
		}
		delete(s.buckets, oldestKey)
	}
}

// TrackedIPs reports how many buckets are currently held. Useful as a metric
// and as the early warning for the memory case above.
func (s *Service) TrackedIPs() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.buckets)
}

// Run reclaims idle buckets until ctx is cancelled. Call it in a goroutine at
// startup. It is safe to skip entirely; the cap in entryFor still bounds
// memory, the sweeper just keeps the map small in the normal case.
func (s *Service) Run(ctx context.Context) error {
	if !s.config.Enabled {
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(s.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.Sweep()
		}
	}
}

// Sweep drops every bucket that has refilled to capacity. A client that has
// been quiet long enough to fully replenish has no state worth remembering,
// so recreating its bucket on the next request is free.
func (s *Service) Sweep() int {
	now := s.clock.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for key, candidate := range s.buckets {
		candidate.mu.Lock()
		idle := candidate.bucket.replenished(now)
		candidate.mu.Unlock()

		if idle {
			delete(s.buckets, key)
			removed++
		}
	}

	return removed
}
