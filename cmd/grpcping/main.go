// Command grpcping sends one message to a dumpster over gRPC using the path
// form /d<uuid-hex>/Anything, and prints the echo. It exists so the smoke test
// and the plan's verification steps need neither grpcurl nor a .proto file.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/thewalkingtoast/dumpster/internal/rawproto"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "gRPC address")
	id := flag.String("id", "", "dumpster uuid")
	msg := flag.String("msg", "hello", "payload to send")
	useTLS := flag.Bool("tls", false, "dial over TLS; the certificate is not verified (this is a debug tool, not a bank)")
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "grpcping: -id is required")
		os.Exit(2)
	}

	var creds credentials.TransportCredentials = insecure.NewCredentials()
	if *useTLS {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	}

	cc, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		fmt.Fprintf(os.Stderr, "grpcping: dial: %v\n", err)
		os.Exit(1)
	}
	defer cc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	method := "/d" + strings.ReplaceAll(*id, "-", "") + "/Anything"
	out := &rawproto.Message{}
	err = cc.Invoke(ctx, method,
		&rawproto.Message{Body: []byte(*msg)}, out,
		grpc.ForceCodec(rawproto.Codec{}))
	if err != nil {
		fmt.Fprintf(os.Stderr, "grpcping: invoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("echo: %s\n", out.Body)
}
