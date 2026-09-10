package gapi

import (
	"context"
	"strconv"

	"github.com/rs/zerolog/log"
	"github.com/techschool/simplebank/ratelimit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// RateLimitInterceptor enforces the shared per-IP budget on the gRPC server.
//
// It is the port 9090 half of the pair. RateLimitMiddleware covers 8080, and
// both are given the same *ratelimit.Service, so a client cannot get a second
// allowance by switching protocols.
//
// Chain it ahead of GrpcLogger so rejected calls are turned away before any
// handler work happens.
func RateLimitInterceptor(limiter *ratelimit.Service) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		decision := limiter.Allow(peerAddress(ctx))

		if decision.Allowed {
			return handler(ctx, req)
		}

		seconds := retryAfterSeconds(decision.RetryAfter)
		// Best effort. The client still gets the RetryInfo detail on the
		// status itself, which is the authoritative source.
		_ = grpc.SetHeader(ctx, metadata.Pairs(
			headerRetryAfter, strconv.Itoa(seconds),
			headerRateLimitLimit, strconv.Itoa(decision.Limit),
			headerRateLimitRemaining, "0",
		))

		log.Warn().
			Str("protocol", "grpc").
			Str("client_ip", peerAddress(ctx)).
			Str("method", info.FullMethod).
			Str("rule", decision.Rule).
			Int("limit", decision.Limit).
			Dur("retry_after", decision.RetryAfter).
			Msg("rate limit exceeded")

		return nil, rateLimitError(decision).Err()
	}
}
