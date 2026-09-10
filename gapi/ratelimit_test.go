package gapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/rs/cors"
	"github.com/stretchr/testify/require"
	mockdb "github.com/techschool/simplebank/db/mock"
	db "github.com/techschool/simplebank/db/sqlc"
	"github.com/techschool/simplebank/pb"
	"github.com/techschool/simplebank/ratelimit"
	"github.com/techschool/simplebank/util"
	mockwk "github.com/techschool/simplebank/worker/mock"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// testClock is a deterministic ratelimit.Clock, so these tests never sleep.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testLimiter builds a limiter with one default budget shared by every IP.
func testLimiter(t *testing.T, requests int, per time.Duration, burst int, clock ratelimit.Clock) *ratelimit.Service {
	t.Helper()

	config := &ratelimit.Config{
		Enabled: true,
		Default: ratelimit.Rule{Requests: requests, Per: per, Burst: burst},
	}
	require.NoError(t, config.Validate())

	return ratelimit.NewService(config, ratelimit.WithClock(clock))
}

// newTestGateway builds the same HTTP chain main.go builds, backed by mocks so
// no database or Redis is needed. Anything that passes the limiter reaches the
// real RPC handlers.
//
// The handler is returned rather than served over TCP so that each request can
// declare its own RemoteAddr. That is what makes per-IP behavior testable
// without needing real network interfaces.
func newTestGateway(t *testing.T, limiter *ratelimit.Service) http.Handler {
	t.Helper()

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	store := mockdb.NewMockStore(ctrl)
	store.EXPECT().GetUser(gomock.Any(), gomock.Any()).AnyTimes().Return(db.User{}, db.ErrRecordNotFound)
	store.EXPECT().CreateUserTx(gomock.Any(), gomock.Any()).AnyTimes().Return(db.CreateUserTxResult{}, db.ErrRecordNotFound)
	store.EXPECT().VerifyEmailTx(gomock.Any(), gomock.Any()).AnyTimes().Return(db.VerifyEmailTxResult{}, db.ErrRecordNotFound)

	server, err := NewServer(util.Config{
		TokenSymmetricKey:   util.RandomString(32),
		AccessTokenDuration: time.Minute,
	}, store, mockwk.NewMockTaskDistributor(ctrl))
	require.NoError(t, err)

	jsonOption := runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
		MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true},
		UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
	})

	grpcMux := runtime.NewServeMux(jsonOption)
	require.NoError(t, pb.RegisterSimpleBankHandlerServer(context.Background(), grpcMux, server))

	mux := http.NewServeMux()
	mux.Handle("/", grpcMux)
	// Stands in for the statik file server, which needs no real files here.
	mux.Handle("/swagger/", http.HandlerFunc(func(res http.ResponseWriter, _ *http.Request) {
		res.WriteHeader(http.StatusOK)
	}))

	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000"},
		AllowedMethods:   []string{http.MethodHead, http.MethodOptions, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		AllowCredentials: true,
	})

	// Same order as main.go: cors -> ratelimit -> logger -> mux.
	return c.Handler(RateLimitMiddleware(limiter, HttpLogger(mux)))
}

// apiCall describes one request to a real endpoint from the README table.
type apiCall struct {
	name   string
	method string
	path   string
	body   string
}

var apiCalls = []apiCall{
	{"create_user", http.MethodPost, "/v1/create_user", `{"username":"ratelimit","password":"secret123","full_name":"Rate Limit","email":"rate@limit.test"}`},
	{"login_user", http.MethodPost, "/v1/login_user", `{"username":"ratelimit","password":"secret123"}`},
	{"update_user", http.MethodPatch, "/v1/update_user", `{"username":"ratelimit","full_name":"Rate Limit"}`},
	{"verify_email", http.MethodGet, "/v1/verify_email?email_id=1&secret_code=abc", ""},
}

func (c apiCall) request(remoteAddr string) *http.Request {
	var body *strings.Reader
	if c.body == "" {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(c.body)
	}

	req := httptest.NewRequest(c.method, c.path, body)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	return req
}

// send issues one request and returns the recorded response.
func send(handler http.Handler, call apiCall, remoteAddr string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, call.request(remoteAddr))
	return rec
}

func loginCall() apiCall { return apiCalls[1] }

// ---------------------------------------------------------------------------
// HTTP gateway
// ---------------------------------------------------------------------------

func TestHTTPRateLimitReturns429(t *testing.T) {
	t.Parallel()

	const burst = 5
	handler := newTestGateway(t, testLimiter(t, 10, time.Second, burst, newTestClock()))

	for i := 1; i <= burst; i++ {
		rec := send(handler, loginCall(), "203.0.113.10:40000")
		require.NotEqual(t, http.StatusTooManyRequests, rec.Code, "request %d is inside the budget", i)
		// The mock store has no users, so a request that got through reaches
		// the real handler and comes back 404. That proves it was not
		// silently swallowed by the middleware.
		require.Equal(t, http.StatusNotFound, rec.Code)
	}

	rec := send(handler, loginCall(), "203.0.113.10:40000")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestHTTP429CarriesRetryAfterAndLimitHeaders(t *testing.T) {
	t.Parallel()

	handler := newTestGateway(t, testLimiter(t, 1, time.Second, 1, newTestClock()))

	require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.11:40000").Code)

	rec := send(handler, loginCall(), "203.0.113.11:40000")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, retryAfter, 1, "Retry-After must never be 0, that invites an immediate retry storm")

	require.Equal(t, "1", rec.Header().Get("X-RateLimit-Limit"))
	require.Equal(t, "0", rec.Header().Get("X-RateLimit-Remaining"))
	require.NotEmpty(t, rec.Header().Get("X-RateLimit-Reset"))
}

func TestHTTP429BodyMatchesGatewayErrorShape(t *testing.T) {
	t.Parallel()

	handler := newTestGateway(t, testLimiter(t, 1, time.Second, 1, newTestClock()))
	send(handler, loginCall(), "203.0.113.12:40000")

	rec := send(handler, loginCall(), "203.0.113.12:40000")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Type       string `json:"@type"`
			RetryDelay string `json:"retry_delay"`
		} `json:"details"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Equal(t, int(codes.ResourceExhausted), body.Code)
	require.Contains(t, body.Message, "rate limit exceeded")
	require.Contains(t, body.Message, "retry after", "the message must tell the caller when they can retry")
	require.Len(t, body.Details, 1)
	require.Contains(t, body.Details[0].Type, "google.rpc.RetryInfo")
	require.NotEmpty(t, body.Details[0].RetryDelay)
}

func TestHTTP429PreservesCORSHeaders(t *testing.T) {
	t.Parallel()

	handler := newTestGateway(t, testLimiter(t, 1, time.Second, 1, newTestClock()))

	exhaust := loginCall().request("203.0.113.13:40000")
	exhaust.Header.Set("Origin", "http://localhost:3000")
	handler.ServeHTTP(httptest.NewRecorder(), exhaust)

	req := loginCall().request("203.0.113.13:40000")
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	// Without this the browser reports an opaque CORS failure and the user
	// never learns they were rate limited.
	require.Equal(t, "http://localhost:3000", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestHTTPRecoversAfterRetryAfterElapses(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	handler := newTestGateway(t, testLimiter(t, 10, time.Second, 2, clock))

	require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.14:40000").Code)
	require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.14:40000").Code)

	rec := send(handler, loginCall(), "203.0.113.14:40000")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	require.NoError(t, err)

	// Honour exactly what we told the client to wait.
	clock.Advance(time.Duration(retryAfter) * time.Second)
	require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.14:40000").Code)
}

func TestHTTPForwardedForHeaderCannotBypassLimit(t *testing.T) {
	t.Parallel()

	handler := newTestGateway(t, testLimiter(t, 2, time.Second, 2, newTestClock()))

	// There is no proxy in front of this service, so X-Forwarded-For is
	// entirely attacker controlled. If the limiter read it, rotating the
	// header would mint a fresh budget on every request.
	limited := 0
	for i := 0; i < 10; i++ {
		req := loginCall().request("203.0.113.15:40000")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited++
		}
	}

	require.Equal(t, 8, limited, "only the first 2 requests should have passed")
}

func TestHTTPDistinctIPsHaveDistinctBudgets(t *testing.T) {
	t.Parallel()

	handler := newTestGateway(t, testLimiter(t, 1, time.Second, 1, newTestClock()))

	require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.16:40000").Code)
	require.Equal(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.16:50000").Code,
		"a new source port is the same client")
	require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.17:40000").Code,
		"a different IP has its own budget")
}

func TestHTTPPreflightAndSwaggerDoNotConsumeBudget(t *testing.T) {
	t.Parallel()

	handler := newTestGateway(t, testLimiter(t, 1, time.Second, 1, newTestClock()))

	// Neither of these is an API call. Swagger alone pulls dozens of static
	// assets on one page load, which would burn a whole budget to read docs.
	for i := 0; i < 20; i++ {
		preflight := httptest.NewRequest(http.MethodOptions, "/v1/login_user", nil)
		preflight.RemoteAddr = "203.0.113.18:40000"
		preflight.Header.Set("Origin", "http://localhost:3000")
		preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, preflight)
		require.NotEqual(t, http.StatusTooManyRequests, rec.Code)

		swagger := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
		swagger.RemoteAddr = "203.0.113.18:40000"
		swaggerRec := httptest.NewRecorder()
		handler.ServeHTTP(swaggerRec, swagger)
		require.Equal(t, http.StatusOK, swaggerRec.Code)
	}

	// The one real API call still has its full budget.
	require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.18:40000").Code)
	require.Equal(t, http.StatusTooManyRequests, send(handler, loginCall(), "203.0.113.18:40000").Code)
}

func TestHTTPDisabledLimiterPassesEverythingThrough(t *testing.T) {
	t.Parallel()

	limiter := ratelimit.NewService(&ratelimit.Config{Enabled: false})
	handler := newTestGateway(t, limiter)

	for i := 0; i < 50; i++ {
		rec := send(handler, loginCall(), "203.0.113.19:40000")
		require.NotEqual(t, http.StatusTooManyRequests, rec.Code)
		require.Empty(t, rec.Header().Get("X-RateLimit-Limit"), "a disabled limiter must not advertise a limit")
	}
}

func TestHTTPAllowedResponsesCarryRemainingHeader(t *testing.T) {
	t.Parallel()

	handler := newTestGateway(t, testLimiter(t, 10, time.Second, 3, newTestClock()))

	for i, wantRemaining := range []string{"2", "1", "0"} {
		rec := send(handler, loginCall(), "203.0.113.20:40000")
		require.NotEqual(t, http.StatusTooManyRequests, rec.Code)
		require.Equal(t, "3", rec.Header().Get("X-RateLimit-Limit"))
		require.Equal(t, wantRemaining, rec.Header().Get("X-RateLimit-Remaining"), "after request %d", i+1)
	}
}

// ---------------------------------------------------------------------------
// The budget is global: one per IP, shared by every API
// ---------------------------------------------------------------------------

func TestLimitIsGlobalAcrossEveryEndpoint(t *testing.T) {
	t.Parallel()

	const addr = "203.0.113.30:40000"
	handler := newTestGateway(t, testLimiter(t, 3, time.Second, 3, newTestClock()))

	// Spend the entire budget on one endpoint.
	for i := 0; i < 3; i++ {
		require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), addr).Code)
	}

	// Every other endpoint is now out of budget too. If any endpoint kept its
	// own bucket, one of these would pass.
	for _, call := range apiCalls {
		t.Run(call.name, func(t *testing.T) {
			require.Equal(t, http.StatusTooManyRequests, send(handler, call, addr).Code)
		})
	}
}

func TestEveryEndpointSpendsTheSameBudget(t *testing.T) {
	t.Parallel()

	const addr = "203.0.113.31:40000"
	// Exactly as many requests as there are endpoints, so one call to each
	// consumes everything and nothing is left over.
	handler := newTestGateway(t, testLimiter(t, len(apiCalls), time.Second, len(apiCalls), newTestClock()))

	for _, call := range apiCalls {
		require.NotEqual(t, http.StatusTooManyRequests, send(handler, call, addr).Code, "%s should pass", call.name)
	}

	for _, call := range apiCalls {
		require.Equal(t, http.StatusTooManyRequests, send(handler, call, addr).Code, "%s should now be limited", call.name)
	}
}

func TestLoginGetsNoSpecialTreatment(t *testing.T) {
	t.Parallel()

	// Each endpoint, in isolation, must exhaust an identical budget after the
	// same number of requests. A future per-endpoint carve-out fails here.
	const budget = 3

	for _, call := range apiCalls {
		t.Run(call.name, func(t *testing.T) {
			t.Parallel()

			handler := newTestGateway(t, testLimiter(t, budget, time.Second, budget, newTestClock()))
			addr := "203.0.113.32:40000"

			for i := 1; i <= budget; i++ {
				require.NotEqual(t, http.StatusTooManyRequests, send(handler, call, addr).Code,
					"%s request %d should be inside the budget", call.name, i)
			}
			require.Equal(t, http.StatusTooManyRequests, send(handler, call, addr).Code,
				"%s should be limited after exactly %d requests, same as every other endpoint", call.name, budget)
		})
	}
}

// ---------------------------------------------------------------------------
// gRPC
// ---------------------------------------------------------------------------

func grpcContext(addr string) context.Context {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		host, portText = addr, "0"
	}
	port, _ := strconv.Atoi(portText)

	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP(host), Port: port},
	})
}

func okHandler(_ context.Context, _ interface{}) (interface{}, error) { return "ok", nil }

var loginInfo = &grpc.UnaryServerInfo{FullMethod: "/pb.SimpleBank/LoginUser"}

func TestGRPCInterceptorReturnsResourceExhausted(t *testing.T) {
	t.Parallel()

	interceptor := RateLimitInterceptor(testLimiter(t, 2, time.Second, 2, newTestClock()))
	ctx := grpcContext("203.0.113.40:40000")

	for i := 0; i < 2; i++ {
		_, err := interceptor(ctx, nil, loginInfo, okHandler)
		require.NoError(t, err)
	}

	_, err := interceptor(ctx, nil, loginInfo, okHandler)
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "retry after")
}

func TestGRPCErrorCarriesRetryInfo(t *testing.T) {
	t.Parallel()

	interceptor := RateLimitInterceptor(testLimiter(t, 1, time.Second, 1, newTestClock()))
	ctx := grpcContext("203.0.113.41:40000")

	_, err := interceptor(ctx, nil, loginInfo, okHandler)
	require.NoError(t, err)

	_, err = interceptor(ctx, nil, loginInfo, okHandler)
	require.Error(t, err)

	details := status.Convert(err).Details()
	require.Len(t, details, 1)

	retryInfo, ok := details[0].(*errdetails.RetryInfo)
	require.True(t, ok, "clients need a machine readable retry delay, not just prose")
	require.Greater(t, retryInfo.GetRetryDelay().AsDuration(), time.Duration(0))
}

func TestGRPCHandlerNotCalledWhenLimited(t *testing.T) {
	t.Parallel()

	interceptor := RateLimitInterceptor(testLimiter(t, 1, time.Second, 1, newTestClock()))
	ctx := grpcContext("203.0.113.42:40000")

	calls := 0
	counting := func(_ context.Context, _ interface{}) (interface{}, error) {
		calls++
		return "ok", nil
	}

	for i := 0; i < 5; i++ {
		_, _ = interceptor(ctx, nil, loginInfo, counting)
	}

	// The point of rate limiting is to shed load before doing the work.
	require.Equal(t, 1, calls)
}

func TestGRPCMissingPeerFailsOpen(t *testing.T) {
	t.Parallel()

	interceptor := RateLimitInterceptor(testLimiter(t, 1, time.Second, 1, newTestClock()))

	for i := 0; i < 5; i++ {
		_, err := interceptor(context.Background(), nil, loginInfo, okHandler)
		require.NoError(t, err, "a call with no identifiable peer must not be dropped")
	}
}

func TestGatewayMapsResourceExhaustedTo429(t *testing.T) {
	t.Parallel()

	// Pins library behavior. If a grpc-gateway upgrade ever remapped this
	// code, throttled gRPC errors surfacing over HTTP would quietly stop
	// being 429 and the frontend would mishandle them.
	require.Equal(t, http.StatusTooManyRequests, runtime.HTTPStatusFromCode(codes.ResourceExhausted))
}

// ---------------------------------------------------------------------------
// One service, both transports
// ---------------------------------------------------------------------------

func TestBudgetIsSharedAcrossHTTPAndGRPC(t *testing.T) {
	t.Parallel()

	const addr = "203.0.113.50:40000"

	// One limiter handed to both adapters, exactly as main.go does it.
	limiter := testLimiter(t, 2, time.Second, 2, newTestClock())
	handler := newTestGateway(t, limiter)
	interceptor := RateLimitInterceptor(limiter)

	// Spend the whole budget over HTTP.
	for i := 0; i < 2; i++ {
		require.NotEqual(t, http.StatusTooManyRequests, send(handler, loginCall(), addr).Code)
	}

	// gRPC from the same IP finds nothing left. If each server built its own
	// limiter, a client could double its allowance by switching protocol.
	_, err := interceptor(grpcContext(addr), nil, loginInfo, okHandler)
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestBudgetIsSharedFromGRPCToHTTP(t *testing.T) {
	t.Parallel()

	const addr = "203.0.113.51:40000"

	limiter := testLimiter(t, 2, time.Second, 2, newTestClock())
	handler := newTestGateway(t, limiter)
	interceptor := RateLimitInterceptor(limiter)

	for i := 0; i < 2; i++ {
		_, err := interceptor(grpcContext(addr), nil, loginInfo, okHandler)
		require.NoError(t, err)
	}

	require.Equal(t, http.StatusTooManyRequests, send(handler, loginCall(), addr).Code)
}

func TestConcurrentHTTPRequestsNeverExceedBudget(t *testing.T) {
	t.Parallel()

	const burst = 10
	const addr = "203.0.113.52:40000"

	// A frozen clock means no refill mid-test, so exactly burst requests may
	// get through no matter how they interleave. Run under -race.
	handler := newTestGateway(t, testLimiter(t, 5, time.Second, burst, newTestClock()))

	var mu sync.Mutex
	passed := 0

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if send(handler, loginCall(), addr).Code != http.StatusTooManyRequests {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.Equal(t, burst, passed)
}
