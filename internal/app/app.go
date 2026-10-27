// Package app wires listeners, storage and the web UI together.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adam/dumpster/internal/hub"
	"github.com/adam/dumpster/internal/ratelimit"
	"github.com/adam/dumpster/internal/store"
)

// Addrs are the addresses the listeners actually bound to.
type Addrs struct {
	HTTP string
}

// App owns every listener and the state behind them.
type App struct {
	cfg   Config
	store *store.Store
	hub   *hub.Hub
	rl    *ratelimit.Limiter
	log   *slog.Logger

	httpSrv *http.Server
	httpLis net.Listener

	mu      sync.Mutex
	addrs   Addrs
	baseURL string

	ctx    context.Context
	cancel context.CancelFunc
}

// New builds an App. Nothing is bound until Start.
func New(cfg Config) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		cfg: cfg,
		store: store.New(store.Limits{
			MaxRecords:   cfg.MaxRecords,
			MaxEvents:    cfg.MaxEvents,
			MaxDumpsters: cfg.MaxDumpsters,
		}, cfg.TTL),
		hub:    hub.New(128),
		rl:     ratelimit.New(cfg.RentPerHour, time.Hour),
		log:    slog.Default(),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start binds the HTTP listener and serves it. It returns once the port is
// bound, so callers can read Addrs immediately.
func (a *App) Start() error {
	hl, err := net.Listen("tcp", a.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	a.httpLis = hl

	base := strings.TrimRight(a.cfg.BaseURL, "/")
	if base == "" {
		base = "http://" + hl.Addr().String()
	}
	a.mu.Lock()
	a.addrs = Addrs{HTTP: hl.Addr().String()}
	a.baseURL = base
	a.mu.Unlock()

	a.httpSrv = &http.Server{
		Handler:           a.withHeaders(a.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       75 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ConnContext:       withConnAcceptedAt,
		ErrorLog:          log.New(io.Discard, "", 0),
	}

	go func() { _ = a.httpSrv.Serve(hl) }()
	go a.reapLoop()
	return nil
}

// Shutdown stops accepting, drains the HTTP server, and ends the reaper.
func (a *App) Shutdown(ctx context.Context) error {
	a.cancel()
	if a.httpSrv == nil {
		return nil
	}
	if err := a.httpSrv.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// Addrs returns the bound addresses.
func (a *App) Addrs() Addrs {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addrs
}

// BaseURL is the origin used for links and for the command hints.
func (a *App) BaseURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.baseURL
}

// Log exposes the app logger to handlers.
func (a *App) Log() *slog.Logger { return a.log }
