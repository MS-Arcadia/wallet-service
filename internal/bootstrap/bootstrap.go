// Package bootstrap assembles the service.
//
// This is the only place in the codebase where a concrete database, broker or cache
// is chosen. Everything above it depends on interfaces, which is what makes the
// domain and application layers testable without any of them running. Composition
// happens here, explicitly, with no reflection-based container: a wiring bug is a
// compile error.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/adapter/in/consumer"
	"github.com/MS-Arcadia/wallet-service/internal/adapter/in/grpcapi"
	"github.com/MS-Arcadia/wallet-service/internal/adapter/in/restapi"
	"github.com/MS-Arcadia/wallet-service/internal/adapter/out/paymentgw"
	"github.com/MS-Arcadia/wallet-service/internal/adapter/out/publisher"
	"github.com/MS-Arcadia/wallet-service/internal/adapter/out/ratelimit"
	"github.com/MS-Arcadia/wallet-service/internal/adapter/out/repo"
	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/config"
	"github.com/MS-Arcadia/wallet-service/internal/domain/abuse"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/interest"
	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/clock"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/grpcx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/health"
	"github.com/MS-Arcadia/wallet-service/internal/platform/httpx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/idgen"
	"github.com/MS-Arcadia/wallet-service/internal/platform/kafkax"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/metrics"
	"github.com/MS-Arcadia/wallet-service/internal/platform/migrate"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/MS-Arcadia/wallet-service/internal/platform/outbox"
	"github.com/MS-Arcadia/wallet-service/internal/platform/postgres"
	"github.com/MS-Arcadia/wallet-service/internal/platform/redisx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/runtimex"
	walletmigrations "github.com/MS-Arcadia/wallet-service/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// App is the assembled service.
type App struct {
	cfg      config.Config
	logger   *slog.Logger
	pool     *pgxpool.Pool
	redis    *redisx.Client
	producer *kafkax.Producer
	metrics  *metrics.Registry
	health   *health.Registry

	dispatcher *outbox.Dispatcher
	consumers  []*kafkax.Consumer

	wallets   *app.WalletService
	giftCards *app.GiftCardService
	discounts *app.DiscountService
	charges   *app.ChargeService
	admin     *app.AdminService

	grpcServer *grpcx.Server
	httpServer *httpx.Server
	closers    []func(context.Context) error
}

// New builds the service from configuration. Anything that cannot be provisioned
// causes a boot failure rather than a service that half works.
func New(ctx context.Context, cfg config.Config) (*App, error) {
	logger := logx.New(logx.Config{
		Level:       cfg.Service.LogLevel,
		Format:      cfg.Service.LogFormat,
		Service:     cfg.Service.Name,
		Version:     cfg.Service.Version,
		Environment: cfg.Service.Environment,
	})
	slog.SetDefault(logger)

	application := &App{
		cfg:     cfg,
		logger:  logger,
		metrics: metrics.New(cfg.Service.Name),
		health:  health.NewRegistry(cfg.Service.Name, cfg.Service.Version),
	}

	if err := application.initDatabase(ctx); err != nil {
		return nil, err
	}
	if err := application.initRedis(ctx); err != nil {
		return nil, err
	}
	if err := application.initKafka(ctx); err != nil {
		return nil, err
	}
	if err := application.initUseCases(ctx); err != nil {
		return nil, err
	}
	if err := application.initTransports(); err != nil {
		return nil, err
	}
	application.initConsumers()

	return application, nil
}

func (a *App) initDatabase(ctx context.Context) error {
	pool, err := postgres.Connect(ctx, postgres.Config{
		DSN:              a.cfg.Database.DSN,
		MaxConns:         a.cfg.Database.MaxConns,
		MinConns:         a.cfg.Database.MinConns,
		MaxConnLifetime:  a.cfg.Database.MaxConnLifetime,
		MaxConnIdleTime:  a.cfg.Database.MaxConnIdleTime,
		ConnectTimeout:   a.cfg.Database.ConnectTimeout,
		ApplicationName:  a.cfg.Service.Name,
		StatementTimeout: a.cfg.Database.StatementTimeout,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: database: %w", err)
	}
	a.pool = pool
	a.closers = append(a.closers, func(context.Context) error {
		pool.Close()
		return nil
	})

	// Postgres is the one dependency the service genuinely cannot serve without: with
	// no ledger there is no wallet.
	a.health.Register(health.Check{
		Name:     "postgres",
		Critical: true,
		Timeout:  2 * time.Second,
		Probe:    func(ctx context.Context) error { return pool.Ping(ctx) },
	})

	if a.cfg.Database.RunMigrations {
		if err := a.runMigrations(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) runMigrations(ctx context.Context) error {
	files, err := migrate.Load(walletmigrations.FS, walletmigrations.Dir)
	if err != nil {
		return fmt.Errorf("bootstrap: load migrations: %w", err)
	}

	runner, closeRunner, err := migrate.Connect(ctx, a.cfg.Database.DSN, a.logger)
	if err != nil {
		return fmt.Errorf("bootstrap: migration connection: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = closeRunner(closeCtx)
	}()

	// A bounded deadline: an advisory lock held by a stuck pod must not hang this boot
	// forever, because the readiness probe would never fail and the rollout would stall.
	migrateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := runner.Up(migrateCtx, files); err != nil {
		return fmt.Errorf("bootstrap: apply migrations: %w", err)
	}

	version, err := runner.Version(migrateCtx)
	if err == nil {
		a.logger.Info("database schema ready", slog.Int64("version", version))
	}
	return nil
}

func (a *App) initRedis(ctx context.Context) error {
	if !a.cfg.Redis.Enabled {
		// Explicitly disabled. The gift-card abuse rule will not be enforced, which is a
		// deliberate trade-off worth saying out loud rather than discovering later.
		a.logger.Warn("redis is disabled; gift card abuse detection will not be enforced")
		return nil
	}

	client, err := redisx.Connect(ctx, redisx.Config{
		Addr:     a.cfg.Redis.Addr,
		Password: a.cfg.Redis.Password,
		DB:       a.cfg.Redis.DB,
		PoolSize: a.cfg.Redis.PoolSize,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: redis: %w", err)
	}
	a.redis = client
	a.closers = append(a.closers, func(context.Context) error { return client.Close() })

	// Not critical: losing Redis degrades abuse detection but must not take wallet
	// reads out of rotation. This is the bulkhead tactic from the architecture doc.
	a.health.Register(health.Check{
		Name:     "redis",
		Critical: false,
		Timeout:  time.Second,
		Probe:    client.Ping,
	})
	return nil
}

func (a *App) initKafka(ctx context.Context) error {
	if !a.cfg.Kafka.Enabled {
		a.logger.Warn("kafka is disabled; events will accumulate in the outbox unpublished")
		return nil
	}

	// Compose brings the broker up alongside this service, so a short wait avoids a
	// pointless crash-loop while Kafka elects a controller.
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := kafkax.WaitForBrokers(waitCtx, a.cfg.Kafka.Brokers, 60*time.Second, a.logger); err != nil {
		return fmt.Errorf("bootstrap: kafka unreachable: %w", err)
	}

	producer, err := kafkax.NewProducer(kafkax.ProducerConfig{
		Brokers:     a.cfg.Kafka.Brokers,
		ClientID:    a.cfg.Kafka.ClientID,
		Compression: true,
	}, a.logger)
	if err != nil {
		return fmt.Errorf("bootstrap: kafka producer: %w", err)
	}
	a.producer = producer
	a.closers = append(a.closers, func(context.Context) error { return producer.Close() })

	if a.cfg.Kafka.EnsureTopics {
		if err := a.ensureTopics(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ensureTopics declares the topics this service owns, plus a dead-letter topic for
// every topic it consumes.
//
// Declaring them in code documents ownership: the wallet service owns wallet-events
// and audit-events, and nothing else may produce to them.
func (a *App) ensureTopics(ctx context.Context) error {
	const (
		financialRetention = int64(90 * 24 * time.Hour / time.Millisecond)
		auditRetention     = int64(365 * 24 * time.Hour / time.Millisecond)
	)

	specs := []kafkax.TopicSpec{
		{
			Name:              a.cfg.Kafka.WalletEventsTopic,
			Partitions:        a.cfg.Kafka.TopicPartitions,
			ReplicationFactor: a.cfg.Kafka.TopicReplication,
			RetentionMs:       financialRetention,
		},
		{
			// The audit trail is kept far longer than a domain topic: it exists to answer
			// questions asked months after the fact.
			Name:              a.cfg.Kafka.AuditEventsTopic,
			Partitions:        a.cfg.Kafka.TopicPartitions,
			ReplicationFactor: a.cfg.Kafka.TopicReplication,
			RetentionMs:       auditRetention,
		},
	}

	// The command topic this service consumes, and a DLQ per consumed topic.
	//
	// Creating an inbound topic looks like the producer's job, and it was left to it — with
	// the result that nobody created wallet-commands at all, because the Store service
	// regarded it as the wallet's and the wallet regarded it as the Store's. Broker-side
	// auto-creation is off, so the first DebitWalletCommand was published to a topic that did
	// not exist and the purchase sat in PENDING with no error anywhere near it.
	//
	// The consumer creates it because the consumer's partition count is what determines its
	// own ordering and how far it can scale. A producer guessing that number would silently
	// cap this service. Both sides declaring it is harmless: creation is idempotent.
	specs = append(specs, kafkax.TopicSpec{
		Name:              a.cfg.Kafka.WalletCommandsTopic,
		Partitions:        a.cfg.Kafka.TopicPartitions,
		ReplicationFactor: a.cfg.Kafka.TopicReplication,
		RetentionMs:       financialRetention,
	})

	// A DLQ per consumed topic. Poison messages must land somewhere an operator looks,
	// not be dropped.
	for _, topic := range a.consumedTopics() {
		specs = append(specs, kafkax.TopicSpec{
			Name:              topic + ".dlq",
			Partitions:        1,
			ReplicationFactor: a.cfg.Kafka.TopicReplication,
			RetentionMs:       auditRetention,
		})
	}

	if err := kafkax.EnsureTopics(ctx, a.cfg.Kafka.Brokers, specs, a.logger); err != nil {
		return fmt.Errorf("bootstrap: ensure topics: %w", err)
	}
	return nil
}

func (a *App) consumedTopics() []string {
	return []string{
		a.cfg.Kafka.PaymentEventsTopic,
		a.cfg.Kafka.UserEventsTopic,
		a.cfg.Kafka.WalletCommandsTopic,
		a.cfg.Kafka.TradeEventsTopic,
	}
}

func (a *App) initUseCases(ctx context.Context) error {
	repo.SetDefaultCurrency(a.cfg.Wallet.Currency)

	txManager := postgres.NewTxManager(a.pool)
	outboxStore := outbox.NewStore(10)

	// The dispatcher is created before the publisher so that the publisher can hand
	// out its Notify method: a use case that commits an event wakes the dispatcher
	// immediately instead of waiting for the next poll.
	var publishTarget outbox.Publisher = noopPublisher{logger: a.logger}
	if a.producer != nil {
		publishTarget = a.producer
	}

	dispatcher := outbox.NewDispatcher(
		outboxStore, txManager, publishTarget, clock.System{}, a.logger, a.metrics,
		outbox.DispatcherConfig{
			PollInterval:   a.cfg.Kafka.OutboxPollInterval,
			BatchSize:      a.cfg.Kafka.OutboxBatchSize,
			PurgeInterval:  a.cfg.Jobs.PurgeInterval,
			PurgeRetention: a.cfg.Jobs.OutboxRetention,
		},
	)
	a.dispatcher = dispatcher

	eventPublisher := publisher.New(outboxStore, publisher.Topics{
		WalletEvents: a.cfg.Kafka.WalletEventsTopic,
		AuditEvents:  a.cfg.Kafka.AuditEventsTopic,
	}, dispatcher.Notify)

	// With Redis absent, an always-allow limiter keeps the service usable. The warning
	// at boot is the honest signal that a rule is not being enforced.
	var limiter port.AbuseLimiter = alwaysAllowLimiter{}
	abusePolicy, err := abuse.NewPolicy(
		a.cfg.Wallet.AbusePerMinute, a.cfg.Wallet.AbusePerHour, a.cfg.Wallet.AbuseFlagAt)
	if err != nil {
		return fmt.Errorf("bootstrap: abuse policy: %w", err)
	}
	if a.redis != nil {
		limiter = ratelimit.New(a.redis, abusePolicy, a.logger)
	}

	hasher, err := giftcard.NewHasher(a.cfg.Wallet.GiftCardPepper)
	if err != nil {
		return fmt.Errorf("bootstrap: gift card hasher: %w", err)
	}

	minBalance, err := money.New(a.cfg.Wallet.InterestMinBalance, a.cfg.Wallet.Currency)
	if err != nil {
		return fmt.Errorf("bootstrap: interest minimum balance: %w", err)
	}
	interestPolicy, err := interest.NewPolicy(interest.Config{
		AnnualRateBps:  a.cfg.Wallet.InterestAnnualRateBps,
		MinimumBalance: minBalance,
		Enabled:        a.cfg.Wallet.InterestEnabled,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: interest policy: %w", err)
	}

	// The Payment Adapter is optional: a deployment that only settles internal money
	// movement needs no bank. Attempting a top-up without it returns UNAVAILABLE rather
	// than panicking.
	var gateway port.PaymentGateway = unavailableGateway{}
	if a.cfg.Payment.GRPCTarget != "" {
		tokens, err := a.serviceTokenSource()
		if err != nil {
			return fmt.Errorf("bootstrap: payment adapter credentials: %w", err)
		}
		conn, err := grpcx.Dial(grpcx.ClientConfig{
			Target:       a.cfg.Payment.GRPCTarget,
			Timeout:      a.cfg.Payment.Timeout,
			ServiceToken: a.cfg.Auth.ServiceToken,
			TokenSource:  tokens,
			ServiceName:  a.cfg.Service.Name,
		}, a.logger)
		if err != nil {
			return fmt.Errorf("bootstrap: payment adapter client: %w", err)
		}
		a.closers = append(a.closers, func(context.Context) error { return conn.Close() })
		gateway = paymentgw.New(conn, a.logger)
	} else {
		a.logger.Warn("no payment adapter configured; bank top-ups are unavailable")
	}

	deps := app.Deps{
		TxManager:     txManager,
		Reader:        a.pool,
		Wallets:       repo.NewWalletRepo(),
		Ledger:        repo.NewLedgerRepo(),
		GiftCards:     repo.NewGiftCardRepo(),
		Discounts:     repo.NewDiscountRepo(),
		Holds:         repo.NewHoldRepo(),
		Idempotency:   repo.NewIdempotencyStore(),
		Publisher:     eventPublisher,
		PaymentGW:     gateway,
		AbuseLimiter:  limiter,
		Metrics:       a.metrics,
		Clock:         clock.System{},
		IDs:           idgen.UUIDv7{},
		Logger:        a.logger,
		Currency:      a.cfg.Wallet.Currency,
		Producer:      a.cfg.Service.Name,
		SchemaVersion: a.cfg.Wallet.SchemaVersion,
	}

	a.wallets = app.NewWalletService(deps)
	a.giftCards = app.NewGiftCardService(deps, hasher, abusePolicy)
	a.discounts = app.NewDiscountService(deps)
	a.charges = app.NewChargeService(deps)
	a.admin = app.NewAdminService(deps, interestPolicy)
	return nil
}

// serviceTokenSource mints the credential this service presents when it calls another one.
//
// Nil when SERVICE_TOKEN is set — that is a deployment where a real Auth service issues the
// credential, and self-signing over the top of it would be wrong.
//
// Otherwise the wallet signs its own, short-lived, with the shared HS256 secret it already holds
// to verify incoming tokens. That is the same thing the order service does to call this service's
// discount API, and it is only possible because the platform uses a symmetric secret — under
// RS256 this would need a private key the wallet has no business holding, and the answer would be
// an Auth service.
//
// This exists because the Payment Adapter's InitiatePayment requires a principal and its comment
// said "the wallet service calls this with a service token" — while the wallet sent nothing at
// all. Every bank top-up failed, and it failed as a 401, which tells a user to log in again.
func (a *App) serviceTokenSource() (func() (string, error), error) {
	if a.cfg.Auth.ServiceToken != "" {
		return nil, nil
	}
	if a.cfg.Auth.Algorithm != "HS256" {
		// Said plainly rather than falling back to anonymous, which is how this broke the first
		// time: the call goes out unauthenticated and the failure appears three services away.
		return nil, fmt.Errorf(
			"SERVICE_TOKEN is required with JWT_ALGORITHM=%s: this service can only sign its "+
				"own credential when the platform uses a symmetric secret",
			a.cfg.Auth.Algorithm)
	}

	issuer, err := authn.NewIssuer(authn.IssuerConfig{
		Secret:   a.cfg.Auth.Secret,
		Issuer:   a.cfg.Auth.Issuer,
		Audience: a.cfg.Auth.Audience,
		// Short, and re-minted per call. A long-lived service credential in memory is a
		// credential that outlives whatever leaks it.
		TTL: 5 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("service token issuer: %w", err)
	}

	name := a.cfg.Service.Name
	now := clock.System{}
	return func() (string, error) {
		// The subject is the service's name, not a user id. The callee only compares it against
		// the user a payment is for and never stores it, so a self-describing string is more use
		// in a log than a synthetic UUID would be.
		return issuer.Issue(name, authn.RoleService, "", now.Now())
	}, nil
}

func (a *App) initTransports() error {
	verifier, err := authn.NewVerifier(authn.VerifierConfig{
		Algorithm:    a.cfg.Auth.Algorithm,
		Secret:       a.cfg.Auth.Secret,
		PublicKeyPEM: a.cfg.Auth.PublicKey,
		Issuer:       a.cfg.Auth.Issuer,
		Audience:     a.cfg.Auth.Audience,
	})
	if err != nil {
		return fmt.Errorf("bootstrap: token verifier: %w", err)
	}

	if a.cfg.Server.Mode.ServesGRPC() {
		server := grpcx.NewServer(grpcx.ServerConfig{
			Addr:             a.cfg.Server.GRPCAddr,
			HandlerTimeout:   a.cfg.Server.HandlerTimeout,
			ShutdownTimeout:  a.cfg.Server.ShutdownTimeout,
			EnableReflection: a.cfg.Server.EnableReflection,
			ServiceName:      a.cfg.Service.Name,
		}, verifier, a.metrics, a.logger)

		grpcapi.NewWalletServer(a.wallets, a.charges, a.giftCards, a.cfg.Wallet.Currency).Register(server.Registrar())
		grpcapi.NewGiftCardServer(a.giftCards, a.cfg.Wallet.Currency).Register(server.Registrar())
		grpcapi.NewDiscountServer(a.discounts, a.cfg.Wallet.Currency).Register(server.Registrar())
		grpcapi.NewAdminServer(a.admin, a.cfg.Wallet.Currency).Register(server.Registrar())

		for _, name := range []string{
			"arcadia.wallet.v1.WalletService",
			"arcadia.wallet.v1.GiftCardService",
			"arcadia.wallet.v1.DiscountService",
			"arcadia.wallet.v1.WalletAdminService",
		} {
			server.SetServing(name, true)
		}
		server.SetServing("", true)
		a.grpcServer = server
	}

	// The HTTP listener always starts, even in grpc-only mode: the health check needs the
	// probes and Prometheus needs /metrics. Only the business routes are conditional.
	a.httpServer = httpx.NewServer(httpx.ServerConfig{
		Addr:            a.cfg.Server.HTTPAddr,
		ReadTimeout:     a.cfg.Server.ReadTimeout,
		WriteTimeout:    a.cfg.Server.WriteTimeout,
		ShutdownTimeout: a.cfg.Server.ShutdownTimeout,
	}, a.buildHTTPHandler(verifier), a.logger)
	return nil
}

func (a *App) buildHTTPHandler(verifier *authn.Verifier) http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints, unauthenticated and never logged as request traffic.
	mux.Handle("GET /metrics", a.metrics.Handler())
	mux.HandleFunc("GET /livez", a.health.LiveHandler())
	mux.HandleFunc("GET /readyz", a.health.ReadyHandler())
	// /healthz is an alias many tools probe by default.
	mux.HandleFunc("GET /healthz", a.health.LiveHandler())

	if a.cfg.Server.Mode.ServesHTTP() {
		restapi.New(a.wallets, a.giftCards, a.discounts, a.charges, a.admin,
			a.cfg.Wallet.Currency).Routes(mux)
		a.logger.Info("REST API enabled", slog.String("addr", a.cfg.Server.HTTPAddr))
	}

	// Outermost first. Recovery wraps everything so that a panic in any later layer
	// still produces a response; authentication runs late so that its own failures are
	// already inside the logging and tracing spans.
	chain := httpx.Chain(
		httpx.Recover(a.logger),
		httpx.RequestID(),
		httpx.SecurityHeaders(),
		httpx.CORS(a.cfg.Server.CORSOrigins),
		httpx.Logging(a.logger),
		httpx.Instrument(a.metrics),
		httpx.Timeout(a.cfg.Server.HandlerTimeout),
		httpx.MaxBodyBytes(a.cfg.Server.MaxBodyBytes),
		httpx.Authenticate(verifier),
	)
	return chain(mux)
}

func (a *App) initConsumers() {
	if a.producer == nil {
		return
	}

	handlers := consumer.NewHandlers(a.wallets, a.charges, a.cfg.Wallet.Currency, a.logger)

	subscriptions := []struct {
		topic   string
		suffix  string
		handler kafkax.Handler
	}{
		{a.cfg.Kafka.PaymentEventsTopic, "payments", handlers.PaymentEventsRouter()},
		{a.cfg.Kafka.UserEventsTopic, "users", handlers.UserEventsRouter()},
		{a.cfg.Kafka.WalletCommandsTopic, "commands", handlers.WalletCommandsRouter()},
		{a.cfg.Kafka.TradeEventsTopic, "trades", handlers.TradeEventsRouter()},
	}

	for _, subscription := range subscriptions {
		// One consumer group per topic rather than one group across all of them: a
		// rebalance or a stuck partition on trade-events must not stall the purchase
		// saga's command topic.
		c, err := kafkax.NewConsumer(kafkax.ConsumerConfig{
			Brokers:         a.cfg.Kafka.Brokers,
			GroupID:         fmt.Sprintf("%s.%s", a.cfg.Kafka.GroupID, subscription.suffix),
			Topics:          []string{subscription.topic},
			DLQTopic:        subscription.topic + ".dlq",
			MaxRetries:      a.cfg.Kafka.ConsumerMaxRetry,
			HandlerTimeout:  a.cfg.Server.HandlerTimeout,
			StartFromOldest: a.cfg.Kafka.StartFromOldest,
		}, subscription.handler, a.producer, a.logger, a.metrics)
		if err != nil {
			// A misconfigured consumer is a boot-time bug; log it and carry on serving the
			// synchronous API rather than refusing to start at all.
			a.logger.Error("failed to create a consumer",
				slog.String("topic", subscription.topic),
				slog.String("error", err.Error()),
			)
			continue
		}
		a.consumers = append(a.consumers, c)
		a.closers = append(a.closers, func(context.Context) error { return c.Close() })
	}
}

// Run starts every component and blocks until shutdown completes.
func (a *App) Run(ctx context.Context) error {
	group := runtimex.NewGroup(a.logger, a.cfg.Server.ShutdownTimeout+10*time.Second)

	group.Add("http-server", a.httpServer.Run)
	if a.grpcServer != nil {
		group.Add("grpc-server", a.grpcServer.Run)
	}
	group.Add("outbox-dispatcher", a.dispatcher.Run)

	for i, c := range a.consumers {
		consumerRef := c
		group.Add(fmt.Sprintf("kafka-consumer-%d", i), consumerRef.Run)
	}

	a.addSchedulers(group)

	// Readiness flips only once everything is wired, so a pod being rolled out does not
	// receive traffic while migrations are still running.
	a.health.MarkReady()
	a.logger.Info("wallet service ready",
		slog.String("mode", string(a.cfg.Server.Mode)),
		slog.String("http", a.cfg.Server.HTTPAddr),
		slog.String("grpc", a.cfg.Server.GRPCAddr),
		slog.String("currency", a.cfg.Wallet.Currency),
		slog.Int("consumers", len(a.consumers)),
	)

	// Fail readiness the instant a signal arrives, so the load balancer drains this
	// instance before the listeners go away.
	group.PreStop("drain", func(context.Context) error {
		a.health.MarkShuttingDown()
		a.logger.Info("readiness withdrawn; draining traffic")
		return nil
	})

	for _, closer := range a.closers {
		group.OnShutdown("closer", closer)
	}

	return group.Run(ctx)
}

// addSchedulers registers the periodic jobs.
func (a *App) addSchedulers(group *runtimex.Group) {
	// Staggered initial delays so that a fresh pod does not run every job at once.
	group.Add("job-hold-sweeper", runtimex.NewScheduler(runtimex.SchedulerConfig{
		Name:         "hold-sweeper",
		Interval:     a.cfg.Jobs.HoldSweepInterval,
		InitialDelay: 30 * time.Second,
	}, func(ctx context.Context) error {
		_, err := a.wallets.ExpireHolds(asService(ctx), 200)
		return err
	}, a.logger).Run)

	group.Add("job-reconciler", runtimex.NewScheduler(runtimex.SchedulerConfig{
		Name:         "reconciler",
		Interval:     a.cfg.Jobs.ReconcileInterval,
		InitialDelay: time.Minute,
	}, func(ctx context.Context) error {
		result, err := a.admin.Reconcile(asService(ctx), "")
		if err != nil {
			return err
		}
		if len(result.Mismatches) > 0 {
			// The gauge is what pages on-call; this line is what tells them where to look.
			a.logger.Error("reconciliation found ledger mismatches",
				slog.Int("count", len(result.Mismatches)))
		}
		return nil
	}, a.logger).Run)

	group.Add("job-interest", runtimex.NewScheduler(runtimex.SchedulerConfig{
		Name:         "interest-accrual",
		Interval:     a.cfg.Jobs.InterestInterval,
		InitialDelay: 2 * time.Minute,
	}, func(ctx context.Context) error {
		// Safe to run on any schedule: the per-wallet-per-date idempotency key means a
		// second run on the same day credits nothing.
		_, err := a.admin.AccrueInterest(asService(ctx), app.AccrueInterestCommand{})
		return err
	}, a.logger).Run)

	group.Add("job-outbox-metrics", runtimex.NewScheduler(runtimex.SchedulerConfig{
		Name:         "outbox-metrics",
		Interval:     a.cfg.Jobs.OutboxMetricsInterval,
		InitialDelay: 15 * time.Second,
		RunOnStart:   true,
	}, a.dispatcher.ReportBacklog, a.logger).Run)
}

// asService attaches a service principal for scheduled work, which has no request
// context and therefore no end user to authorise.
func asService(ctx context.Context) context.Context {
	ctx = authn.WithPrincipal(ctx, authn.Principal{
		UserID: "wallet-service-scheduler",
		Role:   authn.RoleService,
	})
	return ctx
}

// --- Degraded-mode stand-ins ----------------------------------------------

// noopPublisher stands in when Kafka is disabled. Events still accumulate in the
// outbox, so nothing is lost — they publish once a broker appears.
type noopPublisher struct{ logger *slog.Logger }

func (p noopPublisher) Publish(_ context.Context, topic, _ string, _ []byte, _ map[string]string) error {
	return errs.Unavailable("kafka is disabled; %s remains queued in the outbox", topic)
}

// alwaysAllowLimiter stands in when Redis is disabled.
type alwaysAllowLimiter struct{}

func (alwaysAllowLimiter) CheckAndRecordFailure(context.Context, string) (port.AbuseVerdict, error) {
	return port.AbuseVerdict{}, nil
}

func (alwaysAllowLimiter) Check(context.Context, string) (port.AbuseVerdict, error) {
	return port.AbuseVerdict{}, nil
}

func (alwaysAllowLimiter) Reset(context.Context, string) error { return nil }

// unavailableGateway stands in when no Payment Adapter is configured. A top-up fails
// with a clear, retryable error instead of a nil-pointer panic.
type unavailableGateway struct{}

func (unavailableGateway) InitiatePayment(context.Context, port.PaymentRequest) (port.PaymentIntent, error) {
	return port.PaymentIntent{}, errs.Unavailable("bank top-ups are not configured in this deployment")
}

func (unavailableGateway) GetPaymentIntent(context.Context, string) (port.PaymentIntent, error) {
	return port.PaymentIntent{}, errs.Unavailable("bank top-ups are not configured in this deployment")
}
