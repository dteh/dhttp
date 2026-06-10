// Package racing provides HTTP/2 single-packet attack primitives for
// race-condition testing, modelled on PortSwigger's "engine + gate" pattern:
// https://portswigger.net/web-security/race-conditions
//
// An [Engine] owns a dedicated HTTP/2 connection to a target. A [Gate]
// stages N requests on that connection — writing all but the final DATA
// frame of each — then releases the N tail frames in a single TCP write.
// The kernel coalesces them into one TCP segment; the server's epoll wakes
// once and dispatches all N requests within the same scheduling tick.
// Observed end-to-end skew on a localhost target is typically <100µs, well
// inside the window where most TOCTOU race conditions trigger.
//
// Quick start:
//
//	eng, err := racing.NewEngine("https://example.com",
//	    racing.WithHelloID(tls.HelloChrome_Auto))
//	if err != nil { ... }
//	defer eng.Close()
//
//	g := eng.NewGate()
//	for i := 0; i < 30; i++ {
//	    req, _ := http.NewRequest("POST", "https://example.com/redeem",
//	        strings.NewReader(`{"code":"ABC"}`))
//	    req.Header.Set("Content-Type", "application/json")
//	    if err := g.Add(req); err != nil { ... }
//	}
//	resps, err := g.Send(ctx)
//
// Only HTTP/2 targets are supported (the single-packet attack relies on
// stream multiplexing). For HTTP/1.1-only targets, see the separate
// last-byte-sync technique — not implemented here yet.
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	http "github.com/dteh/dhttp"
	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	// h2 initial flow-control window per stream. The engine will not let a
	// single Add buffer more than this for the request body; targets with
	// larger windows are unaffected.
	defaultStreamWindow = 65535
)

// Engine manages a dedicated HTTP/2 connection used for race-condition
// testing. One Engine = one TLS+h2 connection to one target. Create
// multiple Engines for multiple targets, or for fan-out attack patterns
// within a single target.
//
// Engine is safe for concurrent Gate use, but each individual Gate is
// single-threaded (Add and Send must be called from one goroutine).
type Engine struct {
	target  string // host:port
	scheme  string // "https" only for now
	helloID tls.ClientHelloID
	tlsConf *tls.Config

	conn   net.Conn
	framer *http2.Framer

	writeMu  sync.Mutex // serialises framer writes
	hpackBuf *bytes.Buffer
	hpackEnc *hpack.Encoder

	nextSID atomic.Uint32 // next client stream ID; odd starting at 1

	mu          sync.Mutex
	pending     map[uint32]*streamState
	serverSetup chan struct{}
	closed      chan struct{}
	closeErr    error
}

// streamState is the in-flight state for one HTTP/2 stream.
type streamState struct {
	id          uint32
	respHeaders http.Header
	respStatus  int
	respBody    bytes.Buffer
	done        chan struct{}
	doneOnce    sync.Once // guards close(done) — END_STREAM can arrive on HEADERS or DATA, and RST_STREAM / engine close also fire it
	err         error
	req         *http.Request
}

// finish closes done exactly once, regardless of which code path completed
// the stream (HEADERS+END_STREAM, DATA+END_STREAM, RST_STREAM, engine close,
// or a malformed server response).
func (st *streamState) finish(err error) {
	st.doneOnce.Do(func() {
		if err != nil && st.err == nil {
			st.err = err
		}
		close(st.done)
	})
}

// Option configures an Engine.
type Option func(*engineOpts)

type engineOpts struct {
	helloID tls.ClientHelloID
	tlsConf *tls.Config
	dial    func(context.Context, string) (net.Conn, error)
}

// WithHelloID sets the utls ClientHello fingerprint. Defaults to
// HelloChrome_Auto so the engine looks like a normal browser to the
// network path between you and the target.
func WithHelloID(id tls.ClientHelloID) Option {
	return func(o *engineOpts) { o.helloID = id }
}

// WithTLSConfig overrides the TLS config (useful for InsecureSkipVerify
// when probing local test servers, or for custom RootCAs).
func WithTLSConfig(c *tls.Config) Option {
	return func(o *engineOpts) { o.tlsConf = c }
}

// WithDialer overrides the underlying TCP dial. Default is net.Dialer with
// a 10s timeout.
func WithDialer(d func(context.Context, string) (net.Conn, error)) Option {
	return func(o *engineOpts) { o.dial = d }
}

// NewEngine opens a TLS+h2 connection to target, completes the HTTP/2
// preface + SETTINGS exchange, and returns a ready Engine. target must be
// an https:// URL; the path is ignored, only host:port is used.
func NewEngine(target string, opts ...Option) (*Engine, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("racing: parse target: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("racing: target must be https:// (HTTP/2 only); got %q", u.Scheme)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	plain, err := o.dial(ctx, net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("racing: dial: %w", err)
	}

	// Disable Nagle so the single big-write at gate.Send() is flushed
	// immediately as one TCP segment, not coalesced with anything that
	// happens to be in the kernel buffer afterwards.
	if tcp, ok := plain.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}

	// utls handshake with the requested parrot. The parrot's ALPN list must
	// advertise h2; HelloChrome_Auto and friends all do.
	tconn := tls.UClient(plain, o.tlsConf, o.helloID)
	if err := tconn.Handshake(); err != nil {
		plain.Close()
		return nil, fmt.Errorf("racing: tls handshake: %w", err)
	}
	if proto := tconn.ConnectionState().NegotiatedProtocol; proto != "h2" {
		tconn.Close()
		return nil, fmt.Errorf("racing: server did not negotiate h2 (got %q); single-packet attack needs HTTP/2", proto)
	}

	e := &Engine{
		target:      net.JoinHostPort(host, port),
		scheme:      "https",
		helloID:     o.helloID,
		tlsConf:     o.tlsConf,
		conn:        tconn,
		framer:      http2.NewFramer(tconn, bufio.NewReader(tconn)),
		hpackBuf:    new(bytes.Buffer),
		pending:     make(map[uint32]*streamState),
		serverSetup: make(chan struct{}),
		closed:      make(chan struct{}),
	}
	e.hpackEnc = hpack.NewEncoder(e.hpackBuf)
	e.nextSID.Store(1)

	// h2 preface + initial SETTINGS.
	if _, err := tconn.Write([]byte(clientPreface)); err != nil {
		tconn.Close()
		return nil, fmt.Errorf("racing: write preface: %w", err)
	}
	if err := e.framer.WriteSettings(); err != nil {
		tconn.Close()
		return nil, fmt.Errorf("racing: write settings: %w", err)
	}

	go e.readLoop()

	// Wait for server SETTINGS+ACK exchange to complete before letting Add
	// run — otherwise we might race the server's SETTINGS_ACK with our first
	// HEADERS frame and confuse some servers.
	select {
	case <-e.serverSetup:
	case <-e.closed:
		return nil, fmt.Errorf("racing: connection closed during setup: %w", e.closeErr)
	case <-time.After(10 * time.Second):
		tconn.Close()
		return nil, errors.New("racing: timeout waiting for server SETTINGS")
	}

	return e, nil
}

// Close releases the underlying connection. Any in-flight Gate.Send will
// return an error.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closeErr == nil {
		e.closeErr = errors.New("racing: engine closed")
	}
	select {
	case <-e.closed:
	default:
		close(e.closed)
	}
	return e.conn.Close()
}

// NewGate returns a fresh Gate bound to this engine. A Gate is single-use:
// after Send is called, create another Gate for another batch.
func (e *Engine) NewGate() *Gate {
	return &Gate{engine: e}
}

// Gate stages N requests on an Engine and releases them in one TCP packet.
type Gate struct {
	engine *Engine
	primed []*primed
	sent   bool
}

// primed holds the per-request state between Add and Send.
type primed struct {
	state *streamState
	tail  []byte // serialised final DATA frame (with END_STREAM); written at Send time
}

// Add primes one request on the gate. Writes HEADERS and all but the final
// byte of the request body to the connection. May be called many times
// before Send.
//
// The request's Body, if set, must be readable end-to-end synchronously
// from this call. Streaming bodies (chunked encoders that block on a
// channel) are not supported.
func (g *Gate) Add(req *http.Request) error {
	if g.sent {
		return errors.New("racing: gate already sent")
	}
	if req.URL == nil {
		return errors.New("racing: request URL is nil")
	}

	// Read the entire body up-front so we know its length and can split off
	// the final byte for the tail frame.
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("racing: read request body: %w", err)
		}
		req.Body.Close()
		body = b
	}

	sid := g.engine.nextSID.Add(2) - 2 // 1, 3, 5, ...
	st := &streamState{
		id:   sid,
		done: make(chan struct{}),
		req:  req,
	}

	g.engine.mu.Lock()
	g.engine.pending[sid] = st
	g.engine.mu.Unlock()

	// Encode HEADERS via HPACK.
	g.engine.writeMu.Lock()
	g.engine.hpackBuf.Reset()
	method := req.Method
	if method == "" {
		method = "GET"
	}
	path := req.URL.RequestURI()
	host := req.URL.Host
	if h := req.Header.Get("Host"); h != "" {
		host = h
	}
	if h := req.Host; h != "" {
		host = h
	}

	// Pseudo-header emission. Honour PHeaderOrderKey if the caller set one
	// (so the wire order matches a specific browser's fingerprint); fall
	// back to Chrome's default order otherwise.
	psHeaders := map[string]string{
		":method":    method,
		":authority": host,
		":scheme":    "https",
		":path":      path,
	}
	if pho := req.Header[http.PHeaderOrderKey]; len(pho) > 0 {
		seen := make(map[string]bool, len(pho))
		for _, name := range pho {
			if v, ok := psHeaders[name]; ok && !seen[name] {
				g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: name, Value: v})
				seen[name] = true
			}
		}
		// Anything not listed gets emitted in default order so a partial
		// list still produces a well-formed request.
		for _, name := range []string{":method", ":authority", ":scheme", ":path"} {
			if !seen[name] {
				g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: name, Value: psHeaders[name]})
			}
		}
	} else {
		// Chrome's pseudo-header order.
		g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: ":method", Value: method})
		g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: ":authority", Value: host})
		g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
		g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: ":path", Value: path})
	}
	if len(body) > 0 {
		g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(len(body))})
	}

	// Build the set of regular header names to emit, lower-cased, skipping
	// h2 reserved/forbidden headers, the magic ordering keys, and a
	// caller-supplied content-length (we always emit our own derived from
	// the actual body length above).
	skip := map[string]bool{
		"host":               true,
		"connection":         true,
		"transfer-encoding":  true,
		"upgrade":            true,
		"keep-alive":         true,
		"proxy-connection":   true,
		"content-length":     true,
		strings.ToLower(http.HeaderOrderKey):  true,
		strings.ToLower(http.PHeaderOrderKey): true,
	}
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		if skip[strings.ToLower(k)] {
			continue
		}
		keys = append(keys, k)
	}

	// Emit headers honouring HeaderOrderKey if present, then any leftovers
	// lexicographically (matches dhttp's writeSubset behaviour).
	emitHeader := func(k string) {
		for _, v := range req.Header[k] {
			g.engine.hpackEnc.WriteField(hpack.HeaderField{Name: strings.ToLower(k), Value: v})
		}
	}
	if order := req.Header[http.HeaderOrderKey]; len(order) > 0 {
		rank := make(map[string]int, len(order))
		for i, name := range order {
			rank[strings.ToLower(name)] = i
		}
		sort.SliceStable(keys, func(i, j int) bool {
			ri, oi := rank[strings.ToLower(keys[i])]
			rj, oj := rank[strings.ToLower(keys[j])]
			switch {
			case oi && oj:
				return ri < rj
			case oi:
				return true
			case oj:
				return false
			}
			return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
		})
	} else {
		sort.Slice(keys, func(i, j int) bool {
			return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
		})
	}
	for _, k := range keys {
		emitHeader(k)
	}
	hdrBlock := append([]byte(nil), g.engine.hpackBuf.Bytes()...)
	err := g.engine.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      sid,
		BlockFragment: hdrBlock,
		EndStream:     false, // we always send at least one DATA frame so the tail can carry END_STREAM
		EndHeaders:    true,
	})
	if err != nil {
		g.engine.writeMu.Unlock()
		return fmt.Errorf("racing: write HEADERS: %w", err)
	}

	// If body is large enough to split: write body[:-1] as a DATA frame
	// without END_STREAM, and stage body[-1:] as the tail. If body is
	// empty: stage an empty DATA-with-END_STREAM as the tail.
	var tail []byte
	switch {
	case len(body) == 0:
		// Empty DATA frame with END_STREAM. Some servers reject this, but
		// most accept; for GETs prefer a tiny dummy body or use a POST.
		tail = mustEncodeDataFrame(sid, nil, true)
	case len(body) == 1:
		// Single-byte body: skip the prefix DATA frame entirely.
		tail = mustEncodeDataFrame(sid, body, true)
	default:
		if err := g.engine.framer.WriteData(sid, false, body[:len(body)-1]); err != nil {
			g.engine.writeMu.Unlock()
			return fmt.Errorf("racing: write priming DATA: %w", err)
		}
		tail = mustEncodeDataFrame(sid, body[len(body)-1:], true)
	}
	g.engine.writeMu.Unlock()

	g.primed = append(g.primed, &primed{state: st, tail: tail})
	return nil
}

// Send releases all primed requests. Writes every tail DATA frame in a
// single Conn.Write so the kernel coalesces them into one TCP segment.
// Returns responses in the order their requests were Add-ed.
//
// Send blocks until every stream has received END_STREAM or ctx is done.
func (g *Gate) Send(ctx context.Context) ([]*http.Response, error) {
	if g.sent {
		return nil, errors.New("racing: gate already sent")
	}
	g.sent = true
	if len(g.primed) == 0 {
		return nil, errors.New("racing: gate has no primed requests")
	}

	// Concatenate all tail frames into one buffer and write in one syscall.
	// On a sane stack with TCP_NODELAY (or a small enough payload to fit in
	// one segment) this is delivered as a single TCP packet.
	var buf bytes.Buffer
	for _, p := range g.primed {
		buf.Write(p.tail)
	}

	g.engine.writeMu.Lock()
	_, err := g.engine.conn.Write(buf.Bytes())
	g.engine.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("racing: tail write: %w", err)
	}

	// Wait for every stream to complete or ctx to expire.
	resps := make([]*http.Response, len(g.primed))
	for i, p := range g.primed {
		select {
		case <-p.state.done:
			if p.state.err != nil {
				resps[i] = nil
				// Don't fail the whole gate on one stream error — let the
				// caller see partial results.
				continue
			}
			resps[i] = streamToResponse(p.state)
		case <-ctx.Done():
			return resps, ctx.Err()
		case <-g.engine.closed:
			return resps, fmt.Errorf("racing: engine closed: %w", g.engine.closeErr)
		}
	}
	return resps, nil
}

// streamToResponse builds an http.Response from a completed streamState.
func streamToResponse(st *streamState) *http.Response {
	body := bytes.NewReader(st.respBody.Bytes())
	return &http.Response{
		Status:        strconv.Itoa(st.respStatus) + " " + http.StatusText(st.respStatus),
		StatusCode:    st.respStatus,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		Header:        st.respHeaders,
		Body:          io.NopCloser(body),
		ContentLength: int64(st.respBody.Len()),
		Request:       st.req,
	}
}

// mustEncodeDataFrame serialises a DATA frame by hand. http2.Framer.WriteData
// writes directly to the underlying conn — we need the bytes so we can hold
// them back and release them in a coalesced write.
func mustEncodeDataFrame(streamID uint32, payload []byte, endStream bool) []byte {
	const frameHeaderLen = 9
	out := make([]byte, frameHeaderLen+len(payload))
	// length (24-bit)
	out[0] = byte(len(payload) >> 16)
	out[1] = byte(len(payload) >> 8)
	out[2] = byte(len(payload))
	// type = 0 (DATA)
	out[3] = 0
	// flags
	if endStream {
		out[4] = 0x1 // END_STREAM
	}
	// stream ID (31-bit, MSB reserved)
	out[5] = byte(streamID >> 24)
	out[6] = byte(streamID >> 16)
	out[7] = byte(streamID >> 8)
	out[8] = byte(streamID)
	copy(out[frameHeaderLen:], payload)
	return out
}

// readLoop reads frames from the connection and dispatches them to streams.
func (e *Engine) readLoop() {
	defer func() {
		e.mu.Lock()
		if e.closeErr == nil {
			e.closeErr = errors.New("racing: read loop exited")
		}
		select {
		case <-e.closed:
		default:
			close(e.closed)
		}
		// Fail any still-pending streams.
		for _, st := range e.pending {
			st.finish(e.closeErr)
		}
		e.mu.Unlock()
	}()

	hdec := hpack.NewDecoder(4096, nil)

	for {
		frame, err := e.framer.ReadFrame()
		if err != nil {
			e.mu.Lock()
			e.closeErr = fmt.Errorf("racing: read frame: %w", err)
			e.mu.Unlock()
			return
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			// Acknowledge server SETTINGS. We don't honour SETTINGS_MAX_FRAME_SIZE
			// etc. — assume server defaults are sane for the small frames we send.
			e.writeMu.Lock()
			err := e.framer.WriteSettingsAck()
			e.writeMu.Unlock()
			if err != nil {
				return
			}
			// Signal setup complete (idempotent under the once-close pattern).
			select {
			case <-e.serverSetup:
			default:
				close(e.serverSetup)
			}
		case *http2.PingFrame:
			if f.IsAck() {
				continue
			}
			e.writeMu.Lock()
			err := e.framer.WritePing(true, f.Data)
			e.writeMu.Unlock()
			if err != nil {
				return
			}
		case *http2.WindowUpdateFrame:
			// We currently send small request bodies (<= initial window),
			// so we ignore WINDOW_UPDATE for outgoing flow control. Receive
			// flow control: we let the connection window drift; for very
			// large responses the connection would stall. Acceptable for
			// race-testing where bodies are usually small.
		case *http2.HeadersFrame:
			e.mu.Lock()
			st := e.pending[f.StreamID]
			e.mu.Unlock()
			if st == nil {
				continue
			}
			fields, err := hdec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				st.finish(fmt.Errorf("hpack decode: %w", err))
				continue
			}
			st.respHeaders = make(http.Header)
			for _, hf := range fields {
				if hf.Name == ":status" {
					code, _ := strconv.Atoi(hf.Value)
					st.respStatus = code
					continue
				}
				if strings.HasPrefix(hf.Name, ":") {
					continue
				}
				st.respHeaders.Add(hf.Name, hf.Value)
			}
			if f.StreamEnded() {
				st.finish(nil)
			}
		case *http2.DataFrame:
			e.mu.Lock()
			st := e.pending[f.StreamID]
			e.mu.Unlock()
			if st == nil {
				continue
			}
			st.respBody.Write(f.Data())
			if f.StreamEnded() {
				st.finish(nil)
			}
		case *http2.RSTStreamFrame:
			e.mu.Lock()
			st := e.pending[f.StreamID]
			e.mu.Unlock()
			if st != nil {
				st.finish(fmt.Errorf("racing: server reset stream: code %v", f.ErrCode))
			}
		case *http2.GoAwayFrame:
			e.mu.Lock()
			e.closeErr = fmt.Errorf("racing: server GOAWAY: code %v, last stream %d, debug %q",
				f.ErrCode, f.LastStreamID, string(f.DebugData()))
			e.mu.Unlock()
			return
		}
	}
}
