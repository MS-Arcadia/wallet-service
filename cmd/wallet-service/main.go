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
	"os"

	"github.com/MS-Arcadia/wallet-service/internal/bootstrap"
	"github.com/MS-Arcadia/wallet-service/internal/config"
)

// version is stamped at build time with -ldflags "-X main.version=...". The value
// appears in every log line and on the health endpoint, which is what makes an
// incident report unambiguous about which build was running.
var version = "dev"

func main() {
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
