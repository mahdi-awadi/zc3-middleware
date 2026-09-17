// Command middleware is the headless gym access system: it fronts a ZKTeco C3
// panel and runs the whole membership layer — members with time-boxed
// subscriptions bound to cards, a per-swipe gate decision engine, and the
// embedded Arabic console served at "/". All configuration comes from the
// environment / .env. (The Windows desktop build is cmd/gymgate.)
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/awadi/zk-c3-middleware/internal/app"
	"github.com/awadi/zk-c3-middleware/internal/config"
)

// version is injected at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := app.NewLogger(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.New(cfg, version, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}
