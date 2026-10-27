package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/rawproto"
)

func grpcClient(t *testing.T, a *App) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(a.Addrs().GRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func TestGRPCDumpsRawMessageByPath(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)
	cc := grpcClient(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	method := "/d" + strings.ReplaceAll(id, "-", "") + "/Anything"
	out := &rawproto.Message{}
	err := cc.Invoke(ctx, method,
		&rawproto.Message{Body: []byte("ping")},
		out,
		grpc.ForceCodec(rawproto.Codec{}),
	)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(out.Body) != "ping" {
		t.Fatalf("echo = %q, want ping", out.Body)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindGRPC {
		t.Fatalf("kind = %q, want grpc", rec.Kind)
	}
	if len(rec.Events) != 1 || rec.Events[0].Text != "ping" {
		t.Fatalf("events = %+v", rec.Events)
	}
	if !rec.Closed {
		t.Fatal("record should be closed when the RPC returns")
	}
}

func TestGRPCDumpsByMetadataHeader(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)
	cc := grpcClient(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-dumpster-id", id)

	out := &rawproto.Message{}
	err := cc.Invoke(ctx, "/dumpster.Ingest/Anything",
		&rawproto.Message{Body: []byte("hi")}, out, grpc.ForceCodec(rawproto.Codec{}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := len(records(t, a, id)); got != 1 {
		t.Fatalf("records = %d, want 1", got)
	}
}

func TestGRPCUnknownDumpster(t *testing.T) {
	a := newTestApp(t)
	cc := grpcClient(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cc.Invoke(ctx, "/d00000000000000700080000000000000/Any",
		&rawproto.Message{Body: []byte("x")}, &rawproto.Message{}, grpc.ForceCodec(rawproto.Codec{}))
	if err == nil {
		t.Fatal("expected NotFound")
	}
	if !strings.Contains(err.Error(), "unknown or expired") {
		t.Fatalf("err = %v", err)
	}
}
