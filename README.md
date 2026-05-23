# DHTTP
A fork of github.com/golang/go/src/net/http

- For HTTP/1.1 and HTTP/2
- Arbitrary header and pseudo-header ordering
- Client-side TLS ClientHello parroting via [utls](https://github.com/refraction-networking/utls)
- Tracks upstream Go via a patch-series workflow

## Install
```go
import (
    http "github.com/dteh/dhttp"
)
```

Version-pin to match your Go release:
```
go get github.com/dteh/dhttp@v1.26  // latest tracking Go 1.26.x
go get github.com/dteh/dhttp@v1.25  // latest tracking Go 1.25.x
go get github.com/dteh/dhttp@v1.24  // latest tracking Go 1.24.x
```
See [the Releases page](https://github.com/dteh/dhttp/releases) for the version compatibility matrix.

## Public API additions

### `ClientHelloSettings` on `Transport`
```go
type ClientHelloSettings struct {
    HelloID  tls.ClientHelloID    // from refraction-networking/utls
    Override tls.ClientHelloSpec
}
```
Set `HelloID` to the desired parrot (e.g. `tls.HelloChrome_Auto`). For a custom spec, set `HelloID = tls.HelloCustom` and provide `Override`. utls is a client-side fingerprinting tool only — servers gain nothing from it; the fork does not extend server-side TLS.

### Header ordering magic keys
```go
const HeaderOrderKey  = "Header-Order:"   // HTTP/1.1 + HTTP/2 header order
const PHeaderOrderKey = "PHeader-Order:"  // HTTP/2 pseudo-header order (:authority, :method, :path, :scheme)
```
Populate `req.Header[HeaderOrderKey]` with a lower-cased list of header names; the wire ordering follows the slice. Unlisted headers go after, lexicographically.

## How this repo is built

Generated Go source is committed (so `go get` works directly), but the *source of truth* is `patches/*.patch` applied on top of vanilla upstream Go. Reviewers should read patch diffs as the real change; the regenerated Go files are machine output.

Layout:
```
patches/        # dhttp's divergence from vanilla net/http
  series        # patch application order
overlay/        # fork-only files (no vanilla counterpart) that override the generated tree
scripts/
  build.sh      # regenerate the module from upstream + patches/ + overlay/
  upgrade.sh    # bump UPSTREAM_TAG, rebuild, surface rejects
  forward_rewrite.py  # import-path rewrites: net/http -> github.com/dteh/dhttp
UPSTREAM_TAG    # pinned upstream Go tag (e.g. go1.26.3)
INTERNAL_DEPS   # src/internal/* and src/net/http/internal/* packages vendored at build time
```

## Regenerating after an upstream Go release

```bash
scripts/upgrade.sh go1.26.3      # or whatever the new tag is
# Patches that fail to apply cleanly leave .rej files in the scratch tree;
# refresh each by hand and re-run.
go build ./...
go test ./...                    # baseline diff vs .baseline_failures.txt
```

`scripts/build.sh` is idempotent — re-running it over an already-built tree produces the same output.

## Reproduce this repo from scratch

The patch series + overlay + scripts are self-contained. Clone, then:
```bash
scripts/build.sh
```
Output is committed to the repo; you only need to run `build.sh` if you change `patches/`, `overlay/`, or `UPSTREAM_TAG`.

## Test strategy

Many upstream `net/http` tests assume Google CI infrastructure (LUCI buildlets, internal endpoints, specific timing). Google's CI runner is not publicly accessible. We don't try to fix every upstream test — instead:

- **Canary tests** (must pass on every upgrade): `TestHeaderOrder*`, `TestPHeaderOrder*`, `TestALPN*`, `TestDhttp*`. dhttp-specific; any failure is a real regression.
- **Baseline diff**: `.baseline_failures.txt` (gitignored) holds known failures on master; new failures after an upgrade are the signal.
- **`.test_skip.txt`** (committed): pre-existing failures excluded from the diff with one-line reasons.
- **JA3/JA4 smoke**: manually hit `tls.peet.ws/api/all` with `HelloChrome_Auto` and compare against `.fingerprint-baseline.json` (gitignored).
