package racing_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	http "github.com/dteh/dhttp"
	"github.com/dteh/dhttp/racing"
	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// TestEngineGateSmoke fires a batch of 10 POST requests through the
// single-packet engine against httpbin.dev/anything and verifies every
// stream gets a 200 response with the expected echo body. This validates
// the full engine + gate + read-loop path end-to-end but doesn't try to
// measure server-side request-arrival skew (that requires a target with
// race-condition detection instrumentation).
//
// Tagged as a network test — skip with -short.
func TestEngineGateSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}

	eng, err := racing.NewEngine("https://httpbin.dev")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	const N = 10
	g := eng.NewGate()
	for i := 0; i < N; i++ {
		body := fmt.Sprintf(`{"i":%d}`, i)
		req, err := http.NewRequest("POST", "https://httpbin.dev/anything", strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Race-Idx", fmt.Sprint(i))
		if err := g.Add(req); err != nil {
			t.Fatalf("gate.Add[%d]: %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resps, err := g.Send(ctx)
	if err != nil {
		t.Fatalf("gate.Send: %v", err)
	}
	if got, want := len(resps), N; got != want {
		t.Fatalf("got %d responses, want %d", got, want)
	}
	for i, resp := range resps {
		if resp == nil {
			t.Errorf("response %d is nil", i)
			continue
		}
		if resp.StatusCode != 200 {
			t.Errorf("response %d: status %d, want 200", i, resp.StatusCode)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("response %d: read body: %v", i, err)
			continue
		}
		// httpbin echoes our JSON body inside its "data" string field as
		// escaped JSON, so the substring is "\\\"i\\\":N". Also check the
		// echoed X-Race-Idx header so a mis-routed response is caught even
		// if two bodies happened to be the same.
		dataField := fmt.Sprintf(`\"i\":%d`, i)
		idxHeader := fmt.Sprintf(`"X-Race-Idx": [`+"\n"+`      "%d"`, i)
		if !strings.Contains(string(body), dataField) {
			t.Errorf("response %d: body does not contain %q\nbody (first 500): %s", i, dataField, truncate(body, 500))
		}
		if !strings.Contains(string(body), idxHeader) {
			t.Errorf("response %d: body does not echo X-Race-Idx: %d", i, i)
		}
	}
}

// startH2HeaderCaptureServer spins up an in-process TLS listener that
// speaks HTTP/2 manually (no net/http server, so we keep frame-level
// access). On the first client connection it completes the preface +
// SETTINGS dance, reads one HEADERS frame, decodes the field list (which
// preserves wire order — unlike http.Header which is a map), and pushes
// the slice onto the returned channel. It then sends a minimal 200
// response so the client doesn't block. Returns (serverURL, recordedCh,
// closeFn).
func startH2HeaderCaptureServer(t *testing.T) (string, <-chan []hpack.HeaderField, func()) {
	t.Helper()
	cert := localhostCert(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	recorded := make(chan []hpack.HeaderField, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read client preface.
		preface := make([]byte, len("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
		if _, err := io.ReadFull(conn, preface); err != nil {
			return
		}

		framer := http2.NewFramer(conn, conn)
		// Send server SETTINGS + ACK exchange.
		if err := framer.WriteSettings(); err != nil {
			return
		}

		dec := hpack.NewDecoder(4096, nil)
		var sentResp bool
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch f := frame.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					framer.WriteSettingsAck()
				}
			case *http2.HeadersFrame:
				fields, err := dec.DecodeFull(f.HeaderBlockFragment())
				if err != nil {
					return
				}
				select {
				case recorded <- fields:
				default:
				}
				if !sentResp {
					// Minimal 200 response so the client's gate.Send unblocks.
					var hbuf bytes.Buffer
					enc := hpack.NewEncoder(&hbuf)
					enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
					framer.WriteHeaders(http2.HeadersFrameParam{
						StreamID:      f.StreamID,
						BlockFragment: hbuf.Bytes(),
						EndStream:     true,
						EndHeaders:    true,
					})
					sentResp = true
				}
			}
		}
	}()

	url := "https://" + listener.Addr().String()
	return url, recorded, func() { listener.Close() }
}

// localhostCert mints a self-signed cert valid for 127.0.0.1.
func localhostCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "racing-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

func insecureTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// TestEngineHonoursHeaderOrderKey verifies the h2 engine emits headers in
// the order specified by HeaderOrderKey rather than Go map iteration order.
// Uses an in-process h2 server (so we can introspect the raw HEADERS
// frame's field order, which a real public echo service would lose to
// JSON map serialisation).
func TestEngineHonoursHeaderOrderKey(t *testing.T) {
	srv, recorded, close := startH2HeaderCaptureServer(t)
	defer close()

	eng, err := racing.NewEngine(srv,
		racing.WithTLSConfig(insecureTLSConfig()))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	g := eng.NewGate()
	req, err := http.NewRequest("POST", srv+"/echo", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Race-A", "aaa")
	req.Header.Set("X-Race-B", "bbb")
	req.Header.Set("X-Race-C", "ccc")
	req.Header.Set("X-Race-D", "ddd")
	req.Header[http.HeaderOrderKey] = []string{
		"x-race-d",
		"x-race-c",
		"x-race-b",
		"x-race-a",
	}
	if err := g.Add(req); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := g.Send(ctx); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Pull the recorded header field order out of the test server and
	// assert the four x-race-* headers appear in the order we asked for.
	fields := <-recorded
	var got []string
	for _, hf := range fields {
		if strings.HasPrefix(hf.Name, "x-race-") {
			got = append(got, hf.Name)
		}
	}
	want := []string{"x-race-d", "x-race-c", "x-race-b", "x-race-a"}
	if !slicesEqual(got, want) {
		t.Errorf("wire-order of x-race-* headers = %v, want %v\nall fields: %v", got, want, fields)
	}
}

// TestH1EngineGateSmoke fires 10 POSTs through the h1 last-byte-sync
// engine against httpbin.dev (which speaks both h1 and h2 — we force h1
// via ALPN). Validates the per-conn dial + prime + barrier-fire +
// response-read pipeline end-to-end. Doesn't measure server-side skew.
func TestH1EngineGateSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	eng, err := racing.NewH1Engine("https://httpbin.dev")
	if err != nil {
		t.Fatalf("NewH1Engine: %v", err)
	}
	defer eng.Close()

	const N = 10
	g := eng.NewGate()
	for i := 0; i < N; i++ {
		body := fmt.Sprintf(`{"i":%d}`, i)
		req, err := http.NewRequest("POST", "https://httpbin.dev/anything", strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Race-Idx", fmt.Sprint(i))
		if err := g.Add(req); err != nil {
			t.Fatalf("h1 gate.Add[%d]: %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resps, err := g.Send(ctx)
	if err != nil {
		t.Fatalf("h1 gate.Send: %v", err)
	}
	if got, want := len(resps), N; got != want {
		t.Fatalf("got %d responses, want %d", got, want)
	}
	for i, resp := range resps {
		if resp == nil {
			t.Errorf("response %d is nil", i)
			continue
		}
		if resp.StatusCode != 200 {
			t.Errorf("response %d: status %d, want 200", i, resp.StatusCode)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("response %d: read body: %v", i, err)
			continue
		}
		dataField := fmt.Sprintf(`\"i\":%d`, i)
		if !strings.Contains(string(body), dataField) {
			t.Errorf("response %d: body does not contain %q\nbody (first 500): %s", i, dataField, truncate(body, 500))
		}
	}
}

// TestH1EngineRequiresHTTPS mirrors the h2 version.
func TestH1EngineRequiresHTTPS(t *testing.T) {
	_, err := racing.NewH1Engine("http://example.com")
	if err == nil {
		t.Fatal("expected error for http:// target")
	}
}

// TestEngineRequiresHTTPS confirms NewEngine refuses non-https targets;
// the single-packet attack relies on HTTP/2 which only works over TLS in
// practice.
func TestEngineRequiresHTTPS(t *testing.T) {
	_, err := racing.NewEngine("http://example.com")
	if err == nil {
		t.Fatal("expected error for http:// target")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("expected error to mention https, got: %v", err)
	}
}

// TestGateAddAfterSend verifies single-use gate semantics.
func TestGateAddAfterSend(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	eng, err := racing.NewEngine("https://httpbin.dev")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	g := eng.NewGate()
	req, _ := http.NewRequest("GET", "https://httpbin.dev/get", nil)
	if err := g.Add(req); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := g.Send(ctx); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Add after Send must fail.
	req2, _ := http.NewRequest("GET", "https://httpbin.dev/get", nil)
	if err := g.Add(req2); err == nil {
		t.Error("Add after Send: expected error, got nil")
	}
	// Second Send must fail.
	if _, err := g.Send(ctx); err == nil {
		t.Error("Send after Send: expected error, got nil")
	}
}
