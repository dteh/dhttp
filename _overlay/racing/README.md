# racing

HTTP/2 single-packet attack + HTTP/1.1 last-byte sync primitives for
dhttp. Modelled on PortSwigger's "engine + gate" pattern:
https://portswigger.net/web-security/race-conditions

## Two techniques

| Technique | Server-side skew | Connections | Use when |
|-----------|------------------|-------------|----------|
| **`Engine` / `Gate`** — h2 single-packet | sub-100µs (typical) | 1 TLS conn, N streams | Target supports h2 (most modern targets) |
| **`H1Engine` / `H1Gate`** — h1 last-byte sync | ~ms (typical) | N TLS conns | Target only speaks h1; cross-validation; servers with h2 stream serialisation |

Both share the same gate-style API — only the constructor changes.

## What it does

Stage N HTTP/2 requests on one connection, then release them in a single
TCP packet. Server's epoll wakes once and dispatches all N requests inside
the same scheduling tick — typically sub-100µs end-to-end skew. Useful for
exploiting TOCTOU race conditions in web applications (token reuse, coupon
double-redeem, balance checks, etc.) during authorized security testing.

## How

For each request the engine writes the HEADERS frame and all-but-the-final
byte of the DATA frame immediately. The single byte that carries
`END_STREAM` is held back. When you call `Gate.Send()`, the held-back tail
frames are concatenated into one buffer and flushed in a single
`conn.Write`. Frame size is ~10 bytes (9-byte header + 1-byte payload) so
30 streams = 300 bytes — comfortably under MTU, kernel ships it as one
segment.

## Usage

```go
import (
    "context"
    "strings"
    "time"

    http "github.com/dteh/dhttp"
    "github.com/dteh/dhttp/racing"
    tls "github.com/refraction-networking/utls"
)

eng, err := racing.NewEngine("https://target.example.com",
    racing.WithHelloID(tls.HelloChrome_Auto))
if err != nil { panic(err) }
defer eng.Close()

g := eng.NewGate()
for i := 0; i < 30; i++ {
    req, _ := http.NewRequest("POST", "https://target.example.com/redeem",
        strings.NewReader(`{"code":"ABC"}`))
    req.Header.Set("Content-Type", "application/json")
    g.Add(req)
}

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
resps, err := g.Send(ctx)
// resps is in the same order as Add() calls
```

## H1Engine usage

Identical shape — one constructor change:

```go
eng, err := racing.NewH1Engine("https://target.example.com")
// ...everything else identical to Engine...
```

`H1Engine` always negotiates HTTP/1.1, even against h2-capable servers
(strips `h2` from the parrot's ALPN list before the handshake). Use it
when you suspect server-side h2 stream serialisation is widening h2's
effective skew, or against h1-only targets.

## Empty-body requests

For requests **with** a body, the engine holds back the final body byte
as the END_STREAM tail (one 10-byte DATA frame per stream).

For requests **without** a body (GET, body-less POST/DELETE/etc.), the
tail is an empty DATA frame with END_STREAM (9-byte frame header, zero
payload). All major HTTP/2 implementations accept this (nginx, Apache,
Go `net/http`, IIS, h2load, curl), but a few overly-pedantic h2 stacks
reject empty-body DATA frames as malformed. If you hit one:

- Use a body-bearing method (POST with a 1-byte body) instead, OR
- Fall back to `H1Engine` — its tail is a single header-terminator byte,
  which every server accepts.

The empty DATA frame is a deliberate choice: alternatives (sending
END_STREAM on the HEADERS frame itself) would put a 100-500 byte HEADERS
frame in the tail, which can exceed MTU when staged 30+ streams deep —
defeating the single-packet attack's whole point.

## Header ordering

Set `req.Header[http.HeaderOrderKey]` with a slice of lower-cased header
names to control wire-order; unlisted headers go after, lexicographically.
Set `req.Header[http.PHeaderOrderKey]` for pseudo-header order (defaults
to Chrome's `:method, :authority, :scheme, :path`). Both keys are stripped
from the wire automatically.

## Constraints

- **`Engine` is HTTP/2 only.** For HTTP/1.1-only targets use `H1Engine`.
- **TLS only** — neither engine supports cleartext h2c / plain h1.
- **Small bodies preferred** — bodies must fit in the initial 65535-byte
  per-stream window. Neither engine currently sends `WINDOW_UPDATE`s
  for upstream flow control. Race-testing payloads are usually tiny so
  this is fine.
- **One Gate per batch** — Gate is single-use. Create a new gate via
  `engine.NewGate()` for each attack burst.
- **One Engine per target** — `Engine` owns one TLS+h2 connection;
  `H1Engine` is connection-less itself but each `H1Gate.Add` opens a
  fresh conn.
- **H2 SETTINGS sent are Go h2 stack defaults**, not Chrome's. Defenders
  who fingerprint h2 SETTINGS (some Akamai/Cloudflare configurations
  do) will see a non-browser shape. The TLS ClientHello via utls is
  still Chrome.

## Verifying the packet actually coalesces

The engine sets `TCP_NODELAY` on the underlying socket and writes all tail
frames in a single `Conn.Write`, so the kernel should ship them as one
TCP segment. To verify on the wire:

```
sudo tcpdump -i any -nn -X -s 0 'tcp port 443 and host target.example.com'
```

You should see one segment containing all N tail frames (each is 10 bytes
for a single-byte body, 9 bytes for an empty body). On a target with
race-condition detection instrumentation (e.g., a contrived endpoint that
logs request arrival nanoseconds), you can also measure server-side skew.

## What's not here yet

- Connection warm-up / ping-pong to lower RTT before the attack
- Per-stream priority hints
- Streaming request bodies
- Receive-side flow control (`WINDOW_UPDATE` for large responses)
- Browser-matched h2 SETTINGS (currently sends Go h2 stack defaults)
- HPACK encoder concurrency safety across multiple Gates on one Engine
  (single-threaded per Gate is fine; cross-Gate is not)

PRs welcome.

## Authorized use only

This is a security-testing tool. Use it against systems you own or have
explicit permission to test. Race-condition exploitation can cause data
corruption, financial loss, and service disruption — exactly the kinds of
bugs you're looking for, but only in scope.
