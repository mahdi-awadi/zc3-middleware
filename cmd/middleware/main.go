// Command middleware is the ZKTeco C3 access-control middleware: a REST + SSE
// HTTP service that fronts a C3 panel, streams realtime access events, and
// manages cards and doors. All configuration comes from the environment / .env.
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

	"github.com/awadi/zk-c3-middleware/internal/access"
	"github.com/awadi/zk-c3-middleware/internal/api"
	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/config"
	"github.com/awadi/zk-c3-middleware/internal/store"
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
	log.Info("starting middleware", "version", version, "panel", cfg.PanelAddr, "http", cfg.HTTPAddr)

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
		Logger:          log,
	})

	// Durable audit log of every access event.
	audit, err := store.Open(cfg.AuditLogPath, cfg.AuditRingSize)
	if err != nil {
		return err
	}
	defer audit.Close()
	srv.SetAudit(audit)

	// Optional middleware-driven access: swipe at a reader opens the mapped door.
	allowed := map[string]bool{}
	allowAny := false
	for _, c := range cfg.AutoOpenCards {
		if c == "*" {
			allowAny = true
		} else {
			allowed[c] = true
		}
	}
	reactor := access.New(panel, access.Policy{
		Enabled:    cfg.AutoOpenEnabled,
		Seconds:    cfg.AutoOpenSeconds,
		AllowAny:   allowAny,
		Allowed:    allowed,
		ReaderDoor: cfg.ReaderDoorMap,
		Timeout:    cfg.IOTimeout,
	}, log)
	go reactor.Run(ctx)
	if reactor.Enabled() {
		log.Info("auto-open enabled", "cards", cfg.AutoOpenCards, "reader_door_map", cfg.ReaderDoorMap, "seconds", cfg.AutoOpenSeconds)
	}

	// Every panel event fans out to SSE subscribers and the access reactor.
	sink := func(ev c3.Event) {
		srv.Fanout(ev)
		reactor.Handle(ev)
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
