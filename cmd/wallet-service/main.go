// Command wallet-service runs the Arcadia wallet and payments service.
//
// The binary is deliberately thin: load configuration, build the application, run it,
// exit with a non-zero status if anything failed. Every decision worth reviewing lives
// in internal/bootstrap, which is the one place that knows about concrete
// infrastructure.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/bootstrap"
	"github.com/MS-Arcadia/wallet-service/internal/config"
)

// version is stamped at build time with -ldflags "-X main.version=...". The value
// appears in every log line and on the health endpoint, which is what makes an
// incident report unambiguous about which build was running.
var version = "dev"

func main() {
	// `wallet-service healthcheck` probes the process's own readiness endpoint and
	// exits 0 or 1. It exists because the image is distroless: there is no shell, no
	// curl and no wget for a Docker HEALTHCHECK to call, so the binary is the only
	// thing in the image that can make an HTTP request.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// Written to stderr rather than through the logger: a configuration failure
		// happens before the logger exists.
		fmt.Fprintf(os.Stderr, "wallet-service failed to start: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Service.Version == "dev" && version != "dev" {
		cfg.Service.Version = version
	}

	// The context is cancelled by the runtime group on SIGINT or SIGTERM; the group
	// owns signal handling so that shutdown ordering lives in one place.
	ctx := context.Background()

	app, err := bootstrap.New(ctx, cfg)
	if err != nil {
		return err
	}
	return app.Run(ctx)
}

// probe reports whether the service is ready to serve traffic.
//
// It reads HTTP_ADDR straight from the environment instead of calling config.Load,
// because a health check must never fail for a reason unrelated to health — loading the
// full configuration requires secrets that are none of a probe's business.
//
// Readiness rather than liveness: a container that cannot reach its database should be
// reported unhealthy, and DEGRADED (Redis down, say) deliberately still passes.
func probe() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// ":8080" means "every interface"; a client has to name one.
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/readyz returned %s", resp.Status)
	}
	return nil
}
