package app

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything tunable. Every field has a sane default; environment
// variables and flags override it.
type Config struct {
	HTTPAddr string
	GRPCAddr string
	TCPAddr  string
	UDPAddr  string

	BaseURL string // external origin for links, e.g. https://dumpster.example

	// RedisURL, when set, makes the app use Redis instead of memory. Both
	// backends implement store.Backend, so nothing else changes.
	RedisURL string

	// InstanceID identifies this process in the pub/sub relay, so it can ignore
	// its own messages. Generated per process when empty.
	InstanceID string

	PublicGRPC string // what a human should type; defaults to BaseURL host + port
	PublicTCP  string
	PublicUDP  string

	// TLS is the single statement of intent: this app is reachable over TLS
	// (https/wss). It decides whether gRPC requires a certificate and whether the
	// page advertises wss://, so the two can never disagree.
	TLS bool

	// Material for the gRPC listener. Only meaningful when TLS is on.
	GRPCTLSCert string
	GRPCTLSKey  string

	TTL       time.Duration
	ReapEvery time.Duration

	MaxBody      int
	MaxRecords   int
	MaxEvents    int
	MaxDumpsters int // global ceiling; 0 = unlimited
	MaxPerIP     int // per-caller ceiling; 0 = unlimited

	// Byte ceilings for the in-memory backend. Counts multiply, so these are the
	// only knobs that actually bound memory.
	MaxBytesPerDumpster int
	MaxBytes            int
	RentPerHour         int
	ConnIdle            time.Duration
	TrustProxy          bool // trust the first X-Forwarded-For hop: only safe if the proxy overwrites it
	TrustRealIP         bool // trust X-Real-IP: the header nginx writes from the socket

	// Abuse ceilings. Renting was always limited; consuming was not, and the raw
	// ports have no other protection in front of them.
	IngestPerMinute int
	MaxConnsPerIP   int
}

// DefaultConfig is the configuration the plan's expected outputs assume.
func DefaultConfig() Config {
	return Config{
		HTTPAddr: ":8080",
		GRPCAddr: ":8081",
		TCPAddr:  ":8082",
		UDPAddr:  ":8083",

		TTL:       3 * time.Hour,
		ReapEvery: time.Minute,

		MaxBody:      1 << 20,
		MaxRecords:   500,
		MaxEvents:    500,
		MaxDumpsters: 50000, // whole-board ceiling; protects the map, not the bytes
		MaxPerIP:     500,   // each address may hold 500 live dumpsters

		MaxBytesPerDumpster: 8 << 20, // 8 MiB of records per dumpster
		MaxBytes:            4 << 30, // 4 GiB across the whole board

		IngestPerMinute: 600, // ~10/s sustained per address
		MaxConnsPerIP:   64,  // concurrent tcp/websocket/sse sessions
		RentPerHour:     10,
		ConnIdle:        90 * time.Second,
	}
}

// FromEnv overlays DUMPSTER_* variables. Invalid values are ignored, so a typo
// can never stop the service from starting.
func (c Config) FromEnv() Config {
	if v := os.Getenv("DUMPSTER_HTTP_ADDR"); v != "" {
		c.HTTPAddr = v
	}
	if v := os.Getenv("DUMPSTER_GRPC_ADDR"); v != "" {
		c.GRPCAddr = v
	}
	if v := os.Getenv("DUMPSTER_TCP_ADDR"); v != "" {
		c.TCPAddr = v
	}
	if v := os.Getenv("DUMPSTER_UDP_ADDR"); v != "" {
		c.UDPAddr = v
	}
	if v := os.Getenv("DUMPSTER_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("DUMPSTER_REDIS_URL"); v != "" {
		c.RedisURL = v
	}
	if v := os.Getenv("DUMPSTER_INSTANCE_ID"); v != "" {
		c.InstanceID = v
	}
	if v := os.Getenv("DUMPSTER_PUBLIC_GRPC"); v != "" {
		c.PublicGRPC = v
	}
	if v := os.Getenv("DUMPSTER_PUBLIC_TCP"); v != "" {
		c.PublicTCP = v
	}
	if v := os.Getenv("DUMPSTER_TLS"); v != "" {
		c.TLS = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("DUMPSTER_GRPC_TLS_CERT"); v != "" {
		c.GRPCTLSCert = v
	}
	if v := os.Getenv("DUMPSTER_GRPC_TLS_KEY"); v != "" {
		c.GRPCTLSKey = v
	}
	if v := os.Getenv("DUMPSTER_PUBLIC_UDP"); v != "" {
		c.PublicUDP = v
	}

	c.TTL = durationEnv("DUMPSTER_TTL", c.TTL)
	c.ReapEvery = durationEnv("DUMPSTER_REAP_EVERY", c.ReapEvery)
	c.ConnIdle = durationEnv("DUMPSTER_CONN_IDLE", c.ConnIdle)

	if v, ok := intEnv("DUMPSTER_MAX_BODY"); ok {
		c.MaxBody = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_RECORDS"); ok {
		c.MaxRecords = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_EVENTS"); ok {
		c.MaxEvents = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_DUMPSTERS"); ok {
		c.MaxDumpsters = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_PER_IP"); ok {
		c.MaxPerIP = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_BYTES_PER_DUMPSTER"); ok {
		c.MaxBytesPerDumpster = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_BYTES"); ok {
		c.MaxBytes = v
	}
	if v, ok := intEnv("DUMPSTER_INGEST_PER_MINUTE"); ok {
		c.IngestPerMinute = v
	}
	if v, ok := intEnv("DUMPSTER_MAX_CONNS_PER_IP"); ok {
		c.MaxConnsPerIP = v
	}
	if v, ok := intEnv("DUMPSTER_RENT_PER_HOUR"); ok {
		c.RentPerHour = v
	}
	if v := os.Getenv("DUMPSTER_TRUST_PROXY"); v != "" {
		c.TrustProxy = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("DUMPSTER_TRUST_REAL_IP"); v != "" {
		c.TrustRealIP = v == "1" || strings.EqualFold(v, "true")
	}
	return c
}

func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func intEnv(key string) (int, bool) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	return 0, false
}
