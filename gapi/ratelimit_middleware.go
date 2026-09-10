package gapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/techschool/simplebank/ratelimit"
	"google.golang.org/protobuf/encoding/protojson"
)

// Headers set on rate limited responses.
const (
	headerRetryAfter         = "Retry-After"
	headerRateLimitLimit     = "X-RateLimit-Limit"
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRateLimitReset     = "X-RateLimit-Reset"
)

// rateLimitMarshaler mirrors the marshaler the gateway is configured with in
// main.go, so a 429 written here is shaped exactly like an error written by
// grpc-gateway and clients need only one error parser.
var rateLimitMarshaler = protojson.MarshalOptions{UseProtoNames: true}

// RateLimitMiddleware enforces the shared per-IP budget on the HTTP gateway.
//
// This middleware is not an optimization, it is required for coverage. The
// gateway is registered with RegisterSimpleBankHandlerServer, the in-process
// variant, so an HTTP request calls the service method as a plain Go function
// and never crosses a gRPC connection. A grpc.UnaryInterceptor therefore never
// runs for port 8080. The interceptor covers 9090, this covers 8080, and both
// share one *ratelimit.Service so the budget is the same on either port.
//
// Install it inside the CORS handler. A 429 written outside CORS has no
// Access-Control-Allow-Origin header, and the browser reports an opaque CORS
// failure instead of the rate limit the user actually hit.
func RateLimitMiddleware(limiter *ratelimit.Service, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		if skipRateLimit(req) {
			handler.ServeHTTP(res, req)
			return
		}

		// Always the TCP peer, never X-Forwarded-For. With no proxy in front
		// of this service that header is entirely attacker-controlled, and a
		// client that varied it could mint a fresh budget on every request.
		decision := limiter.Allow(req.RemoteAddr)
		setRateLimitHeaders(res.Header(), decision)

		if decision.Allowed {
			handler.ServeHTTP(res, req)
			return
		}

		writeRateLimited(res, req, decision)
	})
}

// skipRateLimit exempts traffic that is not an API call.
//
// Every /v1 endpoint is treated identically, with no per-endpoint or
// per-method allowances. The two exclusions here are not endpoints: CORS
// preflights are browser bookkeeping, and the Swagger UI pulls dozens of
// static assets on a single page load, which would burn a whole budget just
// to read the docs.
func skipRateLimit(req *http.Request) bool {
	if req.Method == http.MethodOptions {
		return true
	}
	return strings.HasPrefix(req.URL.Path, "/swagger/")
}

func setRateLimitHeaders(header http.Header, decision ratelimit.Decision) {
	// A decision with no limit never consulted a budget: the limiter is off,
	// or the client is exempt or unlisted. Advertising a limit would be a lie.
	if decision.Limit == 0 {
		return
	}

	header.Set(headerRateLimitLimit, strconv.Itoa(decision.Limit))
	header.Set(headerRateLimitRemaining, strconv.Itoa(decision.Remaining))

	if !decision.Allowed {
		header.Set(headerRetryAfter, strconv.Itoa(retryAfterSeconds(decision.RetryAfter)))
		header.Set(headerRateLimitReset, strconv.FormatInt(decision.ResetAt.Unix(), 10))
	}
}

func writeRateLimited(res http.ResponseWriter, req *http.Request, decision ratelimit.Decision) {
	statusRateLimited := rateLimitError(decision)

	// Warn, not Error. A rate limiter doing its job is expected traffic, and
	// a steady stream of Error logs trains the team to ignore them.
	log.Warn().
		Str("protocol", "http").
		Str("client_ip", req.RemoteAddr).
		Str("method", req.Method).
		Str("path", req.RequestURI).
		Str("rule", decision.Rule).
		Int("limit", decision.Limit).
		Dur("retry_after", decision.RetryAfter).
		Msg("rate limit exceeded")

	body, err := rateLimitMarshaler.Marshal(statusRateLimited.Proto())
	if err != nil {
		http.Error(res, statusRateLimited.Message(), http.StatusTooManyRequests)
		return
	}

	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(http.StatusTooManyRequests)
	res.Write(body)
}
