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

	"google.golang.org/grpc"

	"github.com/adam/dumpster/internal/hub"
	"github.com/adam/dumpster/internal/ratelimit"
	"github.com/adam/dumpster/internal/redisstore"
	"github.com/adam/dumpster/internal/store"
)

// Addrs are the addresses the listeners actually bound to.
type Addrs struct {
	HTTP string
	GRPC string
	TCP  string
	UDP  string
}

// App owns every listener and the state behind them.
type App struct {
	cfg   Config
	store store.Backend
	hub   *hub.Hub
	rl    *ratelimit.Limiter
	log   *slog.Logger

	httpSrv *http.Server
	httpLis net.Listener
	grpcSrv *grpc.Server
	grpcLis net.Listener
	tcpLis  net.Listener
	udpConn net.PacketConn

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
		cfg:    cfg,
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
	db, err := a.openStore()
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	a.store = db

	hl, err := net.Listen("tcp", a.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	a.httpLis = hl

	gl, err := net.Listen("tcp", a.cfg.GRPCAddr)
	if err != nil {
		_ = hl.Close()
		return fmt.Errorf("listen grpc: %w", err)
	}
	a.grpcLis = gl
	a.grpcSrv = a.newGRPCServer()

	tl, err := net.Listen("tcp", a.cfg.TCPAddr)
	if err != nil {
		_ = hl.Close()
		_ = gl.Close()
		return fmt.Errorf("listen tcp: %w", err)
	}
	a.tcpLis = tl

	ul, err := net.ListenPacket("udp", a.cfg.UDPAddr)
	if err != nil {
		_ = hl.Close()
		_ = gl.Close()
		_ = tl.Close()
		return fmt.Errorf("listen udp: %w", err)
	}
	a.udpConn = ul

	base := strings.TrimRight(a.cfg.BaseURL, "/")
	if base == "" {
		base = "http://" + hl.Addr().String()
	}
	a.mu.Lock()
	a.addrs = Addrs{
		HTTP: hl.Addr().String(),
		GRPC: gl.Addr().String(),
		TCP:  tl.Addr().String(),
		UDP:  ul.LocalAddr().String(),
	}
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
	go func() { _ = a.grpcSrv.Serve(gl) }()
	go a.acceptTCP(tl)
	go a.readUDP(ul)
	go a.reapLoop()
	return nil
}

// Shutdown stops accepting, drains the HTTP server, and ends the reaper.
func (a *App) Shutdown(ctx context.Context) error {
	a.cancel()
	if a.store != nil {
		_ = a.store.Shutdown()
	}
	if a.grpcSrv != nil {
		a.grpcSrv.GracefulStop()
	}
	if a.tcpLis != nil {
		_ = a.tcpLis.Close()
	}
	if a.udpConn != nil {
		_ = a.udpConn.Close()
	}
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

// openStore picks the backend. Redis is opt-in, so the default build needs no
// external service at all.
func (a *App) openStore() (store.Backend, error) {
	if a.cfg.RedisURL == "" {
		return store.New(store.Limits{
			MaxRecords:   a.cfg.MaxRecords,
			MaxEvents:    a.cfg.MaxEvents,
			MaxDumpsters: a.cfg.MaxDumpsters,
		}, a.cfg.TTL), nil
	}
	return redisstore.New(a.ctx, redisstore.Options{
		URL:          a.cfg.RedisURL,
		TTL:          a.cfg.TTL,
		MaxRecords:   a.cfg.MaxRecords,
		MaxEvents:    a.cfg.MaxEvents,
		MaxDumpsters: a.cfg.MaxDumpsters,
	})
}
