// Command dumpster runs the multi-protocol request dumpster.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thewalkingtoast/dumpster/internal/app"
)

// version is stamped at build time (-X main.version=...) and logged at startup,
// so a deployed container can be told apart from the build you assume is running.
var version = "dev"

func main() {
	cfg := app.DefaultConfig().FromEnv()

	flag.StringVar(&cfg.HTTPAddr, "http", cfg.HTTPAddr, "http listen address (websocket and sse share it)")
	flag.StringVar(&cfg.GRPCAddr, "grpc", cfg.GRPCAddr, "grpc listen address")
	flag.StringVar(&cfg.TCPAddr, "tcp", cfg.TCPAddr, "raw tcp listen address")
	flag.StringVar(&cfg.UDPAddr, "udp", cfg.UDPAddr, "raw udp listen address")
	flag.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "external origin for links, e.g. https://dumpster.example")
	flag.StringVar(&cfg.PublicGRPC, "public-grpc", cfg.PublicGRPC, "address shown to users for grpc")
	flag.StringVar(&cfg.PublicTCP, "public-tcp", cfg.PublicTCP, "address shown to users for tcp")
	flag.StringVar(&cfg.PublicUDP, "public-udp", cfg.PublicUDP, "address shown to users for udp")
	flag.BoolVar(&cfg.TLS, "tls", cfg.TLS, "this app is reachable over TLS: grpc requires a certificate and the page advertises wss")
	flag.StringVar(&cfg.GRPCTLSCert, "grpc-tls-cert", cfg.GRPCTLSCert, "PEM certificate for the grpc listener (needs -grpc-tls-key)")
	flag.StringVar(&cfg.GRPCTLSKey, "grpc-tls-key", cfg.GRPCTLSKey, "PEM key for the grpc listener (needs -grpc-tls-cert)")
	flag.DurationVar(&cfg.TTL, "ttl", cfg.TTL, "how long a dumpster lives")
	flag.IntVar(&cfg.MaxBody, "max-body", cfg.MaxBody, "bytes of body kept per request")
	flag.IntVar(&cfg.MaxDumpsters, "max", cfg.MaxDumpsters, "global cap on live dumpsters (0 = none)")
	flag.IntVar(&cfg.MaxPerIP, "max-per-ip", cfg.MaxPerIP, "cap on live dumpsters per address (0 = none)")
	flag.IntVar(&cfg.IngestPerMinute, "ingest-per-minute", cfg.IngestPerMinute, "ingested items per address per minute (0 = none)")
	flag.IntVar(&cfg.MaxConnsPerIP, "max-conns-per-ip", cfg.MaxConnsPerIP, "concurrent tcp/websocket/sse sessions per address (0 = none)")
	flag.IntVar(&cfg.MaxBytesPerDumpster, "max-bytes-per-dumpster", cfg.MaxBytesPerDumpster, "retained bytes per dumpster (0 = none)")
	flag.IntVar(&cfg.MaxBytes, "max-bytes", cfg.MaxBytes, "retained bytes across all dumpsters (0 = none)")
	flag.BoolVar(&cfg.TrustProxy, "trust-proxy", cfg.TrustProxy, "trust the first X-Forwarded-For hop (only if your proxy overwrites it)")
	flag.BoolVar(&cfg.TrustRealIP, "trust-real-ip", cfg.TrustRealIP, "trust X-Real-IP for the reported client ip (what nginx writes)")
	flag.StringVar(&cfg.RedisURL, "redis", cfg.RedisURL, "redis url; empty uses in-memory storage")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	a := app.New(cfg)
	if err := a.Start(); err != nil {
		slog.Error("start failed", "err", err)
		os.Exit(1)
	}

	addrs := a.Addrs()
	slog.Info("dumpster up",
		"version", version,
		"http", addrs.HTTP,
		"grpc", addrs.GRPC,
		"tcp", addrs.TCP,
		"udp", addrs.UDP,
		"ttl", cfg.TTL.String(),
		"base", a.BaseURL(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("draining")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
		os.Exit(1)
	}
}
