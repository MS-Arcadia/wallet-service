// Package config loads the service's configuration from the environment.
//
// Nothing here has a usable default for a secret. A missing JWT key or gift-card
// pepper stops the process at boot with a clear message, which is far better than a
// service that starts happily and accepts forged tokens.
package config

import (
	"fmt"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/config"
)

// ServerMode selects which inbound transports to serve.
type ServerMode string

const (
	// ModeGRPC serves gRPC only. The platform default.
	ModeGRPC ServerMode = "grpc"
	// ModeHTTP serves REST only.
	ModeHTTP ServerMode = "http"
	// ModeBoth serves both, which is how the Postman collection and the Next.js front
	// end reach the service while other services use gRPC.
	ModeBoth ServerMode = "both"
)

// ServesGRPC reports whether the gRPC listener should start.
func (m ServerMode) ServesGRPC() bool { return m == ModeGRPC || m == ModeBoth }

// ServesHTTP reports whether the REST listener should start.
func (m ServerMode) ServesHTTP() bool { return m == ModeHTTP || m == ModeBoth }

// Config is the complete service configuration.
type Config struct {
	Service  ServiceConfig
	Server   ServerConfig
	Database DatabaseConfig
	Redis    RedisConfig
	Kafka    KafkaConfig
	Auth     AuthConfig
	Wallet   WalletConfig
	Payment  PaymentConfig
	Jobs     JobsConfig
}

// ServiceConfig identifies the running instance.
type ServiceConfig struct {
	Name        string
	Version     string
	Environment string
	LogLevel    string
	LogFormat   string
}

// ServerConfig configures the inbound transports.
type ServerConfig struct {
	Mode ServerMode
	// HTTPAddr serves REST plus the probe and metrics endpoints. The operational
	// endpoints are always served, even in grpc-only mode, because Kubernetes and
	// Prometheus both need them.
	HTTPAddr        string
	GRPCAddr        string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	HandlerTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64
	CORSOrigins     []string
	// EnableReflection exposes gRPC service descriptors, so grpcurl and Postman can
	// call the API without a local copy of the protos. Handy in development.
	EnableReflection bool
}

// DatabaseConfig configures Postgres.
type DatabaseConfig struct {
	DSN              string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	ConnectTimeout   time.Duration
	StatementTimeout time.Duration
	RunMigrations    bool
}

// RedisConfig configures Redis, used for abuse detection and locking.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	PoolSize int
	// Enabled turns Redis off entirely. With it off the service still works; the
	// gift-card abuse rule simply is not enforced, which the boot log says out loud.
	Enabled bool
}

// KafkaConfig configures the event backbone.
type KafkaConfig struct {
	Brokers  []string
	ClientID string
	GroupID  string
	// Enabled turns the broker off, for running a single service locally against
	// nothing else.
	Enabled bool
	// Produced topics.
	WalletEventsTopic string
	AuditEventsTopic  string
	// Consumed topics.
	PaymentEventsTopic  string
	UserEventsTopic     string
	WalletCommandsTopic string
	TradeEventsTopic    string
	// EnsureTopics creates the topics this service owns at boot. Broker-side
	// auto-creation is off, so a typo fails loudly instead of silently creating a
	// topic nobody reads.
	EnsureTopics       bool
	TopicPartitions    int
	TopicReplication   int
	ConsumerMaxRetry   int
	StartFromOldest    bool
	OutboxPollInterval time.Duration
	OutboxBatchSize    int
}

// AuthConfig configures token verification.
type AuthConfig struct {
	Algorithm authn.Algorithm
	Secret    string
	PublicKey string
	Issuer    string
	Audience  string
	// ServiceToken identifies this service when it calls the Payment Adapter.
	ServiceToken string
}

// WalletConfig holds the domain settings.
type WalletConfig struct {
	Currency string
	// GiftCardPepper is the HMAC key for gift-card code hashing. Rotating it makes
	// every existing code unredeemable, so it is treated as permanent secret state.
	GiftCardPepper string
	// Abuse thresholds for gift-card redemption attempts.
	AbusePerMinute int64
	AbusePerHour   int64
	AbuseFlagAt    int64
	// Interest settings.
	InterestEnabled       bool
	InterestAnnualRateBps int64
	InterestMinBalance    int64
	// SchemaVersion stamps published events.
	SchemaVersion int
}

// PaymentConfig configures the outbound call to the Payment Adapter.
type PaymentConfig struct {
	// GRPCTarget is the adapter's address. Empty disables bank top-ups, which is
	// acceptable for a deployment that only settles internal money movement.
	GRPCTarget string
	Timeout    time.Duration
}

// JobsConfig configures the background schedulers.
type JobsConfig struct {
	// ReconcileInterval is how often balances are checked against the ledger.
	ReconcileInterval time.Duration
	// InterestInterval is how often accrual runs. Daily in production; the
	// per-wallet-per-date idempotency key makes a shorter interval harmless.
	InterestInterval time.Duration
	// HoldSweepInterval is how often lapsed holds are released.
	HoldSweepInterval time.Duration
	// OutboxMetricsInterval is how often the backlog gauge is refreshed.
	OutboxMetricsInterval time.Duration
	// PurgeInterval and retention windows for the bookkeeping tables.
	PurgeInterval        time.Duration
	IdempotencyRetention time.Duration
	OutboxRetention      time.Duration
}

// Load reads the configuration, reporting every problem at once.
func Load() (Config, error) {
	l := config.NewLoader()

	cfg := Config{
		Service: ServiceConfig{
			Name:        l.String("SERVICE_NAME", "wallet-service"),
			Version:     l.String("SERVICE_VERSION", "dev"),
			Environment: l.String("ENVIRONMENT", "local"),
			LogLevel:    l.String("LOG_LEVEL", "info"),
			LogFormat:   l.String("LOG_FORMAT", "json"),
		},

		Server: ServerConfig{
			Mode:             ServerMode(l.OneOf("SERVER_MODE", "grpc", "grpc", "http", "both")),
			HTTPAddr:         l.String("HTTP_ADDR", ":8080"),
			GRPCAddr:         l.String("GRPC_ADDR", ":9090"),
			ReadTimeout:      l.Duration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:     l.Duration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			HandlerTimeout:   l.Duration("HANDLER_TIMEOUT", 30*time.Second),
			ShutdownTimeout:  l.Duration("SHUTDOWN_TIMEOUT", 20*time.Second),
			MaxBodyBytes:     l.Int64("HTTP_MAX_BODY_BYTES", 1<<20),
			CORSOrigins:      l.Strings("CORS_ORIGINS", []string{"http://localhost:3000"}),
			EnableReflection: l.Bool("GRPC_REFLECTION", true),
		},

		Database: DatabaseConfig{
			DSN:              l.MustString("DATABASE_URL"),
			MaxConns:         int32(l.Int("DB_MAX_CONNS", 20)),
			MinConns:         int32(l.Int("DB_MIN_CONNS", 2)),
			MaxConnLifetime:  l.Duration("DB_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime:  l.Duration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute),
			ConnectTimeout:   l.Duration("DB_CONNECT_TIMEOUT", 10*time.Second),
			StatementTimeout: l.Duration("DB_STATEMENT_TIMEOUT", 15*time.Second),
			RunMigrations:    l.Bool("DB_RUN_MIGRATIONS", true),
		},

		Redis: RedisConfig{
			Enabled:  l.Bool("REDIS_ENABLED", true),
			Addr:     l.String("REDIS_ADDR", "localhost:6379"),
			Password: l.String("REDIS_PASSWORD", ""),
			DB:       l.Int("REDIS_DB", 0),
			PoolSize: l.Int("REDIS_POOL_SIZE", 20),
		},

		Kafka: KafkaConfig{
			Enabled:  l.Bool("KAFKA_ENABLED", true),
			Brokers:  l.Strings("KAFKA_BROKERS", []string{"localhost:9092"}),
			ClientID: l.String("KAFKA_CLIENT_ID", "wallet-service"),
			GroupID:  l.String("KAFKA_GROUP_ID", "wallet-service"),

			WalletEventsTopic: l.String("KAFKA_TOPIC_WALLET_EVENTS", "wallet-events"),
			AuditEventsTopic:  l.String("KAFKA_TOPIC_AUDIT_EVENTS", "audit-events"),

			PaymentEventsTopic:  l.String("KAFKA_TOPIC_PAYMENT_EVENTS", "payment-events"),
			UserEventsTopic:     l.String("KAFKA_TOPIC_USER_EVENTS", "user-events"),
			WalletCommandsTopic: l.String("KAFKA_TOPIC_WALLET_COMMANDS", "wallet-commands"),
			TradeEventsTopic:    l.String("KAFKA_TOPIC_TRADE_EVENTS", "trade-events"),

			EnsureTopics:     l.Bool("KAFKA_ENSURE_TOPICS", true),
			TopicPartitions:  l.Int("KAFKA_TOPIC_PARTITIONS", 3),
			TopicReplication: l.Int("KAFKA_TOPIC_REPLICATION", 1),
			ConsumerMaxRetry: l.Int("KAFKA_CONSUMER_MAX_RETRY", 3),
			// Financial consumers read a new group from the beginning: skipping history
			// would silently drop money movements that happened before this pod started.
			StartFromOldest:    l.Bool("KAFKA_START_FROM_OLDEST", true),
			OutboxPollInterval: l.Duration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
			OutboxBatchSize:    l.Int("OUTBOX_BATCH_SIZE", 100),
		},

		Auth: AuthConfig{
			Algorithm:    authn.Algorithm(l.OneOf("JWT_ALGORITHM", "HS256", "HS256", "RS256")),
			Secret:       l.String("JWT_SECRET", ""),
			PublicKey:    l.String("JWT_PUBLIC_KEY", ""),
			Issuer:       l.String("JWT_ISSUER", "arcadia-auth"),
			Audience:     l.String("JWT_AUDIENCE", "arcadia"),
			ServiceToken: l.String("SERVICE_TOKEN", ""),
		},

		Wallet: WalletConfig{
			Currency:       l.String("WALLET_CURRENCY", "IRR"),
			GiftCardPepper: l.MustString("GIFT_CARD_PEPPER"),

			AbusePerMinute: l.Int64("GIFTCARD_ABUSE_PER_MINUTE", 5),
			AbusePerHour:   l.Int64("GIFTCARD_ABUSE_PER_HOUR", 30),
			AbuseFlagAt:    l.Int64("GIFTCARD_ABUSE_FLAG_AT", 10),

			InterestEnabled:       l.Bool("INTEREST_ENABLED", true),
			InterestAnnualRateBps: l.Int64("INTEREST_ANNUAL_RATE_BPS", 500),
			InterestMinBalance:    l.Int64("INTEREST_MIN_BALANCE_MINOR", 100_000),

			SchemaVersion: l.Int("EVENT_SCHEMA_VERSION", 1),
		},

		Payment: PaymentConfig{
			GRPCTarget: l.String("PAYMENT_SERVICE_GRPC", "localhost:9091"),
			Timeout:    l.Duration("PAYMENT_SERVICE_TIMEOUT", 10*time.Second),
		},

		Jobs: JobsConfig{
			ReconcileInterval:     l.Duration("JOB_RECONCILE_INTERVAL", 15*time.Minute),
			InterestInterval:      l.Duration("JOB_INTEREST_INTERVAL", 24*time.Hour),
			HoldSweepInterval:     l.Duration("JOB_HOLD_SWEEP_INTERVAL", 5*time.Minute),
			OutboxMetricsInterval: l.Duration("JOB_OUTBOX_METRICS_INTERVAL", 30*time.Second),
			PurgeInterval:         l.Duration("JOB_PURGE_INTERVAL", 6*time.Hour),
			IdempotencyRetention:  l.Duration("IDEMPOTENCY_RETENTION", 30*24*time.Hour),
			OutboxRetention:       l.Duration("OUTBOX_RETENTION", 7*24*time.Hour),
		},
	}

	if err := l.Err(); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate checks the cross-field rules the loader cannot express.
func (c Config) validate() error {
	// The signing material must match the algorithm. Booting without it would mean
	// every request is rejected as unauthenticated, or worse, that a placeholder secret
	// is in use.
	switch c.Auth.Algorithm {
	case authn.AlgHS256:
		if len(c.Auth.Secret) < 32 {
			return fmt.Errorf("JWT_SECRET must be at least 32 bytes when JWT_ALGORITHM is HS256")
		}
	case authn.AlgRS256:
		if c.Auth.PublicKey == "" {
			return fmt.Errorf("JWT_PUBLIC_KEY is required when JWT_ALGORITHM is RS256")
		}
	}

	if len(c.Wallet.GiftCardPepper) < 32 {
		return fmt.Errorf("GIFT_CARD_PEPPER must be at least 32 bytes; a short pepper makes gift card hashes brute-forceable")
	}
	if c.Wallet.AbusePerHour < c.Wallet.AbusePerMinute {
		return fmt.Errorf("GIFTCARD_ABUSE_PER_HOUR (%d) cannot be lower than GIFTCARD_ABUSE_PER_MINUTE (%d)",
			c.Wallet.AbusePerHour, c.Wallet.AbusePerMinute)
	}
	if c.Wallet.InterestAnnualRateBps < 0 || c.Wallet.InterestAnnualRateBps > 10_000 {
		return fmt.Errorf("INTEREST_ANNUAL_RATE_BPS must be between 0 and 10000, got %d",
			c.Wallet.InterestAnnualRateBps)
	}
	if len(c.Wallet.Currency) != 3 {
		return fmt.Errorf("WALLET_CURRENCY must be a 3-letter ISO-4217 code, got %q", c.Wallet.Currency)
	}
	return nil
}
