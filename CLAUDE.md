# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

`dhttp` is a fork of Go's standard `net/http` package (from `github.com/golang/go/src/net/http`). It is intended to be a near drop-in replacement that adds two capabilities the upstream package does not support:

1. **Arbitrary HTTP header ordering** (both HTTP/1.1 header order and HTTP/2 pseudo-header order).
2. **TLS ClientHello parroting** via [refraction-networking/utls](https://github.com/refraction-networking/utls), so requests can mimic Chrome / Firefox / Safari fingerprints.

It is imported as `http "github.com/dteh/dhttp"` so existing call sites work unchanged.

## Build and test

This is a library; there is no binary. Use standard Go tooling from the repository root:

```bash
go build ./...
go test ./...
go test -run TestNameRegex ./...            # run a single test
go test -run TestHeaderWrite -v             # verbose single test in the root package
go test -race ./...                          # race detector
```

The `nethttpomithttp2` build tag (see `omithttp2.go`) disables HTTP/2 support; most work should be done without it.

The `upstream/` directory is a checkout of vanilla `net/http` used only for diffing against upstream during merges (see README's "pulling upstream changes" section). It is **not** a buildable part of this module — don't edit it and don't include it in test runs.

## Architecture

Because this is a fork of `net/http`, the overall layout mirrors the standard library. The pieces that matter for this fork:

### Header ordering (`header.go`, plus consumers in `transport.go`, `h2_bundle.go`, `request.go`)

Two magic header keys are recognised on `Request.Header` / `ResponseWriter.Header`:

- `HeaderOrderKey = "Header-Order:"` — slice of lower-cased header names defining the wire order. Keys not listed go after the ordered ones, lexicographically.
- `PHeaderOrderKey = "PHeader-Order:"` — HTTP/2 pseudo-header order. Valid entries are `:authority`, `:method`, `:path`, `:scheme`.

These keys are stripped before the headers hit the wire (`h2_bundle.go:9487`, `transport.go:579`, `response.go:29-30`). Several normally-implicit headers (`User-Agent`, `Content-Length`, `Transfer-Encoding`, `Trailer`, `Accept-Encoding`) have been made orderable — recent commits (`81807d0`, `36b8d30`, `71fd5bb`) fixed cases where `transferWriter` was overwriting caller-provided values; preserve that behaviour when touching those paths.

### TLS parroting (`transport.go`)

`Transport` has an added field:

```go
ClientHelloSettings ClientHelloSettings   // transport.go:322
type ClientHelloSettings struct {          // transport.go:2173
    HelloID  tls.ClientHelloID             // from refraction-networking/utls
    Override tls.ClientHelloSpec
}
```

Behaviour (`transport.go:~1709-1725`):
- If `HelloID` is unset, defaults to `HelloChrome_Auto`.
- If the caller passes a `HelloID` other than `HelloCustom`, that fingerprint is used directly.
- If `HelloID == HelloCustom`, `Override` is used as the ClientHelloSpec.
- The dialer returns a `*utls.UConn`, not a `*crypto/tls.Conn` — so anywhere the upstream package assumes `*tls.Conn`, this fork uses the utls drop-in (`tls "github.com/refraction-networking/utls"` at the top of files that touch TLS).

The settings flow from `Transport` into `persistConn.clientHelloSettings` at `transport.go:1804`.

### HTTP/2 bundle (`h2_bundle.go`)

`golang.org/x/net/http2` is vendored into a single ~400k-line file (`h2_bundle.go`) the same way upstream does it. The header-order logic for HTTP/2 lives in this file (search for `HeaderOrderKey` / `PHeaderOrderKey` around lines 9487, 9575, 9615, 9678). When merging upstream `http2` changes, expect to re-apply those hunks.

### Import rewrites

This fork rewrites the upstream import paths:

- `net/http` → `github.com/dteh/dhttp`
- `internal/...` → `github.com/dteh/dhttp/internal/...`

The README documents the regex used. Apply the same rewrites whenever pulling upstream code in.

## Working with upstream merges

The README's "pulling upstream changes" section is the source of truth. The flow is:

1. `git filter-repo` a fresh `golang/go` checkout into `./upstream/`.
2. Add it as a local remote and `git merge upstream-local/master`.
3. Re-apply the import-path rewrites and re-check the dhttp-specific patches (header ordering, utls dialer, `ClientHelloSettings`) since they conflict often.

Tags like `// [dhttp]` in comments mark fork-specific additions — preserve them.
