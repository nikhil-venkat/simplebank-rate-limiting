# Simple Bank

A backend web service for a simple bank, built with Go. Based on the [Backend Master Class](https://bit.ly/backendmaster) course by [TECH SCHOOL](https://bit.ly/m/techschool) (also on [Udemy](https://bit.ly/backendudemy)).

The server exposes both **gRPC** (port 9090) and **HTTP** (port 8080, via grpc-gateway) APIs. A minimal **Vue 3** frontend (port 3000) provides a login UI.

## Available APIs

| Method  | Path              | Description                                      | Auth required |
|---------|-------------------|--------------------------------------------------|---------------|
| `POST`  | `/v1/create_user` | Register a new user                              | No            |
| `POST`  | `/v1/login_user`  | Login, returns access and refresh tokens         | No            |
| `PATCH` | `/v1/update_user` | Update user profile                              | Yes           |
| `GET`   | `/v1/verify_email` | Verify email address via link sent after signup  | No            |

All four endpoints are rate limited per client IP. See [Rate limiting](#rate-limiting).

Swagger documentation is served at `/swagger/` when the server is running.

The same endpoints are available as gRPC methods on port 9090 (service `pb.SimpleBank`).

> **Note:** The database layer includes schemas for accounts, entries, and transfers, but these are not yet exposed as API endpoints.

## Running the service

### With Docker (recommended)

Requires [Docker](https://www.docker.com/products/docker-desktop) only.

```bash
docker compose up --build
```

This starts all services:

| Service    | URL                     |
|------------|-------------------------|
| HTTP API   | http://localhost:8080   |
| gRPC API   | localhost:9090          |
| Swagger UI | http://localhost:8080/swagger/ |
| Frontend   | http://localhost:3000   |

Database migrations run automatically on startup.

To stop everything:

```bash
docker compose down
```

### Without Docker

Requires [Go](https://golang.org/), [PostgreSQL](https://www.postgresql.org/), [Redis](https://redis.io/), and [golang-migrate](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate) installed locally.

1. Start Postgres and create the database:

```bash
make postgres
make createdb
make migrateup
```

2. Start Redis:

```bash
make redis
```

3. Start the backend server:

```bash
make server
```

4. Start the frontend (in a separate terminal):

```bash
cd frontend
npm install
npm run dev
```

## Rate limiting

Requests are rate limited per client IP inside the application service layer,
with no load balancer or proxy assumed in front of the service.

**One budget per IP, shared by everything.** The HTTP gateway and the gRPC
server are handed the same limiter at startup, so a client cannot get a second
allowance by switching protocol. Every API draws on that one budget: spending
it on `/v1/login_user` leaves nothing for `/v1/create_user`. No endpoint is
given a larger or smaller allowance than any other.

Both servers need their own enforcement point. The gateway is registered with
`RegisterSimpleBankHandlerServer`, the in-process variant, so an HTTP request
calls the service method as a plain Go function and never crosses a gRPC
connection. A `grpc.UnaryInterceptor` therefore never runs for port 8080.
`gapi.RateLimitMiddleware` covers HTTP, `gapi.RateLimitInterceptor` covers
gRPC, and both are thin wrappers over the same `ratelimit.Service`.

The client IP always comes from the TCP peer, never from `X-Forwarded-For`.
With no proxy in front of the service that header is attacker-controlled, and
a client that varied it could mint a fresh budget on every request.

### Rules

Limits live in [`ratelimit.yaml`](ratelimit.yaml), which is mounted into the
container so it can be edited and applied with a restart rather than a rebuild.

```yaml
enabled: true
algorithm: token_bucket     # or fixed_window
enforcement: all_clients    # or listed_only

default:                    # applies to any IP no rule matches
  requests: 10
  per: 1s
  burst: 20

exempt:                     # never limited, checked first
  - 10.0.0.0/8

rules:                      # per-IP and per-CIDR overrides
  - ip: 203.0.113.42        # a single abusive client
    requests: 1
    per: 1s
    burst: 1
  - ip: 198.51.100.0/24     # a noisy partner range
    requests: 5
    per: 1s
    burst: 10

max_tracked_ips: 100000
```

Every `ip` accepts a bare address or a CIDR block, and the most specific prefix
wins regardless of the order rules are written in. A malformed file is a fatal
error at startup: booting with rules that silently parsed to nothing, while you
believe the service is protected, is worse than not booting.

| Algorithm | Behaviour | Trade-off |
|---|---|---|
| `token_bucket` (default) | Refills steadily up to `burst`, spends one token per request | Two numbers to tune |
| `fixed_window` | Counts requests per window, resets on rollover | Up to 2x the limit can land across a window boundary |

### When a client is throttled

HTTP returns `429 Too Many Requests` with `Retry-After` in seconds, plus
`X-RateLimit-Limit`, `X-RateLimit-Remaining`, and `X-RateLimit-Reset`. The body
uses the same shape grpc-gateway produces for every other error:

```json
{
  "code": 8,
  "message": "rate limit exceeded: too many requests from your IP address, please retry after 2 seconds",
  "details": [
    { "@type": "type.googleapis.com/google.rpc.RetryInfo", "retry_delay": "2s" }
  ]
}
```

gRPC returns `ResourceExhausted` with the same message and the same `RetryInfo`
detail attached.

CORS preflights and the `/swagger/` static assets are not charged to the
budget. Neither is an API call, and the Swagger UI pulls dozens of files on a
single page load.

### Validating it

Run the end-to-end script. It brings up the stack with a deliberately tiny
budget, trips the limit, and checks the response, the recovery, and the fact
that the budget really is shared:

```bash
make validate-ratelimit
```

Add `--keep` to leave the stack running, or `--no-up` to check a server you
started yourself.

Run the unit and integration tests, including the race detector:

```bash
make test-ratelimit
```

To try it by hand, point the server at the small budget in
[`ratelimit.validate.yaml`](ratelimit.validate.yaml) and loop:

```bash
for i in $(seq 1 10); do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -X POST http://localhost:8080/v1/login_user \
    -H 'Content-Type: application/json' \
    -d '{"username":"someone","password":"secret123"}'
done
```

> **Under Docker,** the API does not see `127.0.0.1`. It sees the gateway the
> host reaches it through: `172.16.0.0/12` on Linux, `192.168.65.1` on Docker
> Desktop for macOS. So a rule written for `127.0.0.1` has no effect on a
> containerized run, and exempting either range would exempt all of your local
> traffic. To find the address the server actually sees, trip the limit once
> and read `client_ip` from the log:
>
> ```bash
> docker compose logs api | grep "rate limit exceeded" | tail -1
> ```
>
> Or run with `make server` on the host to test rules for specific addresses.

## Running tests

### With Docker (recommended)

Spins up Postgres in a container, runs migrations, executes all tests, and tears everything down:

```bash
make dockertest
```

To run a specific test, start the test dependencies first:

```bash
docker compose -f docker-compose.test.yaml run --rm migrate
go test -v ./db/sqlc/ -run TestCreateUser
docker compose -f docker-compose.test.yaml down
```

### Without Docker

Requires Postgres running locally with the `simple_bank` database created and migrations applied:

```bash
make test
```

## Development

### Code generation

These commands regenerate code from source definitions. You only need them if you modify the corresponding source files.

Generate SQL CRUD code from queries (requires [sqlc](https://github.com/kyleconroy/sqlc)):

```bash
make sqlc
```

Generate gRPC/gateway Go code from proto files (requires `protoc` with Go plugins):

```bash
make proto
```

Generate DB mocks for testing (requires [gomock](https://github.com/golang/mock)):

```bash
make mock
```

### Database migrations

Create a new migration file:

```bash
make new_migration name=<migration_name>
```

Run migrations up or down (one version at a time):

```bash
make migrateup1
make migratedown1
```

### Database documentation

Generate and publish DB docs (requires [dbdocs](https://dbdocs.io/docs) and [DBML CLI](https://www.dbml.org/cli/)):

```bash
make db_docs
make db_schema
```

View the published documentation at [dbdocs.io/techschool.guru/simple_bank](https://dbdocs.io/techschool.guru/simple_bank) (password: `secret`).

### gRPC client

Connect to the gRPC server interactively using [Evans](https://github.com/ktr0731/evans):

```bash
make evans
```

## Configuration

The server reads configuration from `app.env` (and environment variables, which take precedence). Key settings:

| Variable               | Default                          | Description                     |
|------------------------|----------------------------------|---------------------------------|
| `DB_SOURCE`            | `postgresql://root:secret@localhost:5432/simple_bank?sslmode=disable` | Postgres connection string |
| `HTTP_SERVER_ADDRESS`  | `0.0.0.0:8080`                   | HTTP gateway listen address     |
| `GRPC_SERVER_ADDRESS`  | `0.0.0.0:9090`                   | gRPC server listen address      |
| `REDIS_ADDRESS`        | `0.0.0.0:6379`                   | Redis address for async workers |
| `TOKEN_SYMMETRIC_KEY`  | *(set in app.env)*               | 32-byte key for PASETO tokens   |
| `ACCESS_TOKEN_DURATION`| `1m`                             | Access token lifetime           |
| `REFRESH_TOKEN_DURATION`| `24h`                           | Refresh token lifetime          |
| `ALLOWED_ORIGINS`      | `http://localhost:3000`          | CORS allowed origins            |
| `RATE_LIMIT_ENABLED`   | `true`                           | Master switch for rate limiting |
| `RATE_LIMIT_CONFIG_PATH`| `ratelimit.yaml`                | Path to the rate limit rules    |
