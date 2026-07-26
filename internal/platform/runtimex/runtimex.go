// Package runtimex manages the process lifecycle: starting long-running
// components, coordinating graceful shutdown, and running periodic jobs.
//
// A wallet service is not just an HTTP handler. It is an HTTP server, a gRPC
// server, one or more Kafka consumers, an outbox dispatcher and a handful of
// schedulers, all of which must start together and stop in an orderly way — Kafka
// consumers first, so that no new work arrives while the last requests drain.
package runtimex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Component is a long-running part of the service.
type Component struct {
	// Name appears in lifecycle logs.
	Name string
	// Run blocks until ctx is canceled. Returning a non-nil error brings the whole
	// service down, which is correct for a server that cannot bind its port.
	Run func(ctx context.Context) error
}

// Group runs components together and shuts them all down when any one exits or a
// termination signal arrives.
type Group struct {
	components []Component
	logger     *slog.Logger
	// onShutdown hooks run after every component has stopped, in reverse order of
	// registration. Connection pools and the telemetry flush belong here.
	onShutdown []Hook
	// shutdownGrace is how long components get to stop before the process exits
	// anyway. It must be shorter than the pod's terminationGracePeriodSeconds.
	shutdownGrace time.Duration
	// preStop runs the moment a signal is received, before components are canceled.
	// Flipping readiness to false here is what lets the load balancer drain this
	// instance before it stops accepting work.
	preStop []Hook
}

// Hook is a named shutdown or pre-stop action.
type Hook struct {
	Name string
	Fn   func(ctx context.Context) error
}

// NewGroup returns an empty Group.
func NewGroup(logger *slog.Logger, shutdownGrace time.Duration) *Group {
	if logger == nil {
		logger = slog.Default()
	}
	if shutdownGrace <= 0 {
		shutdownGrace = 30 * time.Second
	}
	return &Group{logger: logger, shutdownGrace: shutdownGrace}
}

// Add registers a component.
func (g *Group) Add(name string, run func(ctx context.Context) error) *Group {
	g.components = append(g.components, Component{Name: name, Run: run})
	return g
}

// OnShutdown registers a cleanup hook.
func (g *Group) OnShutdown(name string, fn func(ctx context.Context) error) *Group {
	g.onShutdown = append(g.onShutdown, Hook{Name: name, Fn: fn})
	return g
}

// PreStop registers an action that runs as soon as a signal arrives.
func (g *Group) PreStop(name string, fn func(ctx context.Context) error) *Group {
	g.preStop = append(g.preStop, Hook{Name: name, Fn: fn})
	return g
}

// Run starts everything and blocks until shutdown completes.
func (g *Group) Run(ctx context.Context) error {
	if len(g.components) == 0 {
		return errors.New("runtimex: no components registered")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	errCh := make(chan error, len(g.components))
	var wg sync.WaitGroup

	for _, component := range g.components {
		wg.Add(1)
		go func(component Component) {
			defer wg.Done()
			g.logger.Info("component starting", slog.String("component", component.Name))
			if err := component.Run(ctx); err != nil {
				errCh <- fmt.Errorf("component %s: %w", component.Name, err)
				return
			}
			g.logger.Info("component stopped", slog.String("component", component.Name))
		}(component)
	}

	var runErr error
	select {
	case sig := <-signals:
		g.logger.Info("received termination signal", slog.String("signal", sig.String()))
		g.runPreStop()
	case err := <-errCh:
		runErr = err
		g.logger.Error("component failed; shutting the service down", slog.String("error", err.Error()))
	case <-ctx.Done():
		g.logger.Info("context canceled; shutting the service down")
	}

	// Tell every component to stop and give them a bounded window to comply.
	cancel()

	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()

	select {
	case <-stopped:
		g.logger.Info("all components stopped")
	case <-time.After(g.shutdownGrace):
		g.logger.Warn("components did not stop within the grace period",
			slog.Duration("grace", g.shutdownGrace))
	}

	// Drain any component error that arrived during shutdown so it is not lost.
	select {
	case err := <-errCh:
		if runErr == nil {
			runErr = err
		}
	default:
	}

	if err := g.runShutdownHooks(); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

func (g *Group) runPreStop() {
	for _, hook := range g.preStop {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hook.Fn(ctx); err != nil {
			g.logger.Warn("pre-stop hook failed",
				slog.String("hook", hook.Name),
				slog.String("error", err.Error()),
			)
		}
		cancel()
	}
	// Give load balancers a moment to notice the readiness change before the
	// listeners go away. Without this pause a rolling update drops in-flight
	// requests that were routed just before the endpoint was withdrawn.
	if len(g.preStop) > 0 {
		time.Sleep(2 * time.Second)
	}
}

func (g *Group) runShutdownHooks() error {
	var failures []error
	for i := len(g.onShutdown) - 1; i >= 0; i-- {
		hook := g.onShutdown[i]
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		g.logger.Info("running shutdown hook", slog.String("hook", hook.Name))
		if err := hook.Fn(ctx); err != nil {
			g.logger.Error("shutdown hook failed",
				slog.String("hook", hook.Name),
				slog.String("error", err.Error()),
			)
			failures = append(failures, fmt.Errorf("shutdown hook %s: %w", hook.Name, err))
		}
		cancel()
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

// Scheduler runs a function on a fixed interval.
type Scheduler struct {
	name     string
	interval time.Duration
	// initialDelay staggers jobs so that several schedulers do not all fire on the
	// same tick right after boot.
	initialDelay time.Duration
	fn           func(ctx context.Context) error
	logger       *slog.Logger
	// runOnStart executes once immediately, before the first interval elapses.
	runOnStart bool
}

// SchedulerConfig configures a Scheduler.
type SchedulerConfig struct {
	Name         string
	Interval     time.Duration
	InitialDelay time.Duration
	RunOnStart   bool
}

// NewScheduler builds a Scheduler.
func NewScheduler(cfg SchedulerConfig, fn func(ctx context.Context) error, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	return &Scheduler{
		name:         cfg.Name,
		interval:     cfg.Interval,
		initialDelay: cfg.InitialDelay,
		fn:           fn,
		logger:       logger.With(slog.String("scheduler", cfg.Name)),
		runOnStart:   cfg.RunOnStart,
	}
}

// Run ticks until ctx is canceled. A failing job is logged and retried on the
// next tick rather than stopping the scheduler: a transient database error must
// not permanently disable interest accrual.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.initialDelay > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.initialDelay):
		}
	}

	s.logger.Info("scheduler started", slog.Duration("interval", s.interval))

	if s.runOnStart {
		s.tick(ctx)
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler stopped")
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	start := time.Now()
	if err := s.fn(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.logger.Error("scheduled job failed",
			slog.String("error", err.Error()),
			slog.Duration("duration", time.Since(start)),
		)
		return
	}
	s.logger.Debug("scheduled job completed", slog.Duration("duration", time.Since(start)))
}
