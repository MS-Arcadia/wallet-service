package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ServerConfig configures the HTTP server.
type ServerConfig struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// ReadTimeout, WriteTimeout and IdleTimeout protect against slow clients.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	// ReadHeaderTimeout specifically defends against Slowloris.
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout bounds graceful drain.
	ShutdownTimeout time.Duration
}

func (c ServerConfig) withDefaults() ServerConfig {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 15 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 120 * time.Second
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = 10 * time.Second
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 20 * time.Second
	}
	return c
}

// Server runs an HTTP listener with graceful shutdown.
type Server struct {
	server *http.Server
	cfg    ServerConfig
	logger *slog.Logger
}

// NewServer builds a Server around handler.
func NewServer(cfg ServerConfig, handler http.Handler, logger *slog.Logger) *Server {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		server: &http.Server{
			Addr:              cfg.Addr,
			Handler:           handler,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		},
		cfg:    cfg,
		logger: logger.With(slog.String("component", "http-server")),
	}
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.cfg.Addr }

// Run serves until ctx is canceled, then drains in-flight requests.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("httpx: listen on %s: %w", s.cfg.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("http server listening", slog.String("addr", listener.Addr().String()))
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		return s.shutdown()
	}
}

func (s *Server) shutdown() error {
	s.logger.Info("http server shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))

	// A detached context: the parent is already canceled, and Shutdown needs time
	// to let in-flight requests finish.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	if err := s.server.Shutdown(ctx); err != nil {
		// Force-close whatever is left rather than leaking connections on exit.
		_ = s.server.Close()
		return fmt.Errorf("httpx: graceful shutdown timed out: %w", err)
	}
	s.logger.Info("http server stopped cleanly")
	return nil
}
