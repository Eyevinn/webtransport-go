package integrationtests

// This test exercises the reason the Eyevinn fork of webtransport-go exists:
// the client must interoperate with web-transport-quinn based endpoints
// (moq-rs / cdn.moq.dev, Cloudflare's WT endpoint), which only understand the
// WEBTRANSPORT_MAX_SESSIONS codepoint (0xc671706a) and validate the client's
// SETTINGS with supports_webtransport(). Stock upstream v0.11.x sends and
// accepts only SETTINGS_WT_ENABLED (0x2c7cf000), so it fails against quinn in
// both directions; the fork additionally sends and accepts 0xc671706a.
//
// The test drives the real web-transport-quinn `echo-server` example, so a
// green run proves the fork actually handshakes with a quinn peer. It skips
// (rather than fails) when the compiled echo-server is not available, so it
// stays out of the way in environments without the Rust workspace.
//
// Build the server once from the web-transport workspace:
//
//	cargo build -p web-transport-quinn --example echo-server
//
// then run:
//
//	go test -run TestQuinnInterop -v
//
// Point QUINN_ECHO_SERVER at the binary if it lives somewhere non-default.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/quic-go/webtransport-go"
)

func locateQuinnEchoServer(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("QUINN_ECHO_SERVER"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("QUINN_ECHO_SERVER=%q not usable: %v", p, err)
		}
		return p
	}
	// Default layout: the web-transport Rust workspace sits next to this fork.
	def := filepath.Join("..", "..", "web-transport", "target", "debug", "examples", "echo-server")
	if _, err := os.Stat(def); err != nil {
		t.Skipf("quinn echo-server not found (build it with "+
			"`cargo build -p web-transport-quinn --example echo-server` or set "+
			"QUINN_ECHO_SERVER); looked at %s", def)
	}
	return def
}

// selfSignedCert writes a short-lived EC cert+key pair to dir and returns the paths.
func selfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	_ = c.Close()
	return port
}

func TestQuinnInterop(t *testing.T) {
	server := locateQuinnEchoServer(t)

	dir := t.TempDir()
	certPath, keyPath := selfSignedCert(t, dir)
	port := freeLoopbackPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var serverLog bytes.Buffer
	cmd := exec.CommandContext(ctx, server,
		"--addr", addr,
		"--tls-cert", certPath,
		"--tls-key", keyPath,
	)
	cmd.Env = append(os.Environ(), "RUST_LOG=info")
	cmd.Stdout = &serverLog
	cmd.Stderr = &serverLog
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start echo-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("echo-server log:\n%s", serverLog.String())
		}
	})

	d := &webtransport.Transport{
		// quinn's web-transport-proto rejects the draft-15 "webtransport-h3"
		// :protocol token, so ask for the legacy "webtransport" token.
		LegacyConnectProtocol: true,
		// quinn does not implement the QUIC RESET_STREAM_AT extension that
		// draft-ietf-webtrans-http3-16 requires, so don't insist on it.
		AllowPeerWithoutPartialDelivery: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{http3.NextProtoH3},
		},
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
		},
	}
	defer d.Close()

	// The server needs a moment to bind; retry the dial until it is up.
	url := "https://" + addr + "/"
	var sess *webtransport.Session
	deadline := time.Now().Add(15 * time.Second)
	for {
		dialCtx, dialCancel := context.WithTimeout(ctx, 2*time.Second)
		rsp, s, err := d.Dial(dialCtx, url, nil)
		dialCancel()
		if err == nil {
			if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
				t.Fatalf("unexpected CONNECT status: %d", rsp.StatusCode)
			}
			sess = s
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not establish WebTransport session with quinn echo-server: %v\nserver log:\n%s", err, serverLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer sess.CloseWithError(0, "")

	// Handshake succeeded — that alone proves the codepoint interop. Now prove
	// the data path works: the echo-server reads a bidi stream to EOF and
	// echoes it back.
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	want := []byte("hello quinn from webtransport-go")
	if _, err := stream.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stream.Close(); err != nil { // FIN so the server's read_to_end returns
		t.Fatalf("close send side: %v", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo mismatch: got %q want %q", got, want)
	}
	t.Logf("quinn interop OK: negotiated protocol %q, echoed %d bytes",
		sess.SessionState().ApplicationProtocol, len(got))
}
