package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/rawproto"
)

// newGRPCServer builds a server that accepts any service and method. The forced
// codec means the payload is never parsed as protobuf, so no .proto file is
// needed to dump a call.
func (a *App) newGRPCServer(creds credentials.TransportCredentials) *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.ForceServerCodec(rawproto.Codec{}),
		grpc.MaxRecvMsgSize(a.cfg.MaxBody),
		grpc.MaxSendMsgSize(a.cfg.MaxBody),
		grpc.UnknownServiceHandler(a.handleGRPCStream),
	}
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}
	return grpc.NewServer(opts...)
}

// grpcTransportCreds loads the configured certificate, or returns nil for
// plaintext h2c. Half a configuration is refused outright rather than quietly
// falling back to an unencrypted listener.
func (a *App) grpcTransportCreds() (credentials.TransportCredentials, error) {
	cert, key := a.cfg.GRPCTLSCert, a.cfg.GRPCTLSKey
	switch {
	case !a.cfg.TLS && cert == "" && key == "":
		return nil, nil // plaintext h2c, and nothing on the page claims otherwise
	case !a.cfg.TLS:
		// A certificate without the switch means the operator believes something
		// that is not true: refuse rather than pick one of the two stories.
		return nil, fmt.Errorf("grpc tls cert/key set but -tls is off: enable -tls or remove them")
	case cert == "" || key == "":
		return nil, fmt.Errorf("-tls is on but grpc needs both a cert and a key (cert=%q key=%q)", cert, key)
	}
	creds, err := credentials.NewServerTLSFromFile(cert, key)
	if err != nil {
		return nil, fmt.Errorf("grpc tls: %w", err)
	}
	return creds, nil
}

// handleGRPCStream records one RPC and echoes its first message back.
func (a *App) handleGRPCStream(_ any, stream grpc.ServerStream) error {
	full, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "no method on stream")
	}
	ctx := stream.Context()

	sid, method := parseGRPCMethod(full)
	if sid == "" {
		if md, mdOK := metadata.FromIncomingContext(ctx); mdOK {
			if v := md.Get("x-dumpster-id"); len(v) > 0 {
				sid = v[0]
			}
		}
	}
	if sid == "" {
		return status.Error(codes.InvalidArgument,
			"no dumpster: call /d<uuid-hex>/Method or send x-dumpster-id metadata")
	}
	canonical, err := id.Canonical(sid)
	if err != nil {
		return status.Error(codes.InvalidArgument, "bad dumpster id")
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		return status.Error(codes.NotFound, "unknown or expired dumpster")
	}

	start := time.Now()
	rec := &model.Record{
		Kind:       model.KindGRPC,
		ReceivedAt: start,
		Remote:     remoteFromPeer(ctx),
		Req:        &model.Request{Protocol: "grpc", Path: full, Headers: headerMap(ctx)},
		Meta:       map[string]string{"method": method},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return status.Error(codes.Internal, "could not record")
	}
	a.publishNew(d.ID, &stored)

	var first []byte
	total := 0
	for {
		var msg rawproto.Message
		err := stream.RecvMsg(&msg)
		if err == io.EOF {
			break
		}
		if err != nil {
			a.closeRecord(d.ID, stored.Seq, "recv: "+err.Error())
			return err
		}
		total += len(msg.Body)
		if first == nil {
			first = msg.Body
		}
		ev := model.Event{At: time.Now(), Dir: "in", Type: "message", Bytes: len(msg.Body)}
		if utf8.Valid(msg.Body) {
			ev.Text = truncateText(string(msg.Body), maxInlineText)
		} else {
			ev.Base64 = base64.StdEncoding.EncodeToString(msg.Body)
		}
		if updated, err := a.store.AppendEvent(d.ID, stored.Seq, ev); err == nil {
			a.publishUpdate(d.ID, &updated)
		}
	}
	// The record was created before any bytes arrived, so the payload is written
	// back now — otherwise the request panel claims the RPC carried nothing.
	if total > 0 {
		if updated, err := a.store.PatchReq(d.ID, stored.Seq, func(req *model.Request) {
			req.BodyBytes = total
			req.BodyText, req.BodyBase64 = payloadPreview(first)
		}); err == nil {
			a.publishUpdate(d.ID, &updated)
		}
	}
	a.closeRecord(d.ID, stored.Seq, "rpc finished")

	if first == nil {
		first = []byte{}
	}
	return stream.SendMsg(&rawproto.Message{Body: first})
}

// parseGRPCMethod splits "/d<hex>/Method" into ("<hex>", "Method"). Any other
// service name yields an empty dumpster id, which forces the metadata route.
func parseGRPCMethod(full string) (sid, method string) {
	parts := strings.Split(strings.TrimPrefix(full, "/"), "/")
	if len(parts) != 2 {
		return "", ""
	}
	service := parts[0]
	if len(service) == 33 && (service[0] == 'd' || service[0] == 'D') {
		return service[1:], parts[1]
	}
	return "", parts[1]
}

// headerMap copies incoming gRPC metadata into the record's header map.
func headerMap(ctx context.Context) map[string][]string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	out := make(map[string][]string, len(md))
	for k, v := range md {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// remoteFromPeer describes the gRPC caller.
func remoteFromPeer(ctx context.Context) model.Remote {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return model.Remote{}
	}
	return remoteFromAddr(p.Addr)
}
