// Package redisstore is the durable, multi-instance dumpster backend.
//
// Key layout, all under one prefix:
//
//	dumpster:d:<id>        hash   created_at / expires_at (unix nanos)
//	dumpster:ids           set    every live dumpster id
//	dumpster:seq:<id>      string per-dumpster sequence counter
//	dumpster:r:<id>        list   record sequence numbers, newest first
//	dumpster:rec:<id>:<n>  string the record as JSON
//
// Expiry is Redis's own: every key written for a dumpster gets EXPIREAT at the
// dumpster's absolute expiry, so nothing sweeps records in the hot path.
package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/thewalkingtoast/dumpster/internal/id"
	"github.com/thewalkingtoast/dumpster/internal/model"
	"github.com/thewalkingtoast/dumpster/internal/store"
)

// Options configures a Backend.
type Options struct {
	URL          string // redis://host:port/db
	Prefix       string // defaults to "dumpster:"
	TTL          time.Duration
	MaxRecords   int
	MaxEvents    int
	MaxDumpsters int
	MaxPerIP     int
}

// Backend speaks the same Backend contract as the memory store.
type Backend struct {
	rdb    *redis.Client
	prefix string
	ttl    time.Duration
	limits store.Limits
	now    func() time.Time
}

var _ store.Backend = (*Backend)(nil)

// New dials Redis and verifies it answers.
func New(ctx context.Context, opts Options) (*Backend, error) {
	if opts.Prefix == "" {
		opts.Prefix = "dumpster:"
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = 500
	}
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 500
	}
	clientOpts, err := redis.ParseURL(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(clientOpts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Backend{
		rdb:    client,
		prefix: opts.Prefix,
		ttl:    opts.TTL,
		limits: store.Limits{
			MaxRecords:   opts.MaxRecords,
			MaxEvents:    opts.MaxEvents,
			MaxDumpsters: opts.MaxDumpsters,
			MaxPerIP:     opts.MaxPerIP,
		},
		now: time.Now,
	}, nil
}

// Client exposes the underlying connection, for the pub/sub relay.
func (b *Backend) Client() *redis.Client { return b.rdb }

// Prefix reports the key prefix in use.
func (b *Backend) Prefix() string { return b.prefix }

func (b *Backend) key(parts ...string) string {
	return b.prefix + strings.Join(parts, ":")
}

// Create mints a dumpster and arms its expiry.
func (b *Backend) Create(owner string) (model.Dumpster, error) {
	ctx := context.Background()
	now := b.now().UTC()
	if b.limits.MaxDumpsters > 0 {
		n, err := b.rdb.SCard(ctx, b.key("ids")).Result()
		if err != nil {
			return model.Dumpster{}, err
		}
		if int(n) >= b.limits.MaxDumpsters {
			return model.Dumpster{}, store.ErrFull
		}
	}
	// Per-caller cap. The allowance is a sorted set scored by expiry, so the
	// count prunes itself: expired members are dropped before counting instead
	// of lingering forever the way a plain set would.
	ipKey := b.key("ip", owner)
	if b.limits.MaxPerIP > 0 {
		if err := b.rdb.ZRemRangeByScore(ctx, ipKey, "-inf", strconv.FormatInt(now.Unix(), 10)).Err(); err != nil {
			return model.Dumpster{}, err
		}
		n, err := b.rdb.ZCard(ctx, ipKey).Result()
		if err != nil {
			return model.Dumpster{}, err
		}
		if int(n) >= b.limits.MaxPerIP {
			return model.Dumpster{}, store.ErrFullPerIP
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		sid, err := id.New()
		if err != nil {
			return model.Dumpster{}, err
		}
		expires := now.Add(b.ttl)
		pipe := b.rdb.TxPipeline()
		pipe.HSet(ctx, b.key("d", sid),
			"created_at", now.UnixNano(),
			"expires_at", expires.UnixNano())
		pipe.PExpireAt(ctx, b.key("d", sid), expires)
		pipe.SAdd(ctx, b.key("ids"), sid)
		pipe.ZAdd(ctx, ipKey, redis.Z{Score: float64(expires.Unix()), Member: sid})
		pipe.ExpireAt(ctx, ipKey, expires)
		if _, err := pipe.Exec(ctx); err != nil {
			return model.Dumpster{}, err
		}
		return model.Dumpster{ID: sid, CreatedAt: now, ExpiresAt: expires}, nil
	}
	return model.Dumpster{}, errors.New("could not mint a unique dumpster id")
}

// Get returns the dumpster if it exists and has not expired.
func (b *Backend) Get(sid string) (model.Dumpster, bool) {
	vals, err := b.rdb.HGetAll(context.Background(), b.key("d", sid)).Result()
	if err != nil || len(vals) == 0 {
		return model.Dumpster{}, false
	}
	created, errC := strconv.ParseInt(vals["created_at"], 10, 64)
	expires, errE := strconv.ParseInt(vals["expires_at"], 10, 64)
	if errC != nil || errE != nil {
		return model.Dumpster{}, false
	}
	d := model.Dumpster{
		ID:        sid,
		CreatedAt: time.Unix(0, created),
		ExpiresAt: time.Unix(0, expires),
	}
	if !d.ExpiresAt.After(b.now()) {
		return model.Dumpster{}, false
	}
	return d, true
}

// Add assigns the next sequence number, stores the record, and trims the list.
func (b *Backend) Add(sid string, rec *model.Record) (model.Record, error) {
	ctx := context.Background()
	d, ok := b.Get(sid)
	if !ok {
		return model.Record{}, store.ErrNotFound
	}

	seq, err := b.rdb.Incr(ctx, b.key("seq", sid)).Result()
	if err != nil {
		return model.Record{}, err
	}

	stored := *rec
	stored.Seq = uint64(seq)
	if stored.ReceivedAt.IsZero() {
		stored.ReceivedAt = b.now()
	}
	stored.NormalizeUTC()
	if stored.Events == nil {
		stored.Events = []model.Event{}
	}
	blob, err := json.Marshal(stored)
	if err != nil {
		return model.Record{}, err
	}

	listKey := b.key("r", sid)
	recKey := b.key("rec", sid, strconv.FormatInt(seq, 10))
	pipe := b.rdb.TxPipeline()
	pipe.LPush(ctx, listKey, seq)
	pipe.LTrim(ctx, listKey, 0, int64(b.limits.MaxRecords-1))
	pipe.Set(ctx, recKey, blob, 0)
	pipe.PExpireAt(ctx, listKey, d.ExpiresAt)
	pipe.PExpireAt(ctx, recKey, d.ExpiresAt)
	pipe.PExpireAt(ctx, b.key("seq", sid), d.ExpiresAt)
	if _, err := pipe.Exec(ctx); err != nil {
		return model.Record{}, err
	}
	return stored, nil
}

// AppendEvent rewrites one record with an extra event.
//
// This is a read-modify-write of the record JSON rather than a separate list:
// it keeps the wire protocol identical to the memory backend (one whole record
// per update, which the browser swaps in wholesale) at the cost of re-encoding a
// bounded blob per frame. Records are capped at 500 events / 1 MiB.
func (b *Backend) AppendEvent(sid string, seq uint64, ev model.Event) (model.Record, error) {
	return b.mutate(sid, seq, func(rec *model.Record) {
		if len(rec.Events) >= b.limits.MaxEvents {
			rec.DroppedEvents++
			return
		}
		if ev.At.IsZero() {
			ev.At = b.now()
		}
		ev.At = ev.At.UTC()
		rec.Events = append(rec.Events, ev)
	})
}

// Close marks a record finished.
// PatchReq edits a stored record's request and republishes it.
func (b *Backend) PatchReq(sid string, seq uint64, fn func(*model.Request)) (model.Record, error) {
	return b.mutate(sid, seq, func(rec *model.Record) {
		if rec.Req != nil {
			fn(rec.Req)
		}
	})
}

func (b *Backend) Close(sid string, seq uint64, note string) (model.Record, error) {
	return b.mutate(sid, seq, func(rec *model.Record) {
		rec.Closed = true
		rec.CloseNote = note
	})
}

func (b *Backend) mutate(sid string, seq uint64, fn func(*model.Record)) (model.Record, error) {
	ctx := context.Background()
	key := b.key("rec", sid, strconv.FormatUint(seq, 10))
	raw, err := b.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return model.Record{}, store.ErrNotFound
	}
	if err != nil {
		return model.Record{}, err
	}
	var rec model.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return model.Record{}, err
	}
	fn(&rec)
	blob, err := json.Marshal(rec)
	if err != nil {
		return model.Record{}, err
	}
	if err := b.rdb.Set(ctx, key, blob, redis.KeepTTL).Err(); err != nil {
		return model.Record{}, err
	}
	return rec, nil
}

// List returns every record, newest first.
func (b *Backend) List(sid string) ([]model.Record, bool) {
	ctx := context.Background()
	if _, ok := b.Get(sid); !ok {
		return nil, false
	}
	seqs, err := b.rdb.LRange(ctx, b.key("r", sid), 0, -1).Result()
	if err != nil {
		return nil, false
	}
	if len(seqs) == 0 {
		return []model.Record{}, true
	}
	keys := make([]string, len(seqs))
	for i, seq := range seqs {
		keys[i] = b.key("rec", sid, seq)
	}
	blobs, err := b.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, false
	}
	out := make([]model.Record, 0, len(blobs))
	for _, value := range blobs {
		var raw []byte
		switch v := value.(type) {
		case string:
			raw = []byte(v)
		case []byte:
			raw = v
		default:
			continue
		}
		var rec model.Record
		if err := json.Unmarshal(raw, &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out, true
}

// DeleteExpired drops ids whose dumpster hash has already expired, and returns
// them so their subscribers can be closed. Redis does the actual deletion.
func (b *Backend) DeleteExpired(now time.Time) []string {
	ctx := context.Background()
	ids, err := b.rdb.SMembers(ctx, b.key("ids")).Result()
	if err != nil {
		return nil
	}
	var gone []string
	for _, sid := range ids {
		exists, err := b.rdb.Exists(ctx, b.key("d", sid)).Result()
		if err != nil {
			continue
		}
		if exists == 0 {
			_ = b.rdb.SRem(ctx, b.key("ids"), sid).Err()
			gone = append(gone, sid)
		}
	}
	sort.Strings(gone)
	return gone
}

// Len counts dumpsters including any Redis has expired but not yet swept.
func (b *Backend) Len() int {
	n, err := b.rdb.SCard(context.Background(), b.key("ids")).Result()
	if err != nil {
		return 0
	}
	return int(n)
}

// Health reports whether Redis answers.
func (b *Backend) Health() error { return b.rdb.Ping(context.Background()).Err() }

// Shutdown closes the client.
func (b *Backend) Shutdown() error { return b.rdb.Close() }
