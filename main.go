package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rakyll/statik/fs"
	"github.com/rs/cors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	db "github.com/techschool/simplebank/db/sqlc"
	_ "github.com/techschool/simplebank/doc/statik"
	"github.com/techschool/simplebank/gapi"
	"github.com/techschool/simplebank/mail"
	"github.com/techschool/simplebank/pb"
	"github.com/techschool/simplebank/ratelimit"
	"github.com/techschool/simplebank/util"
	"github.com/techschool/simplebank/worker"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
)

var interruptSignals = []os.Signal{
	os.Interrupt,
	syscall.SIGTERM,
	syscall.SIGINT,
}

func main() {
	config, err := util.LoadConfig(".")
	if err != nil {
		log.Fatal().Err(err).Msg("cannot load config")
	}

	if config.Environment == "development" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	ctx, stop := signal.NotifyContext(context.Background(), interruptSignals...)
	defer stop()

	connPool, err := pgxpool.New(ctx, config.DBSource)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot connect to db")
	}

	runDBMigration(config.MigrationURL, config.DBSource)

	store := db.NewStore(connPool)

	redisOpt := asynq.RedisClientOpt{
		Addr: config.RedisAddress,
	}

	taskDistributor := worker.NewRedisTaskDistributor(redisOpt)

	// One limiter, shared by both servers. Constructing it here rather than
	// inside each run* function is what makes the budget global: a client
	// cannot get a second allowance by switching from HTTP to gRPC.
	rateLimiter := newRateLimiter(config)

	waitGroup, ctx := errgroup.WithContext(ctx)

	runTaskProcessor(ctx, waitGroup, config, redisOpt, store)
	runRateLimitSweeper(ctx, waitGroup, rateLimiter)
	runGatewayServer(ctx, waitGroup, config, store, taskDistributor, rateLimiter)
	runGrpcServer(ctx, waitGroup, config, store, taskDistributor, rateLimiter)

	err = waitGroup.Wait()
	if err != nil {
		log.Fatal().Err(err).Msg("error from wait group")
	}
}

func runDBMigration(migrationURL string, dbSource string) {
	migration, err := migrate.New(migrationURL, dbSource)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create new migrate instance")
	}

	if err = migration.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatal().Err(err).Msg("failed to run migrate up")
	}

	log.Info().Msg("db migrated successfully")
}

func runTaskProcessor(
	ctx context.Context,
	waitGroup *errgroup.Group,
	config util.Config,
	redisOpt asynq.RedisClientOpt,
	store db.Store,
) {
	mailer := mail.NewGmailSender(config.EmailSenderName, config.EmailSenderAddress, config.EmailSenderPassword)
	taskProcessor := worker.NewRedisTaskProcessor(redisOpt, store, mailer)

	log.Info().Msg("start task processor")
	err := taskProcessor.Start()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to start task processor")
	}

	waitGroup.Go(func() error {
		<-ctx.Done()
		log.Info().Msg("graceful shutdown task processor")

		taskProcessor.Shutdown()
		log.Info().Msg("task processor is stopped")

		return nil
	})
}

func runGrpcServer(
	ctx context.Context,
	waitGroup *errgroup.Group,
	config util.Config,
	store db.Store,
	taskDistributor worker.TaskDistributor,
	rateLimiter *ratelimit.Service,
) {
	server, err := gapi.NewServer(config, store, taskDistributor)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create server")
	}

	// Rate limit first, so a throttled call is turned away before the handler
	// does any work. GrpcLogger still records everything that gets through.
	grpcInterceptors := grpc.ChainUnaryInterceptor(
		gapi.RateLimitInterceptor(rateLimiter),
		gapi.GrpcLogger,
	)
	grpcServer := grpc.NewServer(grpcInterceptors)
	pb.RegisterSimpleBankServer(grpcServer, server)
	reflection.Register(grpcServer)

	listener, err := net.Listen("tcp", config.GRPCServerAddress)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create listener")
	}

	waitGroup.Go(func() error {
		log.Info().Msgf("start gRPC server at %s", listener.Addr().String())

		err = grpcServer.Serve(listener)
		if err != nil {
			if errors.Is(err, grpc.ErrServerStopped) {
				return nil
			}
			log.Error().Err(err).Msg("gRPC server failed to serve")
			return err
		}

		return nil
	})

	waitGroup.Go(func() error {
		<-ctx.Done()
		log.Info().Msg("graceful shutdown gRPC server")

		grpcServer.GracefulStop()
		log.Info().Msg("gRPC server is stopped")

		return nil
	})
}

func runGatewayServer(
	ctx context.Context,
	waitGroup *errgroup.Group,
	config util.Config,
	store db.Store,
	taskDistributor worker.TaskDistributor,
	rateLimiter *ratelimit.Service,
) {
	server, err := gapi.NewServer(config, store, taskDistributor)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create server")
	}

	jsonOption := runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
		MarshalOptions: protojson.MarshalOptions{
			UseProtoNames: true,
		},
		UnmarshalOptions: protojson.UnmarshalOptions{
			DiscardUnknown: true,
		},
	})

	grpcMux := runtime.NewServeMux(jsonOption)

	err = pb.RegisterSimpleBankHandlerServer(ctx, grpcMux, server)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot register handler server")
	}

	mux := http.NewServeMux()
	mux.Handle("/", grpcMux)

	statikFS, err := fs.New()
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create statik fs")
	}

	swaggerHandler := http.StripPrefix("/swagger/", http.FileServer(statikFS))
	mux.Handle("/swagger/", swaggerHandler)

	c := cors.New(cors.Options{
		AllowedOrigins: config.AllowedOrigins,
		AllowedMethods: []string{
			http.MethodHead,
			http.MethodOptions,
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
		},
		AllowedHeaders: []string{
			"Content-Type",
			"Authorization",
		},
		AllowCredentials: true,
	})
	// cors -> ratelimit -> logger -> mux. The rate limiter sits inside the
	// CORS handler on purpose: a 429 written outside it would carry no
	// Access-Control-Allow-Origin header, and the browser would report an
	// opaque CORS failure instead of the rate limit the user actually hit.
	handler := c.Handler(gapi.RateLimitMiddleware(rateLimiter, gapi.HttpLogger(mux)))

	httpServer := &http.Server{
		Handler: handler,
		Addr:    config.HTTPServerAddress,
	}

	waitGroup.Go(func() error {
		log.Info().Msgf("start HTTP gateway server at %s", httpServer.Addr)
		err = httpServer.ListenAndServe()
		if err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			log.Error().Err(err).Msg("HTTP gateway server failed to serve")
			return err
		}
		return nil
	})

	waitGroup.Go(func() error {
		<-ctx.Done()
		log.Info().Msg("graceful shutdown HTTP gateway server")

		err := httpServer.Shutdown(context.Background())
		if err != nil {
			log.Error().Err(err).Msg("failed to shutdown HTTP gateway server")
			return err
		}

		log.Info().Msg("HTTP gateway server is stopped")
		return nil
	})
}

// newRateLimiter loads the rate limit rules and builds the shared limiter.
//
// A config file that will not parse is fatal. Starting up with rules that
// silently evaluated to nothing, while the operator believes the service is
// protected, is worse than not starting at all.
func newRateLimiter(config util.Config) *ratelimit.Service {
	if !config.RateLimitEnabled {
		log.Warn().Msg("rate limiting is disabled")
		return ratelimit.NewService(&ratelimit.Config{Enabled: false})
	}

	path := config.RateLimitConfigPath
	if path == "" {
		path = "ratelimit.yaml"
	}

	rateLimitConfig, err := ratelimit.LoadConfig(path)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot load rate limit config")
	}

	// RATE_LIMIT_ENABLED is the operational kill switch and outranks the file,
	// so the limiter can be turned off with an environment variable alone.
	rateLimitConfig.Enabled = true

	log.Info().
		Str("algorithm", rateLimitConfig.Algorithm).
		Str("enforcement", rateLimitConfig.Enforcement).
		Int("default_requests", rateLimitConfig.Default.Requests).
		Str("default_per", rateLimitConfig.Default.Per.String()).
		Int("default_burst", rateLimitConfig.Default.Burst).
		Int("rules", len(rateLimitConfig.Rules)).
		Msgf("rate limiting enabled from %s", path)

	return ratelimit.NewService(rateLimitConfig)
}

// runRateLimitSweeper reclaims buckets for clients that have gone quiet, so
// the per-IP map does not grow without bound.
func runRateLimitSweeper(ctx context.Context, waitGroup *errgroup.Group, rateLimiter *ratelimit.Service) {
	waitGroup.Go(func() error {
		return rateLimiter.Run(ctx)
	})
}
