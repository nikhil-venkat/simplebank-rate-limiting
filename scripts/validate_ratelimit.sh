#!/usr/bin/env bash
#
# Validates IP based rate limiting against a running Simple Bank server.
#
#   ./scripts/validate_ratelimit.sh              # brings the stack up, checks, tears it down
#   ./scripts/validate_ratelimit.sh --keep       # leaves the stack running afterwards
#   ./scripts/validate_ratelimit.sh --no-up      # checks a server you started yourself
#   ./scripts/validate_ratelimit.sh --url http://localhost:8080
#   ./scripts/validate_ratelimit.sh --config ratelimit.yaml
#
# The expected budget is read from the config file, not hardcoded, so editing
# the rules and rerunning still checks the right numbers.
#
# The checks run as one narrative against a single client IP. The budget is
# spent once, at the start, entirely on /v1/login_user. Every later check reads
# the consequences of that one spend, which is what makes "the budget is global"
# something the script can actually demonstrate rather than assert.

set -uo pipefail

URL="http://localhost:8080"
BRING_UP=1
KEEP=0
CONFIG="ratelimit.validate.yaml"
COMPOSE=(docker compose -f docker-compose.yaml -f docker-compose.validate.yaml)

while [ $# -gt 0 ]; do
  case "$1" in
    --url)    URL="$2"; shift 2 ;;
    --config) CONFIG="$2"; shift 2 ;;
    --no-up)  BRING_UP=0; shift ;;
    --keep)   KEEP=1; shift ;;
    -h|--help) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

cd "$(dirname "$0")/.."

# --- read the expected budget out of the config file -----------------------
#
# Deliberately not hardcoded. If these numbers drifted from the rules the
# server is actually running, the script would be checking fiction.

yaml_scalar() { # yaml_scalar <file> <key>   top level only
  sed 's/#.*//' "$1" | awk -F: -v key="$2" \
    '$1 == key { gsub(/^[ \t]+|[ \t]+$/, "", $2); print $2; exit }'
}

yaml_nested() { # yaml_nested <file> <block> <key>   one level of indent
  sed 's/#.*//' "$1" \
    | awk -v b="$2" '$0 ~ "^" b ":" {f=1; next} f && /^[^ \t]/ {exit} f {print}' \
    | awk -F: -v key="$3" '{ gsub(/^[ \t]+/, "", $1) } $1 == key { gsub(/^[ \t]+|[ \t]+$/, "", $2); print $2; exit }'
}

duration_seconds() { # "10s" | "1m" | "500ms" -> whole seconds, rounded up, min 1
  awk -v d="$1" 'BEGIN {
    if (d ~ /ms$/)      { sub(/ms$/, "", d); v = d / 1000 }
    else if (d ~ /h$/)  { sub(/h$/,  "", d); v = d * 3600 }
    else if (d ~ /m$/)  { sub(/m$/,  "", d); v = d * 60 }
    else                { sub(/s$/,  "", d); v = d + 0 }
    v = int(v + 0.999999); print (v < 1 ? 1 : v)
  }'
}

if [ ! -f "$CONFIG" ]; then
  echo "config not found: $CONFIG" >&2
  exit 2
fi

ALGORITHM="$(yaml_scalar "$CONFIG" algorithm)"
[ -n "$ALGORITHM" ] || ALGORITHM="token_bucket"
REQUESTS="$(yaml_nested "$CONFIG" default requests)"
PER="$(yaml_nested "$CONFIG" default per)"
BURST="$(yaml_nested "$CONFIG" default burst)"
[ -n "$BURST" ] || BURST="$REQUESTS"   # an omitted burst defaults to requests

if [ -z "$REQUESTS" ] || [ -z "$PER" ]; then
  echo "could not read default.requests / default.per from $CONFIG" >&2
  exit 2
fi

PER_SECONDS="$(duration_seconds "$PER")"

if [ "$ALGORITHM" = "fixed_window" ]; then
  # The whole window resets at once, so the budget is `requests` and a full
  # reset takes one window.
  BUDGET="$REQUESTS"
  REFILL_SECONDS="$PER_SECONDS"
  FULL_REFILL_SECONDS="$PER_SECONDS"
else
  # Tokens come back one at a time at requests/per.
  BUDGET="$BURST"
  REFILL_SECONDS="$(awk -v p="$PER_SECONDS" -v r="$REQUESTS" 'BEGIN { v = int(p / r + 0.999999); print (v < 1 ? 1 : v) }')"
  FULL_REFILL_SECONDS=$((BUDGET * REFILL_SECONDS))
fi

if [ -t 1 ]; then
  RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; OFF=$'\033[0m'
else
  RED=""; GREEN=""; YELLOW=""; BOLD=""; OFF=""
fi

PASSED=0
FAILED=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

step()  { printf '\n%s==> %s%s\n' "$BOLD" "$1" "$OFF"; }
info()  { printf '    %s\n' "$1"; }
pass()  { PASSED=$((PASSED+1)); printf '    %sPASS%s  %s\n' "$GREEN" "$OFF" "$1"; }
fail()  { FAILED=$((FAILED+1)); printf '    %sFAIL%s  %s\n' "$RED" "$OFF" "$1"; }
skip()  { printf '    %sSKIP%s  %s\n' "$YELLOW" "$OFF" "$1"; }

check() { # check <description> <actual> <expected>
  if [ "$2" = "$3" ]; then pass "$1 (got $2)"; else fail "$1 (want $3, got $2)"; fi
}

# call <METHOD> <PATH> [extra curl args...] -> prints status code, saves
# headers to $TMP/headers and body to $TMP/body
call() {
  local method="$1" path="$2"; shift 2
  local body='{"username":"ratelimit_probe","password":"secret123"}'
  case "$path" in
    /v1/create_user) body='{"username":"ratelimit_probe","password":"secret123","full_name":"Rate Limit","email":"probe@ratelimit.test"}' ;;
    /v1/update_user) body='{"username":"ratelimit_probe","full_name":"Rate Limit"}' ;;
  esac

  local args=(-s -o "$TMP/body" -D "$TMP/headers" -w '%{http_code}' -X "$method" "$URL$path" -H 'Content-Type: application/json')
  [ "$method" != "GET" ] && args+=(-d "$body")
  curl "${args[@]}" "$@"
}

header() { # header <name> -> value, lowercased lookup, CR stripped
  tr -d '\r' < "$TMP/headers" | awk -v want="$(echo "$1" | tr 'A-Z' 'a-z')" \
    'BEGIN{IGNORECASE=1} tolower($1) == want":" {sub(/^[^:]*: */, ""); print; exit}'
}

# ---------------------------------------------------------------------------

if [ "$BRING_UP" = "1" ]; then
  step "Starting the stack with ratelimit.validate.yaml"
  info "rules:     $CONFIG"
  info "algorithm: $ALGORITHM"
  info "budget:    $REQUESTS per $PER, burst $BURST  ->  $BUDGET back to back, one back every ${REFILL_SECONDS}s"
  if ! "${COMPOSE[@]}" up -d --build >"$TMP/up.log" 2>&1; then
    printf '%sdocker compose up failed%s\n' "$RED" "$OFF"; tail -30 "$TMP/up.log"; exit 1
  fi
  [ "$KEEP" = "1" ] || trap '"${COMPOSE[@]}" down >/dev/null 2>&1; rm -rf "$TMP"' EXIT
fi

step "Waiting for $URL"
READY=0
for _ in $(seq 1 90); do
  code="$(curl -s -o /dev/null -w '%{http_code}' -m 2 "$URL/v1/login_user" -X POST \
           -H 'Content-Type: application/json' -d '{"username":"wait","password":"waiting"}' 2>/dev/null)"
  # Anything that is not a connection failure means the HTTP server is up.
  if [ -n "$code" ] && [ "$code" != "000" ]; then READY=1; break; fi
  sleep 1
done
if [ "$READY" != "1" ]; then
  printf '%sserver never became reachable at %s%s\n' "$RED" "$URL" "$OFF"
  [ "$BRING_UP" = "1" ] && "${COMPOSE[@]}" logs --tail 40 api
  exit 1
fi
info "server is up"

# The wait loop above already spent budget. Let it refill so the first check
# starts from a known full bucket.
step "Letting the bucket refill before measuring"
info "waiting ${FULL_REFILL_SECONDS}s for a full bucket"
sleep $FULL_REFILL_SECONDS

# ---------------------------------------------------------------------------
step "Check 1: the budget is enforced, and only after the burst is spent"
info "sending $((BUDGET + 3)) requests to /v1/login_user"

ALLOWED=0
LIMITED=0
FIRST_LIMITED_AT=0
for i in $(seq 1 $((BUDGET + 3))); do
  code="$(call POST /v1/login_user)"
  if [ "$code" = "429" ]; then
    LIMITED=$((LIMITED+1))
    [ "$FIRST_LIMITED_AT" = "0" ] && FIRST_LIMITED_AT="$i"
  else
    ALLOWED=$((ALLOWED+1))
  fi
  printf '      request %-2s -> %s\n' "$i" "$code"
done

check "exactly $BUDGET requests were allowed" "$ALLOWED" "$BUDGET"
check "the rest were rejected" "$LIMITED" "3"
check "the first rejection is request $((BUDGET + 1))" "$FIRST_LIMITED_AT" "$((BUDGET + 1))"

# ---------------------------------------------------------------------------
step "Check 2: the 429 tells the caller when to retry"
cat "$TMP/headers" | tr -d '\r' | grep -iE '^(HTTP/|retry-after|x-ratelimit)' | sed 's/^/      /'

RETRY_AFTER="$(header Retry-After)"
if [ -n "$RETRY_AFTER" ] && [ "$RETRY_AFTER" -ge 1 ] 2>/dev/null; then
  pass "Retry-After is present and at least 1 second (got ${RETRY_AFTER}s)"
else
  fail "Retry-After missing or not a positive integer (got '${RETRY_AFTER}')"
fi
check "X-RateLimit-Limit advertises the budget" "$(header X-RateLimit-Limit)" "$BUDGET"
check "X-RateLimit-Remaining is zero" "$(header X-RateLimit-Remaining)" "0"
[ -n "$(header X-RateLimit-Reset)" ] && pass "X-RateLimit-Reset is present" || fail "X-RateLimit-Reset is missing"

step "Check 3: the 429 body is machine readable"
sed 's/^/      /' "$TMP/body"; echo

grep -q '"code":8' "$TMP/body" && pass "code 8 (RESOURCE_EXHAUSTED)" || fail "expected \"code\":8 in the body"
grep -q 'rate limit exceeded' "$TMP/body" && pass "message explains the rejection" || fail "message does not mention the rate limit"
grep -q 'retry after' "$TMP/body" && pass "message says when to retry" || fail "message does not say when to retry"
grep -q 'RetryInfo' "$TMP/body" && pass "RetryInfo detail attached for programmatic clients" || fail "no RetryInfo detail"

# ---------------------------------------------------------------------------
step "Check 4: X-Forwarded-For cannot buy a fresh budget"
info "the bucket is empty; sending 4 requests, each claiming a different origin IP"

SPOOF_LIMITED=0
for i in 1 2 3 4; do
  code="$(call POST /v1/login_user -H "X-Forwarded-For: 198.51.100.$i")"
  printf '      X-Forwarded-For: 198.51.100.%-3s -> %s\n' "$i" "$code"
  [ "$code" = "429" ] && SPOOF_LIMITED=$((SPOOF_LIMITED+1))
done
check "all 4 spoofed requests still rejected" "$SPOOF_LIMITED" "4"

# ---------------------------------------------------------------------------
step "Check 5: the budget is global across every API"
info "the budget above was spent entirely on /v1/login_user"
info "if any endpoint kept its own bucket, it would answer with something other than 429"

GLOBAL_LIMITED=0
for endpoint in "POST /v1/create_user" "POST /v1/login_user" "PATCH /v1/update_user" "GET /v1/verify_email?email_id=1&secret_code=abc"; do
  set -- $endpoint
  code="$(call "$1" "$2")"
  printf '      %-6s %-46s -> %s\n' "$1" "$2" "$code"
  [ "$code" = "429" ] && GLOBAL_LIMITED=$((GLOBAL_LIMITED+1))
done
check "all 4 endpoints are out of budget" "$GLOBAL_LIMITED" "4"

# ---------------------------------------------------------------------------
step "Check 6: static docs are not charged to the budget"
code="$(curl -s -L -o /dev/null -w '%{http_code}' "$URL/swagger/")"
info "GET /swagger/ -> $code"
[ "$code" != "429" ] && pass "Swagger UI still loads while the API is throttled" || fail "Swagger UI is being rate limited"

# ---------------------------------------------------------------------------
step "Check 7: the caller recovers exactly when told to"
info "waiting the ${RETRY_AFTER}s the server asked for"
sleep "$RETRY_AFTER"

code="$(call POST /v1/create_user)"
info "POST /v1/create_user -> $code"
[ "$code" != "429" ] && pass "the recovered budget was honoured" || fail "still rejected after waiting Retry-After"

# How much comes back differs by algorithm, and getting this wrong would make
# the check below assert the wrong thing. A token bucket hands back exactly one
# token when Retry-After elapses. A fixed window rolls over and hands back the
# entire allowance at once.
if [ "$ALGORITHM" = "fixed_window" ]; then
  RECOVERED="$BUDGET"
  info "a fixed window returns the whole allowance of $BUDGET at once"
else
  RECOVERED=1
  info "a token bucket returns exactly one token"
fi

if [ "$RECOVERED" -gt 1 ]; then
  info "spending the remaining $((RECOVERED - 1)) on create_user"
  for _ in $(seq 2 "$RECOVERED"); do call POST /v1/create_user >/dev/null; done
fi

info "with the recovered budget spent on create_user, login_user should have nothing left"
code="$(call POST /v1/login_user)"
info "POST /v1/login_user  -> $code"
check "login_user is rejected again" "$code" "429"

# ---------------------------------------------------------------------------
step "Check 8: gRPC is limited by the same service"
GRPC_PAYLOAD='{"username":"ratelimit_probe","password":"secret123"}'

if command -v grpcurl >/dev/null 2>&1; then
  # Best case: grpcurl runs on the host, so it reaches the server from the
  # same source IP curl did and is spending the very budget curl exhausted.
  info "calling pb.SimpleBank/LoginUser from this host, which shares the HTTP budget"
  out="$(grpcurl -plaintext -d "$GRPC_PAYLOAD" localhost:9090 pb.SimpleBank/LoginUser 2>&1)"
  printf '%s\n' "$out" | head -3 | sed 's/^/      /'
  printf '%s' "$out" | grep -qi 'ResourceExhausted' \
    && pass "gRPC rejected the call, so both ports share one budget" \
    || fail "gRPC did not report ResourceExhausted"

elif API_CID="$("${COMPOSE[@]}" ps -q api 2>/dev/null)" && [ -n "$API_CID" ]; then
  # No host grpcurl. Run it inside the API container's network namespace so it
  # can reach port 9090. It arrives as 127.0.0.1 rather than the bridge
  # gateway, so it gets its own budget: enough to prove the interceptor is
  # enforcing, not that the budget is shared. Cross-protocol sharing is
  # covered by TestBudgetIsSharedAcrossHTTPAndGRPC.
  info "no host grpcurl; running it inside the API container's network namespace"
  info "it arrives as a different client IP, so it starts with its own budget of $BUDGET"

  # This client has its own bucket, which a previous run of this script may
  # have already drained. Let it refill so the count below is deterministic.
  info "waiting ${FULL_REFILL_SECONDS}s for that client's bucket to refill"
  sleep $FULL_REFILL_SECONDS

  # The whole loop runs inside one container. Spawning a container per call
  # would take seconds each, long enough for the bucket to refill mid-run and
  # make the count meaningless. No `head` in the pipeline either: the SIGPIPE
  # it sends kills the busybox loop after two iterations.
  codes="$(docker run --rm --network "container:$API_CID" --entrypoint sh \
    fullstorydev/grpcurl:latest-alpine -c \
    "for i in \$(seq 1 $((BUDGET + 2))); do grpcurl -plaintext -d '$GRPC_PAYLOAD' localhost:9090 pb.SimpleBank/LoginUser 2>&1 | grep -oE 'Code: [A-Za-z]+' || echo 'Code: OK'; done" 2>/dev/null)"

  GRPC_OK=0
  GRPC_LIMITED=0
  i=0
  while IFS= read -r line; do
    i=$((i+1))
    verdict="${line#Code: }"
    if [ "$verdict" = "ResourceExhausted" ]; then
      GRPC_LIMITED=$((GRPC_LIMITED+1))
    else
      GRPC_OK=$((GRPC_OK+1))
    fi
    printf '      call %-2s -> %s\n' "$i" "$verdict"
  done <<< "$codes"

  check "gRPC allowed exactly $BUDGET calls" "$GRPC_OK" "$BUDGET"
  check "gRPC rejected the rest with ResourceExhausted" "$GRPC_LIMITED" "2"
  info "cross-protocol sharing of one budget is covered by TestBudgetIsSharedAcrossHTTPAndGRPC"

else
  skip "grpcurl not installed and no compose stack to borrow; run 'make evans' and call LoginUser repeatedly"
fi

# ---------------------------------------------------------------------------
printf '\n%s==> Result%s\n' "$BOLD" "$OFF"
printf '    %s%d passed%s, %s%d failed%s\n' "$GREEN" "$PASSED" "$OFF" \
  "$([ "$FAILED" -gt 0 ] && echo "$RED" || echo "$GREEN")" "$FAILED" "$OFF"

[ "$FAILED" -eq 0 ] || exit 1
