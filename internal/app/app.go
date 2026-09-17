// Package app wires the gym system together — panel client, connection
// supervisor, gate engine, HTTP API + console — so that every entrypoint
// (headless server, Windows tray app) runs the exact same thing.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"

	"github.com/awadi/zk-c3-middleware/internal/api"
	"github.com/awadi/zk-c3-middleware/internal/auth"
	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/config"
	"github.com/awadi/zk-c3-middleware/internal/gym"
	"github.com/awadi/zk-c3-middleware/internal/supervisor"
)

type App struct {
	cfg     config.Config
	version string
	log     *slog.Logger

	store  *gym.Store
	panel  *c3.Client
	engine *gym.Engine
	srv    *api.Server
	sup    *supervisor.Supervisor
}

// New builds the system from cfg. Nothing is dialed or listened on yet.
func New(cfg config.Config, version string, log *slog.Logger) (*App, error) {
	store, err := gym.Open(cfg.GymDataPath)
	if err != nil {
		return nil, err
	}
	// The active gate (chosen from the console) decides which panel to dial;
	// PANEL_ADDR is the fallback for first boot.
	panelAddr := cfg.PanelAddr
	if ag := store.ActiveGate(); ag != nil {
		panelAddr = ag.Addr
	}
	log.Info("starting gym system", "version", version, "panel", panelAddr, "http", cfg.HTTPAddr, "data", cfg.GymDataPath)
	if panelAddr == "" {
		log.Warn("no panel address yet — add a gate from the console (البوابات) or set PANEL_ADDR")
	}

	panel := c3.New(panelAddr,
		c3.WithTimeout(cfg.IOTimeout),
		c3.WithPollInterval(cfg.EventPollInterval),
		c3.WithReplayBacklog(cfg.EventReplayBacklog),
		c3.WithAllowWrites(cfg.AllowWrites),
	)
	// Activation and the administrator account live next to the gym data.
	authDir := "."
	if cfg.GymDataPath != "" {
		authDir = filepath.Dir(cfg.GymDataPath)
	}
	guard, err := auth.Open(authDir)
	if err != nil {
		return nil, err
	}
	if !guard.Activated() {
		log.Warn("this copy is not activated yet — open the console to activate it")
	}
	srv := api.New(panel, api.Options{
		Auth:            guard,
		APIKey:          cfg.APIKey,
		CORSOrigins:     cfg.CORSOrigins,
		EventBufferSize: cfg.EventBufferSize,
		DoorDefaultSecs: cfg.DoorDefaultSeconds,
		AllowWrites:     cfg.AllowWrites,
		PanelID:         cfg.PanelID,
		Version:         version,
		Logger:          log,
	})
	engine := gym.NewEngine(store, panel, cfg.DoorDefaultSeconds, cfg.IOTimeout, log)
	srv.SetGym(store, engine)
	// Gate switching from the console: repoint the client and drop the current
	// session; the supervisor's reconnect loop dials the new address.
	srv.SetGateSwitch(func(addr string) {
		log.Info("switching panel connection", "addr", addr)
		panel.SetAddr(addr)
		_ = panel.Close()
	})
	if store.Config().Enabled {
		log.Info("gym gate enforcement enabled")
	}

	sink := func(ev c3.Event) {
		srv.NoteEvent(ev)
		engine.Handle(ev)
	}
	sup := supervisor.New(panel, sink, supervisor.Config{
		ReconnectMin: cfg.ReconnectMin,
		ReconnectMax: cfg.ReconnectMax,
		Logger:       log,
	})
	srv.SetReadiness(sup.Connected)

	return &App{cfg: cfg, version: version, log: log, store: store, panel: panel, engine: engine, srv: srv, sup: sup}, nil
}

// PanelConnected reports whether the panel session is currently up.
func (a *App) PanelConnected() bool { return a.sup.Connected() }

// HTTPAddr is the configured listen address.
func (a *App) HTTPAddr() string { return a.cfg.HTTPAddr }

// Run serves until ctx is cancelled or the HTTP listener fails. The panel
// connection is supervised in the background for the whole lifetime.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ln, err := net.Listen("tcp", a.cfg.HTTPAddr)
	if err != nil {
		return err
	}
	go a.engine.Run(ctx)
	go a.sup.Run(ctx)

	httpSrv := &http.Server{
		Handler:           a.srv.Routes(),
		ReadHeaderTimeout: a.cfg.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() {
		a.log.Info("http listening", "addr", ln.Addr().String())
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		a.log.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutCtx, cancelShut := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancelShut()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		a.log.Warn("graceful shutdown timed out", "err", err)
	}
	_ = a.panel.Close()
	a.log.Info("stopped")
	return nil
}

// NewLogger builds the slog logger from LOG_LEVEL / LOG_FORMAT writing to w.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
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
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
