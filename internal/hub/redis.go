package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisBroker relays hub messages between processes over Redis Pub/Sub.
//
// Each message is wrapped in an envelope carrying the publishing instance id, so
// an instance ignores its own traffic: every subscriber receives every message,
// including the sender, and without the filter a single ingest would render twice.
//
// Pub/Sub is fire-and-forget. That is acceptable here because a browser that
// misses a message is corrected by the next one, and every reconnect starts from
// a full snapshot.
type RedisBroker struct {
	rdb    *redis.Client
	prefix string
	self   string
}

type envelope struct {
	Origin  string          `json:"origin"`
	Payload json.RawMessage `json:"payload"`
}

// NewRedisBroker dials Redis and verifies it answers.
func NewRedisBroker(ctx context.Context, url, prefix, self string) (*RedisBroker, error) {
	if prefix == "" {
		prefix = "dumpster:"
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &RedisBroker{rdb: client, prefix: prefix, self: self}, nil
}

// Publish sends one payload to the dumpster's channel.
func (b *RedisBroker) Publish(ctx context.Context, dumpsterID string, payload []byte) error {
	body, err := json.Marshal(envelope{Origin: b.self, Payload: payload})
	if err != nil {
		return err
	}
	return b.rdb.Publish(ctx, b.channel(dumpsterID), body).Err()
}

// Run delivers remote payloads until ctx is cancelled.
func (b *RedisBroker) Run(ctx context.Context, handle func(string, []byte)) error {
	sub := b.rdb.PSubscribe(ctx, b.prefix+"msg:*")
	defer func() { _ = sub.Close() }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, open := <-sub.Channel():
			if !open {
				return nil
			}
			var env envelope
			if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
				continue
			}
			if env.Origin == b.self {
				continue
			}
			handle(strings.TrimPrefix(msg.Channel, b.prefix+"msg:"), env.Payload)
		}
	}
}

// Close releases the connection.
func (b *RedisBroker) Close() error { return b.rdb.Close() }

func (b *RedisBroker) channel(dumpsterID string) string {
	return b.prefix + "msg:" + dumpsterID
}
