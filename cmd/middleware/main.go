// Command middleware is the gym access system: it fronts a ZKTeco C3 panel
// and runs the whole membership layer — members with time-boxed subscriptions
// bound to cards, a per-swipe gate decision engine, and the embedded Arabic
// console + gate kiosk served at "/". All configuration comes from the
// environment / .env.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/awadi/zk-c3-middleware/internal/api"
	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/config"
	"github.com/awadi/zk-c3-middleware/internal/gym"
	"github.com/awadi/zk-c3-middleware/internal/supervisor"
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

	log := newLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)
	log.Info("starting gym system", "version", version, "panel", cfg.PanelAddr, "http", cfg.HTTPAddr)

	// Root context cancelled on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Panel client + connection supervisor.
	panel := c3.New(cfg.PanelAddr,
		c3.WithTimeout(cfg.IOTimeout),
		c3.WithPollInterval(cfg.EventPollInterval),
		c3.WithReplayBacklog(cfg.EventReplayBacklog),
		c3.WithAllowWrites(cfg.AllowWrites),
	)

	srv := api.New(panel, api.Options{
		APIKey:          cfg.APIKey,
		CORSOrigins:     cfg.CORSOrigins,
		EventBufferSize: cfg.EventBufferSize,
		DoorDefaultSecs: cfg.DoorDefaultSeconds,
		AllowWrites:     cfg.AllowWrites,
		PanelID:         cfg.PanelID,
		Version:         version,
		Logger:          log,
	})

	// Gym membership layer: members + subscriptions bound to cards; the gym
	// engine decides the gate on every swipe and feeds the kiosk screen.
	gymStore, err := gym.Open(cfg.GymDataPath)
	if err != nil {
		return err
	}
	gymEngine := gym.NewEngine(gymStore, panel, cfg.DoorDefaultSeconds, cfg.IOTimeout, log)
	go gymEngine.Run(ctx)
	srv.SetGym(gymStore, gymEngine)
	if gymStore.Config().Enabled {
		log.Info("gym gate enforcement enabled", "data", cfg.GymDataPath)
	}

	// Every panel event feeds the gate engine (and the event counter).
	sink := func(ev c3.Event) {
		srv.NoteEvent(ev)
		gymEngine.Handle(ev)
	}
	sup := supervisor.New(panel, sink, supervisor.Config{
		ReconnectMin: cfg.ReconnectMin,
		ReconnectMax: cfg.ReconnectMax,
		Logger:       log,
	})
	srv.SetReadiness(sup.Connected) // /readyz reflects panel link state

	go sup.Run(ctx)

	// HTTP server.
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Warn("graceful shutdown timed out", "err", err)
	}
	_ = panel.Close()
	log.Info("stopped")
	return nil
}

// newLogger builds the slog logger from LOG_LEVEL / LOG_FORMAT.
func newLogger(level, format string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
