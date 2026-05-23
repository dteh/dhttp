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

### Version compatibility

| Your Go version | Use dhttp     | Notes                                        |
|-----------------|---------------|----------------------------------------------|
| 1.26.x          | `v1.26.0`     | utls v1.8.2; tracks `go1.26.3`               |
| 1.25.x          | `v1.25.0`     | utls v1.8.2; tracks `go1.25.10`              |
| 1.24.x          | `v1.24.1`     | utls v1.8.2; tracks `go1.24.13` (`v1.24.0` has a `Content-Length` stripping bug — use `v1.24.1`) |

See [the Releases page](https://github.com/dteh/dhttp/releases) for the full history; each release notes the exact upstream Go tag and utls version.

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
const PHeaderOrderKey = "PHeader-Order:"  // HTTP/2 pseudo-header order
```
Populate `req.Header[HeaderOrderKey]` with a lower-cased list of header names; the wire ordering follows the slice. Unlisted headers go after, lexicographically. Several normally-implicit headers (`User-Agent`, `Content-Length`, `Transfer-Encoding`, `Trailer`, `Accept-Encoding`) are orderable via this mechanism.

`PHeaderOrderKey` accepts `:authority`, `:method`, `:path`, `:scheme`, and `:protocol` (for extended-CONNECT). Unknown pseudo-headers in the list are skipped.

## How this repo is built

Generated Go source is committed (so `go get` works directly), but the *source of truth* is `patches/*.patch` applied on top of vanilla upstream Go. Reviewers should read patch diffs as the real change; the regenerated Go files are machine output.

Layout:
```
patches/                # dhttp's divergence from vanilla net/http
  series                # patch application order (one patch currently)
_overlay/                # fork-only files that override the generated tree after patches apply
scripts/
  build.sh              # regenerate the module from upstream + patches/ + _overlay/
  upgrade.sh            # bump UPSTREAM_TAG, rebuild, surface rejects
  forward_rewrite.py    # import-path rewrites: net/http -> github.com/dteh/dhttp
UPSTREAM_TAG            # pinned upstream Go tag (e.g. go1.26.3)
INTERNAL_DEPS           # src/internal/* and src/net/http/internal/* packages vendored at build time
.test_skip.txt          # baseline failures excluded from upgrade-diff with one-line reasons
```

## Regenerating after an upstream Go release

```bash
scripts/upgrade.sh go1.26.4      # or whatever the new tag is
go build ./...
go test ./...                    # diff failures against .baseline_failures.txt
```

`scripts/build.sh` is idempotent — re-running it over an already-built tree produces the same output.

If a patch fails to apply (common on minor-version bumps; rare on patch releases), `upgrade.sh` leaves `.rej` files in `~/.cache/dhttp-build/`'s scratch tree and restores `UPSTREAM_TAG`. Refresh strategies:

- **Small rejects (import-block shifts, signature tweaks)**: edit the patch-source tree by hand, regenerate the combined patch with `diff -Naur` against the new vanilla tree, and re-run `scripts/build.sh`.
- **A single file with massive rejects (e.g. `transport.go` or `h2_bundle.go` after an upstream restructure)**: start from clean vanilla for that file and re-apply dhttp's mods on top, rather than three-way merging.

`h2_bundle.go` is regenerated wholesale by upstream every minor release, so its line numbers always shift; since Go 1.25 most HTTP/2 header-ordering logic lives in `internal/httpcommon/` instead.

Bumping utls: `go get github.com/refraction-networking/utls@v1.X.Y && go mod tidy`. Always re-capture the JA3/Peetprint reference afterwards — `HelloChrome_Auto` will pick a different Chrome version, which is intended but means existing consumers see a fingerprint shift.

## Reproduce this repo from scratch

The patch series + overlay + scripts are self-contained. Clone, then:
```bash
scripts/build.sh
```
Output is committed to the repo; you only need to run `build.sh` if you change `patches/`, `_overlay/`, or `UPSTREAM_TAG`.

The overlay directory is named `_overlay` (with the leading underscore) so Go tooling ignores it — its files declare `package http` but live in their own directory, which would otherwise trip `go vet` / gopls.

## Test strategy

Many upstream `net/http` tests assume Google CI infrastructure (LUCI buildlets, internal endpoints, specific timing, GODEBUG runtime overrides). Google's CI runner is not publicly accessible to forks. We don't try to fix every upstream test — instead:

- **Canary tests** (must pass on every upgrade): `TestHeaderOrderHTTP1`, `TestHeaderOrderHTTP2`, `TestFileServerMethods`, `TestEarlyHintsRequest`, `TestTrailersServerToClient`, `TestUserAgentMissingHeader`, `TestWriteSubsetConcurrentHeaderWrite`. dhttp-specific or recovered-via-fix; any failure is a real regression.
- **Baseline diff**: `.baseline_failures.txt` (gitignored) holds known failures on master; new entries after an upgrade are the signal.
- **`.test_skip.txt`** (committed): pre-existing failures excluded from the diff with one-line reasons. Stripped from both sides of the diff before reporting.
- **JA3/Peetprint smoke**: manually hit `tls.peet.ws/api/all` with `HelloChrome_Auto` and compare against the previous baseline. Compare **Peetprint**, not JA3 — `HelloChrome_Auto` uses randomized GREASE extension values so the JA3 hash varies run-to-run. Peetprint excludes the randomized fields and is stable across runs for the same utls version.
