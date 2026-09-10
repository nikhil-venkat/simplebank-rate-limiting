# Spec: IP-Based Rate Limiting

Status: Implemented. See section 7 for the files as built.
Scope: Application service layer only. No load balancer, reverse proxy, or API gateway is assumed to sit in front of the service.

---

## 1. Problem and goals

Simple Bank exposes four public endpoints over HTTP (8080) and the same four methods over gRPC (9090). Two of them, `create_user` and `login_user`, are unauthenticated and write to the database. Today an attacker can call them as fast as the network allows. There is credential-stuffing exposure on `login_user`, account-spam exposure on `create_user`, and a general resource-exhaustion path into Postgres and the Redis task queue.

Goals:

1. Cap request rate per client IP, enforced inside the Go service.
2. Return HTTP `429` with a message that tells the caller when to retry.
3. Apply globally across every API, not per endpoint.
4. Drive the IP list and limits from a config file, editable without a rebuild.
5. Be testable locally and in CI.

Non-goals for this change: per-user or per-token limits, distributed limits across multiple replicas (covered as a future option in section 4.3), WAF-style blocking, and permanent IP bans.

---

## 2. The critical constraint: there are two enforcement paths, not one

This is the single most important finding, and it drives the whole design.

The HTTP server and the gRPC server look like one pipeline but are not. In [`main.go:192`](simplebank/main.go#L192) the gateway is registered with `pb.RegisterSimpleBankHandlerServer`, which is the **in-process** variant. Looking at the generated code in [`pb/service_simple_bank.pb.gw.go:190`](simplebank/pb/service_simple_bank.pb.gw.go#L190), each HTTP route calls `local_request_SimpleBank_CreateUser_0(...)`, which invokes the server method **directly as a Go function call**.

It never crosses a gRPC connection. That means:

> **A `grpc.UnaryInterceptor` does not run for any HTTP request on port 8080.**

The existing `gapi.GrpcLogger` has this exact blind spot already: it only logs port 9090 traffic, which is why a separate `gapi.HttpLogger` exists in [`gapi/logger.go:60`](simplebank/gapi/logger.go#L60).

Actual request paths today:

```
HTTP  :8080  →  cors.Handler  →  HttpLogger  →  grpcMux  →  local_request_*  →  server.CreateUser
                                                            (no interceptor)

gRPC  :9090  →  grpcServer  →  GrpcLogger interceptor  →  server.CreateUser
```

So rate limiting has to be installed in both chains, or in a place both chains share.

### 2.1 Where to enforce: three options

| | Where | Runs before JSON parse / DB | Can set `Retry-After` header | Files touched | Verdict |
|---|---|---|---|---|---|
| **A** | HTTP middleware + gRPC interceptor, both calling one shared limiter core | Yes | Yes, natively | 2 thin adapters | **Recommended** |
| **B** | Inside each RPC handler, or a decorator wrapping `Server` | No. Runs after gateway routing and protobuf unmarshal | Only via `grpc.SetHeader` plus a `WithOutgoingHeaderMatcher` on the mux | 4 handlers or 1 wrapper | Not recommended |
| **C** | A `net.Listener` wrapper on both listeners | Yes, earliest possible | No, too low level to write an HTTP body | 1 file | Rejected. Cannot return a 429 body, only drop connections |

**Recommendation: Option A.** The two adapters are roughly 25 lines each and contain zero limiting logic. All decision-making lives in one `ratelimit` package, so the two surfaces cannot drift. Option A also rejects abusive traffic before protobuf unmarshalling and before any database or Redis work, which is the point of rate limiting.

Option B is tempting because it is one code path, but it does the expensive work first and makes the `Retry-After` header awkward on the HTTP side. It is worth documenting as the fallback if the team wants a single insertion point.

---

## 3. Client IP resolution

There is a real correctness and security issue here.

The existing [`gapi/metadata.go:33`](simplebank/gapi/metadata.go#L33) reads `ClientIP` from the `x-forwarded-for` metadata key. On the HTTP path, grpc-gateway's `AnnotateIncomingContext` populates that key from the socket's `RemoteAddr`, but it **appends to any `X-Forwarded-For` header the caller sent**. A caller who sends `X-Forwarded-For: 1.2.3.4` produces `x-forwarded-for: 1.2.3.4, <real-ip>`, and the current code takes element `[0]`, which is the attacker-controlled value.

That is fine for logging. It is **not** safe for enforcement. Any attacker could rotate a header value and bypass the limiter entirely.

Since the spec states there is no load balancer in front of the service, the trust rule is simple:

| Path | Source of truth | Notes |
|---|---|---|
| HTTP `:8080` | `http.Request.RemoteAddr`, host portion only | The real TCP peer. Ignore `X-Forwarded-For` entirely. |
| gRPC `:9090` | `peer.FromContext(ctx).Addr`, host portion only | The real TCP peer. |

Strip the port before using the address as a bucket key, otherwise every new connection from the same host gets a fresh bucket and the limiter does nothing. Normalize IPv6 (including IPv4-mapped forms like `::ffff:127.0.0.1`) so `127.0.0.1` and its v6 mapping land in the same bucket.

Add a config flag `RATE_LIMIT_TRUST_FORWARDED_HEADER` defaulting to `false`, so the behavior can be flipped later if a proxy is introduced, but with the unsafe option explicitly opt-in.

---

## 4. Rate limiting methodologies

### 4.1 Token bucket

Each IP gets a bucket holding up to `burst` tokens, refilled at `rate` tokens per second. A request takes one token, or is rejected if the bucket is empty.

`golang.org/x/time/rate` implements exactly this, is already in [`go.sum`](simplebank/go.sum#L1291) at `v0.2.0` with a valid `h1:` hash (pulled in indirectly today), and is goroutine-safe. Adopting it requires **no new module download**, only promoting it to a direct dependency in `go.mod`.

- Pros: smooth limiting, no boundary spike, explicit burst allowance for legitimate bursty clients, and `Reserve().Delay()` gives an exact retry time for the `Retry-After` header for free.
- Cons: two values to tune instead of one. Slightly harder to explain to non-engineers.
- Memory: one small struct per tracked IP.

### 4.2 Time interval based

**Fixed window.** Count requests per IP within a wall-clock window (for example 60 requests per minute). Reset the counter when the window rolls over.

- Pros: trivially simple, obvious semantics, exact reset time for `Retry-After`.
- Cons: the boundary problem. A client can send 60 requests at `12:00:59` and 60 more at `12:01:00`, so 120 requests land in one second while technically staying inside the limit. That is a 2x burst leak.

**Sliding window counter.** Weight the previous window's count by how far into the current window you are. Removes most of the boundary spike, still O(1) memory per IP.

- Pros: fixed-window simplicity, most of the token-bucket smoothness.
- Cons: approximate, and the `Retry-After` value is an estimate.

**Sliding window log.** Store a timestamp per request and count entries newer than `now - window`. Exact, but memory grows with the limit and it is the easiest of these to turn into its own memory-exhaustion vector.

### 4.3 Comparison

| Algorithm | Burst handling | Memory / IP | Boundary spike | `Retry-After` accuracy | Complexity |
|---|---|---|---|---|---|
| Token bucket | Explicit and tunable | O(1), ~40 bytes | None | Exact | Low, library provided |
| Fixed window | Uncontrolled within window | O(1), ~16 bytes | Up to 2x limit | Exact | Lowest |
| Sliding window counter | Smoothed | O(1), ~32 bytes | Negligible | Approximate | Medium |
| Sliding window log | Exact | O(limit) | None | Exact | Medium, memory risk |

**Recommendation: token bucket as the default, fixed window as a selectable alternative.** Build both behind one `Limiter` interface, selected by an `algorithm` key in the config file. Token bucket is the better limiter and costs nothing extra in dependencies. Fixed window is worth shipping alongside it because it is the easiest to reason about during an incident, and having two implementations behind one interface proves the abstraction is real rather than theoretical.

### 4.4 Where the counters live

For a single instance, an in-process `map[string]*bucket` guarded by a mutex is correct and fast. Note the consequence honestly: with N replicas behind round-robin, the effective limit becomes N times the configured limit, and counters reset on deploy.

Redis is already a dependency (`go-redis/v8` plus `asynq`, running as a compose service), so a Redis-backed store is a natural follow-up. Keep the `Limiter` interface storage-agnostic from day one so that swap is additive. It should not be in this change: it adds a hard runtime dependency on Redis for every request and a failure-mode decision (fail open or fail closed) that deserves its own discussion.

### 4.5 Memory safety

An unbounded `map[ip]bucket` is itself a memory-exhaustion vector, since an attacker with a large source range creates one entry per IP. Required mitigations:

- A background sweeper (every 1 minute) that deletes buckets which are full and idle, meaning the client has been silent long enough to have fully refilled.
- A hard cap `max_tracked_ips` (default 100,000). On overflow, evict least-recently-used and emit a warning log.

---

## 5. Configuration file

New file at the repo root: `ratelimit.yaml`. Separate from [`app.env`](simplebank/app.env) because it holds structured, nested, frequently-edited data that a flat `.env` file cannot express. Viper is already a dependency and reads YAML natively.

```yaml
# ratelimit.yaml
enabled: true

# "token_bucket" or "fixed_window"
algorithm: token_bucket

# all_clients: default rule applies to every IP, `rules` are per-IP overrides
# listed_only: ONLY IPs matching `rules` are limited, everyone else passes
enforcement: all_clients

# Applied to any IP not matched by an entry in `rules`.
# Both algorithms read the same three fields, so switching algorithms never
# means rewriting the rule list. token_bucket refills at requests/per and
# holds at most `burst`; fixed_window allows `requests` per `per` and ignores
# `burst`. An omitted burst defaults to `requests`.
default:
  requests: 10
  per: 1s
  burst: 20

# Never limited. Evaluated before `rules`. Health checks, internal probes.
exempt:
  - 10.0.0.0/8

# Per-IP and per-CIDR overrides.
rules:
  - ip: 203.0.113.42          # single abusive client, throttled hard
    requests: 1
    per: 1s
    burst: 1
  - ip: 198.51.100.0/24       # noisy partner range
    requests: 5
    per: 1s
    burst: 10
  - ip: 192.0.2.7             # trusted integration partner
    requests: 100
    per: 1s
    burst: 200

# Safety valve for the in-process bucket map.
max_tracked_ips: 100000
```

Rules:

- Every `ip` accepts a bare address or CIDR. A bare address is treated as a `/32` or `/128`.
- Matching order: `exempt`, then `rules` most-specific-prefix-first, then `default`. Rules are sorted at load time, so matching does not depend on the order an operator happened to write them in.
- The file is validated at startup. Malformed CIDR, negative rates, or `burst` less than 1 is a fatal error at boot, not a silent skip. Failing to start beats silently running with no protection.
- Optional follow-up: watch the file with `fsnotify` (already an indirect dependency via viper) and hot-reload on change, so an on-call engineer can throttle an attacker without a redeploy. Reload swaps the ruleset atomically and preserves existing buckets.

New entries in [`app.env`](simplebank/app.env):

```
RATE_LIMIT_ENABLED=true
RATE_LIMIT_CONFIG_PATH=ratelimit.yaml
```

`RATE_LIMIT_TRUST_FORWARDED_HEADER` from section 3 was **not** built. With no
proxy in the deployment there is no case where trusting the header is correct,
and shipping an unsafe switch nobody needs invites someone to flip it. Add it
alongside the proxy, if a proxy ever appears.

Keeping the master on/off switch in `app.env` means it is overridable by a plain environment variable in `docker-compose.yaml`, which matters for the local testing flow in section 8.

---

## 6. Response contract

### 6.1 HTTP

Status: `429 Too Many Requests`.

Headers:

| Header | Example | Notes |
|---|---|---|
| `Retry-After` | `30` | Seconds. RFC 9110 compliant. This is the field that answers "when can I retry". |
| `X-RateLimit-Limit` | `20` | How many requests may be made back to back: the bucket capacity for token_bucket, the per-window allowance for fixed_window. Remaining counts down from it, so the two headers are on one scale |
| `X-RateLimit-Remaining` | `0` | Always 0 on a 429 |
| `X-RateLimit-Reset` | `1757522400` | Unix seconds when capacity returns |

Body, deliberately matching the shape grpc-gateway already produces for gRPC status errors, so clients need only one error parser:

```json
{
  "code": 8,
  "message": "rate limit exceeded: too many requests from your IP address, please retry after 30 seconds",
  "details": [
    {
      "@type": "type.googleapis.com/google.rpc.RetryInfo",
      "retry_delay": "30s"
    }
  ]
}
```

`code: 8` is `codes.ResourceExhausted`.

Two CORS notes, both easy to miss:

1. The 429 must be written **inside** the CORS handler, not before it. If the middleware sits outside `c.Handler(...)` in [`main.go:225`](simplebank/main.go#L225), the response has no `Access-Control-Allow-Origin` header and the browser reports an opaque CORS failure instead of the actual 429. Order must be `cors → ratelimit → HttpLogger → mux`.
2. Preflight `OPTIONS` requests should not consume tokens. Skip them, and skip `/swagger/` static assets, otherwise loading the Swagger UI page burns a client's entire budget on CSS and JS files.

### 6.2 gRPC

Return `status.Error(codes.ResourceExhausted, ...)` with an attached `errdetails.RetryInfo`. This mirrors the existing `invalidArgumentError` pattern in [`gapi/error.go:16`](simplebank/gapi/error.go#L16).

grpc-gateway v2 maps `codes.ResourceExhausted` to HTTP 429 in `HTTPStatusFromCode`. That mapping matters for anything that slips past the HTTP middleware, and it is a library behavior rather than ours, so section 8 includes an explicit test that pins it. If it ever returns 403 instead, that test fails loudly rather than the frontend silently mishandling the response.

### 6.3 Global across all APIs

One bucket per IP, shared by all four endpoints and both protocols. Budget spent on `POST /v1/login_user` is unavailable to `POST /v1/create_user`. This is stated explicitly because it is a testable requirement, and section 8.4 tests it directly.

---

## 7. Files as built

### 7.1 New files

| File | Purpose |
|---|---|
| `simplebank/ratelimit/config.go` | Load and validate the rules file. CIDR parsing, specificity sort, match order |
| `simplebank/ratelimit/limiter.go` | The `Decision` result and the internal `bucket` interface both algorithms satisfy |
| `simplebank/ratelimit/token_bucket.go` | Token bucket over `golang.org/x/time/rate` |
| `simplebank/ratelimit/fixed_window.go` | Fixed window counter |
| `simplebank/ratelimit/service.go` | The shared service: per-IP registry, `Allow`, LRU cap, sweeper |
| `simplebank/ratelimit/ip.go` | Peer address to bucket key: strips ports, folds IPv4-mapped IPv6 |
| `simplebank/ratelimit/clock.go` | Injectable clock so no test sleeps |
| `simplebank/gapi/ratelimit_middleware.go` | HTTP adapter. **This is what protects port 8080** |
| `simplebank/gapi/ratelimit_interceptor.go` | gRPC unary interceptor for port 9090 |
| `simplebank/ratelimit.yaml` | The shipped rules |
| `simplebank/ratelimit.validate.yaml` | Tiny budget used by the validation script |
| `simplebank/docker-compose.validate.yaml` | Overlay that swaps in the validation rules |
| `simplebank/scripts/validate_ratelimit.sh` | End-to-end validation, 18 checks against a live stack |
| `simplebank/ratelimit/service_test.go` | Both algorithms, isolation, eviction, concurrency |
| `simplebank/ratelimit/config_test.go` | Parsing, CIDR matching, validation failures |
| `simplebank/ratelimit/main_test.go` | Fake clock and config helpers |
| `simplebank/ratelimit/testdata/ratelimit.yaml` | Config fixture |
| `simplebank/gapi/ratelimit_test.go` | HTTP and gRPC tests against the real endpoints |

Two files in section 7.1 of the original plan were dropped. `store.go` folded
into `service.go`, since the registry is about thirty lines and splitting it
bought nothing. The separate HTTP and gRPC test files became one
`ratelimit_test.go`, because the cross-protocol tests need both and would have
had to live in one of them anyway.

### 7.2 Modified files

| File | Change |
|---|---|
| [`simplebank/main.go`](simplebank/main.go) | Build one limiter in `main` and pass it to both servers. `runGrpcServer` now chains `RateLimitInterceptor` ahead of `GrpcLogger`; `runGatewayServer` inserts the middleware as `cors -> ratelimit -> logger -> mux`. Adds `newRateLimiter` and `runRateLimitSweeper` |
| [`simplebank/util/config.go`](simplebank/util/config.go) | `RateLimitEnabled` and `RateLimitConfigPath` |
| [`simplebank/app.env`](simplebank/app.env) | The two new variables |
| [`simplebank/gapi/error.go`](simplebank/gapi/error.go) | `rateLimitError` and `retryAfterSeconds`. Both transports build their response from this one status |
| [`simplebank/gapi/metadata.go`](simplebank/gapi/metadata.go) | `peerAddress`, which reads the socket and ignores `x-forwarded-for`. `extractMetadata` is untouched, it is still fine for logging |
| [`simplebank/gapi/main_test.go`](simplebank/gapi/main_test.go) | `TestMain` silencing zerolog. The handlers log every request and the output buried real failures |
| [`simplebank/go.mod`](simplebank/go.mod) | `golang.org/x/time v0.2.0` promoted from indirect to direct. `go.sum` unchanged, the hash was already there |
| [`simplebank/Dockerfile`](simplebank/Dockerfile) | `COPY ratelimit.yaml .` |
| [`simplebank/docker-compose.yaml`](simplebank/docker-compose.yaml) | Mounts `ratelimit.yaml` read-only, exposes `RATE_LIMIT_ENABLED` |
| [`simplebank/Makefile`](simplebank/Makefile) | `test-ratelimit` and `validate-ratelimit` |
| [`simplebank/README.md`](simplebank/README.md) | A "Rate limiting" section, the two config variables, the Docker bridge warning |
| [`simplebank/frontend/src/components/LoginUser.vue`](simplebank/frontend/src/components/LoginUser.vue) | A 429 branch that reads `Retry-After` and says how long to wait. Previously anything but a 404 showed "An error occurred" |

### 7.3 Not changed

- `simplebank/gapi/server.go`. The limiter never touches the `Server` struct.
- `simplebank/proto/*` and `simplebank/pb/*`. Rate limiting is a transport
  concern, not part of the service contract. No regeneration.
- `simplebank/db/*`. Nothing is persisted.

---

## 8. Testing and validation

### 8.1 The validation script

`./scripts/validate_ratelimit.sh` brings up the stack with
`ratelimit.validate.yaml` (5 requests per 10s, so one token returns every 2
seconds, slow enough that a shell loop cannot earn budget mid-run), runs 18
checks, and tears down. `--keep` leaves it running, `--no-up` points at a
server you started yourself, `--url` sets the address.

The checks run as one narrative rather than as independent cases. The budget is
spent once, at the start, entirely on `/v1/login_user`, and every later check
reads the consequences of that single spend. That is what lets the script
*demonstrate* the global budget instead of asserting it.

| Check | What it shows |
|---|---|
| 1 | Exactly `burst` requests pass, and the first rejection is request `burst+1` |
| 2 | The 429 carries `Retry-After` >= 1 plus the three `X-RateLimit-*` headers |
| 3 | The body carries code 8, a message saying when to retry, and a `RetryInfo` detail |
| 4 | Four requests with four different `X-Forwarded-For` values are all still rejected |
| 5 | All four endpoints are out of budget, though only `login_user` spent it |
| 6 | `/swagger/` still serves while the API is throttled |
| 7 | Waiting exactly `Retry-After` works, and spending that one token on `create_user` re-throttles `login_user` |
| 8 | gRPC is limited by the same service, 5 calls served then `ResourceExhausted` |

Check 8 uses `grpcurl` from the host when it is installed, which shares the
HTTP client's budget directly. Otherwise it runs `grpcurl` inside the API
container's network namespace, where it arrives as a different client IP and so
gets its own budget: enough to prove the interceptor enforces, though not that
the budget is shared. Cross-protocol sharing is covered by the Go tests below.

### 8.2 `ratelimit` package

Every test uses a fake clock. Nothing sleeps.

Token bucket: burst then denial, refill over time, `RetryAfter` matching the
real refill time, and that waiting exactly that long actually works. Fixed
window: reset at the boundary, and `TestFixedWindowBoundaryBurstIsExpected`,
which pins the 2x boundary leak as intended behavior so nobody "fixes" it by
accident.

`TestDeniedRequestDoesNotConsumeBudget` hammers an empty bucket 50 times and
checks the client still recovers on schedule. A limiter that charges for
rejections punishes clients for retrying and never lets them back in.

Keys and matching: `TestPortIsStrippedFromBucketKey` (the likeliest
implementation bug, since a fresh source port per request would give every
request its own bucket), `TestIPv6MappedAddressSharesBucket`,
`TestMostSpecificRuleWins`, `TestCIDRRuleMatchesMembers`, `TestExemptMatching`,
`TestListedOnlyEnforcement`.

Safety: `TestMaxTrackedIPsEvictsOldest`, `TestSweeperReclaimsIdleBuckets`,
`TestUnparseableAddressFailsOpen`, `TestConcurrentAccessNeverExceedsBurst`
(50 goroutines, frozen clock, so the allowed total must be exactly the burst).

`TestInvalidConfigFailsFast` covers eleven malformed configs.
`TestShippedConfigIsValid` loads the real `ratelimit.yaml` and asserts the
Docker bridge range is not exempt, which would otherwise make the limiter look
broken during local validation.

### 8.3 `gapi` package

These build the same handler chain `main.go` builds, backed by `mockdb` so no
database is needed, and set `RemoteAddr` per request so per-IP behavior is
testable without real network interfaces.

`TestHTTPRateLimitReturns429` asserts the allowed requests come back 404 from
the real handler, not merely "not 429", which proves the middleware passes
traffic through rather than swallowing it.

Response contract: `TestHTTP429CarriesRetryAfterAndLimitHeaders`,
`TestHTTP429BodyMatchesGatewayErrorShape`,
`TestHTTPAllowedResponsesCarryRemainingHeader`,
`TestHTTP429PreservesCORSHeaders` (catches the middleware-ordering mistake from
section 6.1), `TestHTTPRecoversAfterRetryAfterElapses`.

Security and scope: `TestHTTPForwardedForHeaderCannotBypassLimit`,
`TestHTTPDistinctIPsHaveDistinctBudgets`,
`TestHTTPPreflightAndSwaggerDoNotConsumeBudget`,
`TestHTTPDisabledLimiterPassesEverythingThrough`.

The three that carry the requirement:

- `TestLimitIsGlobalAcrossEveryEndpoint` spends the budget on `login_user`,
  then asserts all four endpoints return 429.
- `TestEveryEndpointSpendsTheSameBudget` gives a budget of exactly four, calls
  each endpoint once, and asserts a second pass over all four is throttled.
- `TestLoginGetsNoSpecialTreatment` runs each endpoint in isolation against an
  identical budget and asserts each is throttled after exactly the same number
  of requests. A future per-endpoint carve-out fails here.

gRPC: `TestGRPCInterceptorReturnsResourceExhausted`,
`TestGRPCErrorCarriesRetryInfo`, `TestGRPCMissingPeerFailsOpen`, and
`TestGRPCHandlerNotCalledWhenLimited`, which counts handler invocations, since
shedding load before doing the work is the whole point.

`TestGatewayMapsResourceExhaustedTo429` pins `runtime.HTTPStatusFromCode`.
That mapping belongs to grpc-gateway, not to us, so an upgrade that changed it
would silently stop throttled gRPC errors from surfacing as 429 over HTTP.

Cross-protocol: `TestBudgetIsSharedAcrossHTTPAndGRPC` and
`TestBudgetIsSharedFromGRPCToHTTP` spend the budget on one transport and assert
the other is throttled, which is what proves `main.go` passes one instance to
both servers rather than constructing two.
`TestConcurrentHTTPRequestsNeverExceedBudget` runs 60 goroutines under `-race`.

### 8.4 Running them

```bash
make test-ratelimit        # go test -race ./ratelimit/... ./gapi/...
make validate-ratelimit    # the end-to-end script
```

`make test` picks the new packages up unchanged. Note it does not pass
`-race`; `test-ratelimit` does, and the concurrency tests are only meaningful
with it.

---

## 9. Observability

Log every rejection through the existing zerolog setup, at `Warn`:

```
client_ip, method, path, protocol, rule_matched, limit, retry_after
```

Note that a 429 already flows through `HttpLogger`, which logs any non-200 at `Error` level with the response body. Rejections should be `Warn` rather than `Error`, since a working rate limiter producing a steady stream of `Error` logs will train the team to ignore them.

Counters worth exporting when metrics land: `ratelimit_allowed_total`, `ratelimit_rejected_total{rule}`, `ratelimit_tracked_ips`. The last one is the early warning for the memory-exhaustion case in section 4.5.

---

## 10. Rollout

1. Ship with `RATE_LIMIT_ENABLED=false`, deploy, confirm no behavior change.
2. Enable with `enforcement: listed_only` and an empty `rules` list. Nothing is limited, but the code path is live and the sweeper is running.
3. Add known abusive IPs to `rules`. Verify rejections in logs.
4. Switch to `enforcement: all_clients` with a generous `default` (say 50 rps, burst 100). Watch the rejection counter for false positives from legitimate clients such as the frontend and Swagger UI.
5. Tighten the default toward the target of 10 rps, burst 20.

Rollback at any step is a single environment variable flip and a restart, or a `ratelimit.yaml` edit if hot reload is implemented.

---

## 11. Open questions

1. ~~Should `login_user` get a tighter limit than the global one?~~ **Decided: no.** Limits are identical across every API, and `TestLoginGetsNoSpecialTreatment` enforces that. The concern still stands on its merits, a global cap generous enough for normal browsing is still generous enough for meaningful credential stuffing, so the mitigation belongs somewhere other than the rate limiter: per-account lockout or a login attempt counter keyed on username rather than IP.
2. Fail open or fail closed if the config file is unreadable at hot-reload time? Recommendation: keep the last known-good ruleset in memory, log an error, and do not fail the request.
3. Is IPv6 `/128` per-address limiting the right granularity? A single client can hold a `/64`. Consider limiting IPv6 by `/64` prefix rather than by full address.
