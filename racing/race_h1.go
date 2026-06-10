package racing

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	http "github.com/dteh/dhttp"
	tls "github.com/refraction-networking/utls"
)

// H1Engine manages HTTP/1.1 last-byte-sync race attacks. Each request runs
// over its own TCP+TLS connection (h1 cannot multiplex), so this trades the
// precision of h2 single-packet (~100µs skew typical) for compatibility
// with h1-only targets (~ms skew typical, since the kernel has to ship N
// separate TCP segments instead of coalescing into one).
//
// Use H1Engine when the target negotiates http/1.1 only (no h2 ALPN), or
// when you want to verify a race condition reproduces under a different
// network path than h2.
type H1Engine struct {
	target  string // host:port
	scheme  string // "https"
	helloID tls.ClientHelloID
	tlsConf *tls.Config
	dial    func(context.Context, string) (net.Conn, error)
}

// NewH1Engine returns a configured H1Engine. No connections are opened
// until the first Gate.Add — each Add opens a fresh TCP+TLS connection
// since HTTP/1.1 cannot multiplex.
func NewH1Engine(target string, opts ...Option) (*H1Engine, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("racing: parse target: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("racing: target must be https:// (got %q)", u.Scheme)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}

	o := engineOpts{
		helloID: tls.HelloChrome_Auto,
		tlsConf: &tls.Config{ServerName: host},
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.tlsConf.ServerName == "" {
		o.tlsConf.ServerName = host
	}
	if o.dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		o.dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr)
		}
	}

	return &H1Engine{
		target:  net.JoinHostPort(host, port),
		scheme:  "https",
		helloID: o.helloID,
		tlsConf: o.tlsConf,
		dial:    o.dial,
	}, nil
}

// Close releases any per-engine state. H1Engine holds no persistent
// connections; each Gate owns its own. Provided for symmetry with Engine.
func (e *H1Engine) Close() error { return nil }

// NewGate returns a fresh H1Gate. Single-use — after Send, create a new gate.
func (e *H1Engine) NewGate() *H1Gate {
	return &H1Gate{engine: e}
}

// H1Gate stages N requests across N TCP+TLS connections and fires the
// final byte on each as close to simultaneously as goroutine scheduling
// allows.
type H1Gate struct {
	engine *H1Engine
	primed []*h1Primed
	sent   bool
}

// h1Primed holds the per-connection state between Add and Send.
type h1Primed struct {
	conn     net.Conn
	reader   *bufio.Reader
	req      *http.Request
	tailByte byte
}

// Add primes one request on a fresh connection. Opens a TCP+TLS conn,
// writes the entire serialised request minus its final byte, and stages
// that byte for fire on Send.
//
// For requests with a body the held-back byte is the last body byte; for
// requests without a body it's the final '\n' of the headers terminator —
// either way the server is left blocking on a single byte from the
// kernel, ready to dispatch the moment we release it.
//
// The request's Body, if any, is read end-to-end here (dhttp's
// Request.Write consumes it).
func (g *H1Gate) Add(req *http.Request) error {
	if g.sent {
		return errors.New("racing: h1 gate already sent")
	}
	if req.URL == nil {
		return errors.New("racing: request URL is nil")
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	// Serialise the request to bytes via dhttp's Request.Write — which
	// also honours HeaderOrderKey if set, so wire-level header order is
	// preserved through this path.
	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		return fmt.Errorf("racing: serialise request: %w", err)
	}
	data := buf.Bytes()
	if len(data) == 0 {
		return errors.New("racing: serialised request is empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	plain, err := g.engine.dial(ctx, g.engine.target)
	if err != nil {
		return fmt.Errorf("racing: dial: %w", err)
	}
	if tcp, ok := plain.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}

	cfg := g.engine.tlsConf.Clone()
	cfg.NextProtos = []string{"http/1.1"}

	// utls parrots carry their own ALPN list inside the ClientHello spec,
	// so cfg.NextProtos alone won't stop us from advertising h2. Pull the
	// parrot's spec, strip h2 from its ALPN extension, and apply via
	// HelloCustom so the wire ClientHello still looks like the parrot
	// — minus h2.
	spec, err := tls.UTLSIdToSpec(g.engine.helloID)
	if err != nil {
		plain.Close()
		return fmt.Errorf("racing: load parrot spec: %w", err)
	}
	for _, ext := range spec.Extensions {
		alpn, ok := ext.(*tls.ALPNExtension)
		if !ok {
			continue
		}
		filtered := alpn.AlpnProtocols[:0]
		for _, p := range alpn.AlpnProtocols {
			if p != "h2" {
				filtered = append(filtered, p)
			}
		}
		alpn.AlpnProtocols = filtered
	}
	tconn := tls.UClient(plain, cfg, tls.HelloCustom)
	if err := tconn.ApplyPreset(&spec); err != nil {
		tconn.Close()
		return fmt.Errorf("racing: apply parrot spec: %w", err)
	}
	if err := tconn.Handshake(); err != nil {
		tconn.Close()
		return fmt.Errorf("racing: tls handshake: %w", err)
	}
	if proto := tconn.ConnectionState().NegotiatedProtocol; proto != "" && proto != "http/1.1" {
		tconn.Close()
		return fmt.Errorf("racing: server negotiated %q, want http/1.1", proto)
	}

	// Write all-but-last byte. Server's HTTP parser will buffer this and
	// block waiting for the final byte (either the last body byte or the
	// terminator '\n').
	if _, err := tconn.Write(data[:len(data)-1]); err != nil {
		tconn.Close()
		return fmt.Errorf("racing: prime write: %w", err)
	}

	g.primed = append(g.primed, &h1Primed{
		conn:     tconn,
		reader:   bufio.NewReader(tconn),
		req:      req,
		tailByte: data[len(data)-1],
	})
	return nil
}

// Send releases the final byte on every primed connection from a single
// barrier so all N writers wake up in the same scheduling tick. The kernel
// still has to send N separate TCP segments (no coalescing across
// connections), so server-side skew is typically larger than h2
// single-packet — but still far tighter than sequential http.Client.Do.
//
// Returns responses in Add order; resps[i] may be nil if that connection
// errored. Closes every connection before returning.
func (g *H1Gate) Send(ctx context.Context) ([]*http.Response, error) {
	if g.sent {
		return nil, errors.New("racing: h1 gate already sent")
	}
	g.sent = true
	if len(g.primed) == 0 {
		return nil, errors.New("racing: h1 gate has no primed connections")
	}

	resps := make([]*http.Response, len(g.primed))
	errs := make([]error, len(g.primed))
	ready := make(chan struct{})
	var wg sync.WaitGroup

	for i, p := range g.primed {
		i, p := i, p
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer p.conn.Close()

			// Block until the barrier opens — every goroutine releases together.
			<-ready

			if _, err := p.conn.Write([]byte{p.tailByte}); err != nil {
				errs[i] = fmt.Errorf("racing: tail write: %w", err)
				return
			}

			// Bound the response read so a hung server doesn't keep us here forever.
			if d, ok := ctx.Deadline(); ok {
				p.conn.SetReadDeadline(d)
			} else {
				p.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			}

			resp, err := http.ReadResponse(p.reader, p.req)
			if err != nil {
				errs[i] = fmt.Errorf("racing: read response: %w", err)
				return
			}
			// Drain into a buffer so we can close the conn cleanly and the
			// caller can still re-read the body.
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				errs[i] = fmt.Errorf("racing: read body: %w", err)
				return
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resps[i] = resp
		}()
	}

	// Open the barrier — all N goroutines race to write their single byte.
	close(ready)

	// Wait for all to finish, or ctx to expire.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// Force-close all conns to unblock the readers, then wait for them
		// to exit.
		for _, p := range g.primed {
			p.conn.Close()
		}
		wg.Wait()
		return resps, ctx.Err()
	}

	// Surface the first per-stream error if any, but keep returning the
	// full slice so the caller can inspect partial successes.
	for _, err := range errs {
		if err != nil {
			return resps, err
		}
	}
	return resps, nil
}
