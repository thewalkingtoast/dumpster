package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/adam/dumpster/internal/rawproto"
)

// selfSignedCert writes a throwaway certificate/key pair and returns their paths.
func selfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dumpster-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func grpcClientTLS(t *testing.T, a *App) *grpc.ClientConn {
	t.Helper()
	// InsecureSkipVerify is right here: the test cert is self-signed and the
	// point is that the listener speaks TLS at all.
	cc, err := grpc.NewClient(a.Addrs().GRPC,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		t.Fatalf("grpc.NewClient(tls): %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// Serving gRPC over TLS must actually encrypt the listener: a TLS client gets
// through, and a plaintext client does not.
func TestGRPCServesTLSWhenConfigured(t *testing.T) {
	certPath, keyPath := selfSignedCert(t)
	a := newTestApp(t, func(c *Config) {
		c.TLS = true
		c.GRPCTLSCert = certPath
		c.GRPCTLSKey = keyPath
	})
	id := rent(t, a)
	method := "/d" + strings.ReplaceAll(id, "-", "") + "/Anything"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := &rawproto.Message{}
	if err := grpcClientTLS(t, a).Invoke(ctx, method,
		&rawproto.Message{Body: []byte("over tls")}, out,
		grpc.ForceCodec(rawproto.Codec{})); err != nil {
		t.Fatalf("TLS invoke: %v", err)
	}
	if string(out.Body) != "over tls" {
		t.Fatalf("echo = %q, want the payload back", out.Body)
	}

	plain, err := grpc.NewClient(a.Addrs().GRPC,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(plaintext): %v", err)
	}
	defer plain.Close()
	if err := plain.Invoke(ctx, method,
		&rawproto.Message{Body: []byte("in the clear")}, &rawproto.Message{},
		grpc.ForceCodec(rawproto.Codec{})); err == nil {
		t.Fatal("a plaintext client reached a TLS listener: the listener is not encrypted")
	}

	if got := len(records(t, a, id)); got != 1 {
		t.Fatalf("records = %d, want 1 (only the TLS call should have landed)", got)
	}
}

// Half a TLS configuration must stop the server, not silently serve plaintext.
func TestGRPCRefusesHalfATLSConfig(t *testing.T) {
	certPath, _ := selfSignedCert(t)

	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.GRPCAddr = "127.0.0.1:0"
	cfg.TCPAddr = "127.0.0.1:0"
	cfg.UDPAddr = "127.0.0.1:0"
	cfg.TLS = true
	cfg.GRPCTLSCert = certPath // key deliberately missing

	err := New(cfg).Start()
	if err == nil {
		t.Fatal("Start succeeded with a cert but no key: the listener would be plaintext")
	}
	if !strings.Contains(err.Error(), "needs both a cert and a key") {
		t.Fatalf("error = %v, want it to name the missing half", err)
	}
}

// A certificate without -tls means the operator believes TLS is on while the
// switch says otherwise. Refusing is the only honest reading: serve TLS and the
// page still advertises ws://, or serve plaintext while a cert sits there.
func TestGRPCRefusesACertWithoutTheTLSFlag(t *testing.T) {
	certPath, keyPath := selfSignedCert(t)

	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.GRPCAddr = "127.0.0.1:0"
	cfg.TCPAddr = "127.0.0.1:0"
	cfg.UDPAddr = "127.0.0.1:0"
	cfg.TLS = false
	cfg.GRPCTLSCert = certPath
	cfg.GRPCTLSKey = keyPath

	err := New(cfg).Start()
	if err == nil {
		t.Fatal("Start succeeded with a certificate while -tls was off")
	}
	if !strings.Contains(err.Error(), "-tls is off") {
		t.Fatalf("error = %v, want it to name the contradiction", err)
	}
}

// The switch is what the page reads, so gRPC and the copy-paste examples cannot
// drift apart.
func TestTLSSwitchDrivesGRPC(t *testing.T) {
	certPath, keyPath := selfSignedCert(t)

	off := newTestApp(t)
	if off.cfg.TLS {
		t.Fatal("the test app claims TLS by default")
	}
	if creds, err := off.grpcTransportCreds(); err != nil || creds != nil {
		t.Fatalf("plaintext app returned creds (creds=%v err=%v)", creds, err)
	}

	on := newTestApp(t, func(c *Config) {
		c.TLS = true
		c.GRPCTLSCert = certPath
		c.GRPCTLSKey = keyPath
	})
	if !on.cfg.TLS {
		t.Fatal("the switch did not stick")
	}
	if creds, err := on.grpcTransportCreds(); err != nil || creds == nil {
		t.Fatalf("TLS app returned no creds (err=%v)", err)
	}
}
