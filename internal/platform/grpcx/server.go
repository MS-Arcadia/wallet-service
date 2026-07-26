// Package grpcx builds the gRPC server and client used for the platform's
// primary synchronous transport.
//
// The interceptor chain mirrors the HTTP middleware chain exactly — recovery,
// correlation, tracing, auth, metrics, logging — so that a use case behaves
// identically whichever adapter invoked it.
package grpcx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Metadata keys used by the platform. gRPC lower-cases every key.
const (
	// MetaRequestID carries the request identifier.
	MetaRequestID = "x-request-id"
	// MetaCorrelationID ties calls into one business transaction.
	MetaCorrelationID = "x-correlation-id"
	// MetaIdempotencyKey deduplicates money-moving calls that do not carry the key
	// in their request message.
	MetaIdempotencyKey = "idempotency-key"
	// MetaAuthorization carries the bearer token.
	MetaAuthorization = "authorization"
)

// Metrics is the sink the interceptors report to.
type Metrics interface {
	ObserveRPC(transport, method, code string, duration time.Duration)
	IncInFlight(transport string)
	DecInFlight(transport string)
}

// ServerConfig configures the gRPC server.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":9090".
	Addr string
	// MaxRecvMsgSize and MaxSendMsgSize cap message sizes.
	MaxRecvMsgSize int
	MaxSendMsgSize int
	// HandlerTimeout bounds a single unary call.
	HandlerTimeout time.Duration
	// ShutdownTimeout bounds graceful drain.
	ShutdownTimeout time.Duration
	// EnableReflection exposes the service descriptors so that grpcurl and Postman
	// can call the API without a local copy of the protos. Convenient in
	// development; usually off in production.
	EnableReflection bool
	// ServiceName is used for span and log attribution.
	ServiceName string
}

func (c ServerConfig) withDefaults() ServerConfig {
	if c.Addr == "" {
		c.Addr = ":9090"
	}
	if c.MaxRecvMsgSize <= 0 {
		c.MaxRecvMsgSize = 4 << 20
	}
	if c.MaxSendMsgSize <= 0 {
		c.MaxSendMsgSize = 4 << 20
	}
	if c.HandlerTimeout <= 0 {
		c.HandlerTimeout = 30 * time.Second
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 20 * time.Second
	}
	if c.ServiceName == "" {
		c.ServiceName = "arcadia"
	}
	return c
}

// Server wraps a *grpc.Server with lifecycle management.
type Server struct {
	server *grpc.Server
	health *health.Server
	cfg    ServerConfig
	logger *slog.Logger
}

// NewServer builds a gRPC server with the platform's interceptor chain.
func NewServer(cfg ServerConfig, verifier *authn.Verifier, m Metrics, logger *slog.Logger) *Server {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	serverLogger := logger.With(slog.String("component", "grpc-server"))

	interceptors := []grpc.UnaryServerInterceptor{
		// Recovery is outermost so that it also catches a panic thrown by another
		// interceptor.
		RecoveryInterceptor(serverLogger),
		CorrelationInterceptor(),
		TimeoutInterceptor(cfg.HandlerTimeout),
		LoggingInterceptor(serverLogger),
	}
	if m != nil {
		interceptors = append(interceptors, MetricsInterceptor(m))
	}
	if verifier != nil {
		interceptors = append(interceptors, AuthInterceptor(verifier))
	}
	// Error translation is innermost: it converts the domain error a handler
	// returns into a gRPC status before anything else observes it.
	interceptors = append(interceptors, ErrorInterceptor())

	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(interceptors...),
		grpc.MaxRecvMsgSize(cfg.MaxRecvMsgSize),
		grpc.MaxSendMsgSize(cfg.MaxSendMsgSize),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              2 * time.Minute,
			Timeout:           20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// Reject clients that ping more aggressively than this, which would
			// otherwise be a cheap denial-of-service.
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	)

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	if cfg.EnableReflection {
		reflection.Register(server)
	}

	return &Server{server: server, health: healthServer, cfg: cfg, logger: serverLogger}
}

// Registrar exposes the raw *grpc.Server so that services can register their
// generated handlers.
func (s *Server) Registrar() grpc.ServiceRegistrar { return s.server }

// SetServing marks a service as SERVING in the gRPC health protocol. An empty
// name sets the overall server status.
func (s *Server) SetServing(service string, serving bool) {
	statusValue := healthpb.HealthCheckResponse_NOT_SERVING
	if serving {
		statusValue = healthpb.HealthCheckResponse_SERVING
	}
	s.health.SetServingStatus(service, statusValue)
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.cfg.Addr }

// Run serves until ctx is canceled, then drains gracefully.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("grpcx: listen on %s: %w", s.cfg.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("grpc server listening",
			slog.String("addr", listener.Addr().String()),
			slog.Bool("reflection", s.cfg.EnableReflection),
		)
		serveErr <- s.server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpcx: serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		return s.shutdown()
	}
}

func (s *Server) shutdown() error {
	s.logger.Info("grpc server shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))

	// Flip health to NOT_SERVING first so that clients using health-based load
	// balancing stop routing here before connections start closing.
	s.health.Shutdown()

	stopped := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		s.logger.Info("grpc server stopped cleanly")
		return nil
	case <-time.After(s.cfg.ShutdownTimeout):
		s.logger.Warn("grpc graceful shutdown timed out; forcing stop")
		s.server.Stop()
		return nil
	}
}

// --- Interceptors ---------------------------------------------------------

// RecoveryInterceptor converts a panic into an internal error.
func RecoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("panic while handling rpc",
					slog.String("method", info.FullMethod),
					slog.Any("panic", recovered),
				)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// CorrelationInterceptor propagates or creates the request and correlation ids.
func CorrelationInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		correlationID := metadataValue(ctx, MetaCorrelationID)
		if correlationID == "" {
			correlationID = metadataValue(ctx, MetaRequestID)
		}
		if correlationID == "" {
			correlationID = uuid.NewString()
		}
		return handler(logx.WithCorrelationID(ctx, correlationID), req)
	}
}

// TimeoutInterceptor applies a server-side deadline when the client did not set
// one, so that a forgetful client cannot pin a handler open indefinitely.
func TimeoutInterceptor(timeout time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			return handler(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return handler(ctx, req)
	}
}

// LoggingInterceptor emits one structured line per call.
func LoggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// Health checks arrive constantly and carry no information.
		if strings.HasPrefix(info.FullMethod, "/grpc.health") {
			return handler(ctx, req)
		}

		ctx = logx.WithLogger(ctx, logger)
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start)

		attrs := []any{
			slog.String("method", info.FullMethod),
			slog.String("code", status.Code(err).String()),
			slog.Int64("duration_ms", duration.Milliseconds()),
		}
		if principal, ok := authn.PrincipalFrom(ctx); ok {
			attrs = append(attrs,
				slog.String("user_id", principal.UserID),
				slog.String("role", principal.Role.String()),
			)
		}

		entry := logx.FromContext(ctx)
		switch code := status.Code(err); {
		case code == codes.OK:
			entry.Info("rpc handled", attrs...)
		case code == codes.Internal || code == codes.Unknown || code == codes.DataLoss:
			entry.Error("rpc failed", append(attrs, slog.String("error", err.Error()))...)
		default:
			entry.Warn("rpc rejected", append(attrs, slog.String("error", err.Error()))...)
		}
		return resp, err
	}
}

// MetricsInterceptor records RED metrics per method.
func MetricsInterceptor(m Metrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health") {
			return handler(ctx, req)
		}

		m.IncInFlight("grpc")
		defer m.DecInFlight("grpc")

		start := time.Now()
		resp, err := handler(ctx, req)
		m.ObserveRPC("grpc", info.FullMethod, status.Code(err).String(), time.Since(start))
		return resp, err
	}
}

// AuthInterceptor verifies a bearer token when one is present.
//
// As with the HTTP middleware, an anonymous call is allowed through with no
// principal: the use case decides what it requires. Health and reflection are
// skipped entirely.
func AuthInterceptor(verifier *authn.Verifier) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health") ||
			strings.HasPrefix(info.FullMethod, "/grpc.reflection") {
			return handler(ctx, req)
		}

		header := metadataValue(ctx, MetaAuthorization)
		if header == "" {
			return handler(ctx, req)
		}

		token, err := authn.BearerToken(header)
		if err != nil {
			return nil, errs.ToGRPC(err)
		}
		principal, err := verifier.Verify(token)
		if err != nil {
			return nil, errs.ToGRPC(err)
		}
		return handler(authn.WithPrincipal(ctx, principal), req)
	}
}

// ErrorInterceptor converts a domain error into a gRPC status.
func ErrorInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}
		// A handler that already produced a status is left alone.
		if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
			return resp, err
		}
		return resp, errs.ToGRPC(err)
	}
}

// --- Client ---------------------------------------------------------------

// ClientConfig configures an outbound connection.
type ClientConfig struct {
	// Target is the server address, e.g. "payment-service:9090".
	Target string
	// Timeout is the per-call deadline applied by the client interceptor.
	Timeout time.Duration
	// ServiceToken is a bearer token identifying this service to the callee.
	ServiceToken string
	// ServiceName is used for span attribution.
	ServiceName string
}

// Dial opens a client connection with tracing, correlation propagation and
// health-aware load balancing.
func Dial(cfg ClientConfig, logger *slog.Logger) (*grpc.ClientConn, error) {
	if cfg.Target == "" {
		return nil, errors.New("grpcx: target is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	conn, err := grpc.NewClient(cfg.Target,
		// TLS terminates at the mesh/ingress; inside the cluster traffic is plain
		// HTTP/2 over the pod network.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(
			clientCorrelationInterceptor(cfg.ServiceToken),
			clientTimeoutInterceptor(cfg.Timeout),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		// round_robin spreads calls across every ready endpoint instead of pinning
		// one connection to one pod, which is what makes a Deployment's replicas
		// actually share load.
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	)
	if err != nil {
		return nil, fmt.Errorf("grpcx: dial %s: %w", cfg.Target, err)
	}

	// grpc.NewClient is lazy — it does not connect until the first call — so this records
	// what the client is configured to reach rather than that it succeeded. That is
	// precisely the line you want when a service is calling the wrong address.
	logger.Info("grpc client configured",
		slog.String("component", "grpc-client"),
		slog.String("target", cfg.Target),
		slog.Duration("timeout", cfg.Timeout),
		slog.Bool("service_token", cfg.ServiceToken != ""),
	)
	return conn, nil
}

func clientCorrelationInterceptor(serviceToken string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		pairs := make([]string, 0, 4)
		if correlationID := logx.CorrelationID(ctx); correlationID != "" {
			pairs = append(pairs, MetaCorrelationID, correlationID)
		}
		if serviceToken != "" {
			pairs = append(pairs, MetaAuthorization, "Bearer "+serviceToken)
		}
		if len(pairs) > 0 {
			ctx = metadata.AppendToOutgoingContext(ctx, pairs...)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func clientTimeoutInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// --- Metadata helpers -----------------------------------------------------

func metadataValue(ctx context.Context, key string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// IdempotencyKeyFrom returns the Idempotency-Key metadata value, or "".
func IdempotencyKeyFrom(ctx context.Context) string {
	return strings.TrimSpace(metadataValue(ctx, MetaIdempotencyKey))
}
