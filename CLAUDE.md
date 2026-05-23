# CLAUDE.md

Guidance for Claude Code working in this repository.

## Repository overview

`dhttp` is a hard fork of Go's `net/http` (specifically `src/net/http/`) that adds two capabilities upstream doesn't support:

1. **Arbitrary HTTP header ordering** — HTTP/1.1 header order and HTTP/2 pseudo-header order, controlled by magic keys on `Request.Header`.
2. **Client-side TLS ClientHello parroting** via [refraction-networking/utls](https://github.com/refraction-networking/utls), letting requests mimic Chrome / Firefox / Edge / iOS fingerprints.

Imported as `http "github.com/dteh/dhttp"`. Server-side functionality should not diverge from upstream beyond what's strictly required — utls is a client-only fingerprinting tool, so server-side TLS extensions are explicitly out of scope.

## Layout

The repo holds **both** the generated module source (so `go get` works) AND the patch-series + scripts used to regenerate it from a new upstream Go release:

```
                                              ─── consumer-facing ───
*.go, cgi/, cookiejar/, fcgi/, httptest/,    │  All generated. Each file has
httptrace/, httputil/, internal/, pprof/     │  // Code generated from patches/. DO NOT EDIT.
go.mod, go.sum                                │  Committed so `go get` works directly.

                                              ─── developer-facing ───
patches/0001-dhttp-combined.patch            │  dhttp's divergence from vanilla net/http.
patches/series                               │  Patch application order (currently 1 patch).
_overlay/                                     │  Fork-only files that override the generated tree
                                             │  after patches apply: header_order_test.go,
                                             │  dhttp_client_test.go, utls_client.go.
scripts/build.sh                             │  Regenerate the module from upstream + patches + overlay.
scripts/upgrade.sh <new-tag>                 │  Bump UPSTREAM_TAG and rebuild; surfaces rejects.
scripts/forward_rewrite.py                   │  `net/http` → `github.com/dteh/dhttp` rewrites.
UPSTREAM_TAG                                 │  One line: the pinned upstream Go tag (e.g. go1.26.3).
INTERNAL_DEPS                                │  src/internal/* and src/net/http/internal/* packages
                                             │  vendored into internal/ at build time.
.test_skip.txt                               │  Tests excluded from baseline-diff with reasons.
```

The generated `.go` files at the repo root and under the subpackage directories are **machine output**. Don't hand-edit them — changes will be wiped on the next `scripts/build.sh` run. Edit `patches/0001-dhttp-combined.patch` (or the patch-source tree in `../dhttp-build/work/`) instead.

## Build and test

```bash
go build ./...                                              # build all dhttp packages
go test -count=1 -timeout 10m ./...                         # full test suite (~5 min)
go test -count=1 -run TestHeaderOrderHTTP1 -v .             # one test
go test -race ./...                                         # race detector
```

The `_overlay/` directory contains files with `package http`. The leading underscore is deliberate — Go tooling (`go build`, `go vet`, gopls) skips directories whose names start with `_`, which avoids package-name-vs-directory-name complaints. Treat `_overlay/` as inert source storage; the files are only ever compiled after `scripts/build.sh` copies them into the repo root.

### Test strategy: baseline-diff + canaries

Many upstream `net/http` tests assume Google CI infrastructure (LUCI buildlets, internal endpoints, specific timing, GODEBUG runtime overrides). They were never expected to pass cleanly outside Google's CI. We don't try to fix them.

- **Canary tests (hard gate)** — `TestHeaderOrder*`, `TestPHeaderOrder*`, `TestFileServer*`, `TestEarlyHintsRequest`, `TestTrailersServerToClient`, `TestUserAgentMissingHeader`, `TestWriteSubsetConcurrentHeaderWrite`. dhttp-specific or recovered-via-fix; any failure is a real regression.
- **Baseline-diff (informational)** — generate `.baseline_failures.txt` from a fresh `go test` run, then compare per-upgrade. New names = upgrade-caused; investigate.
- **`.test_skip.txt`** (committed) — pre-existing failures bucketed by cause with one-line reasons. Strip from both sides of the baseline diff before reporting.
- **JA3/Peetprint smoke** — run a small client adapted from `dhttp_client_test.go` against `tls.peet.ws/api/all` with `HelloChrome_Auto`. Peetprint is the stable comparison; JA3 hash varies run-to-run because Chrome's GREASE extension values are randomized. A small reproducer lives in `/tmp/dhttp-fp/` from past sessions; recreate if needed.

## Architecture

### Header ordering

Two magic keys on `Request.Header`:
- `HeaderOrderKey = "Header-Order:"` — lower-cased header names defining wire order. Unlisted keys go after, lexicographically.
- `PHeaderOrderKey = "PHeader-Order:"` — HTTP/2 pseudo-header order. Valid entries: `:authority`, `:method`, `:path`, `:scheme`, `:protocol`.

Both keys are stripped before headers hit the wire. Line numbers shift every upgrade — use these grep anchors:
- Magic-key constants and `headerOrderContains` / `contains` / `writeSubset` patches: `grep -n "HeaderOrderKey" header.go`
- HTTP/1.1 request write integration: `grep -n "twHeaders\|HeaderOrderKey" request.go`
- HTTP/2 hooks (since Go 1.25): `grep -n "HeaderOrderKey\|PHeaderOrderKey\|pHeaderOrder" internal/httpcommon/httpcommon.go`
- Response-side exclusions: `grep -n "HeaderOrderKey" response.go`

Several normally-implicit headers (`User-Agent`, `Content-Length`, `Transfer-Encoding`, `Trailer`, `Accept-Encoding`) have been made orderable. Preserve that behaviour when touching those paths — see commits `71fd5bb`, `36b8d30`, `81807d0`, and **especially the Content-Length fix in `6094c30`** which the orderable rework regressed.

### TLS parroting

`Transport.ClientHelloSettings` exposes the parrot:
```go
type ClientHelloSettings struct {
    HelloID  tls.ClientHelloID    // from refraction-networking/utls
    Override tls.ClientHelloSpec  // used if HelloID == HelloCustom
}
```

Defaults to `HelloChrome_Auto` when unset. Find the touch points:
- Struct + field: `grep -n "ClientHelloSettings" transport.go`
- Dialer / utls.UClient handshake: `grep -n "tls.UClient\|HelloID" transport.go`
- ALPN ordering note for accurate parrots: `grep -n "removeH2FromParrotSpec" transport.go`
- Unencrypted h2 client conn: `_overlay/utls_client.go` (fork-only file)

**Server-side files do not need utls** for the parroting feature, but they DO use the utls import because utls's `tls.Config`, `tls.Conn`, `tls.ConnectionState`, `tls.Certificate` types are **NOT** type aliases of `crypto/tls`'s equivalents — they're independent struct definitions. Mixing crypto/tls and utls types breaks at the `Server.TLSConfig` / `Server.TLSNextProto` boundary because of this. All 18 source files that touch `tls.X` use the utls import.

### HTTP/2

`golang.org/x/net/http2` is vendored into `h2_bundle.go` (~10–13k lines), regenerated wholesale by upstream every Go minor. Most HTTP/2 header-encoding logic moved out of `h2_bundle.go` into `net/http/internal/httpcommon` in Go 1.25. The dhttp HeaderOrder hooks for HTTP/2 now live in `internal/httpcommon/httpcommon.go` — search there, not `h2_bundle.go`, when modifying HTTP/2 ordering behaviour.

### Import rewrites

The build script rewrites vanilla net/http imports to dhttp module paths:
- `"net/http"` → `http "github.com/dteh/dhttp"` (named import)
- `. "net/http"` → `. "github.com/dteh/dhttp"` (dot import, in test files)
- `"net/http/<sub>"` → `"github.com/dteh/dhttp/<sub>"` (cgi, cookiejar, fcgi, httptest, httptrace, httputil, pprof, internal, internal/ascii, internal/httpcommon, internal/testcert)
- `"internal/<X>"` → `"github.com/dteh/dhttp/internal/<X>"` (bisect, cfg, diff, goarch, godebug, godebugs, goexperiment, nettrace, platform, profile, race, synctest, testenv, txtar)

Implemented in `scripts/forward_rewrite.py`. The regex skips comments and only matches inside import statements.

## Maintenance: upgrading to a new Go release

### Normal patch release (e.g. go1.26.3 → go1.26.4)

```bash
git checkout -b upgrade/go1.26.4
scripts/upgrade.sh go1.26.4              # bumps UPSTREAM_TAG, runs build, surfaces rejects
```

If `upgrade.sh` succeeds clean (typical for patch releases):
```bash
go build ./...
go test -count=1 -timeout 10m ./... 2>&1 | tee step.txt
grep '^--- FAIL' step.txt | awk '{print $3}' | sort -u > .step_failures.txt
diff .baseline_failures.txt .step_failures.txt    # should be empty or close to it
go test -count=1 -run 'TestHeaderOrder|TestFileServerMethods|TestEarlyHintsRequest|TestTrailersServerToClient' .
# JA3 smoke if utls bumped (otherwise Peetprint should be unchanged)
```

### Minor release (e.g. go1.26.x → go1.27.0) — expect patch rejects

When `scripts/upgrade.sh` fails with rejects, two strategies in order:

**(a) Small rejects (import-block changes, signature tweaks)** — edit the patch-source tree by hand:
```bash
cd ../dhttp-build/work
# Build vanilla-flat-NEW if not already present:
mkdir -p vanilla-flat-NEW && rsync -a ../dhttp-build/work/upstream-NEW/src/net/http/ vanilla-flat-NEW/
# (Copy each INTERNAL_DEPS package into vanilla-flat-NEW/internal/<base>/)

# Apply upstream delta to the previous-version dhttp-tree:
cp -a dhttp-tree-PREV dhttp-tree-NEW
cd dhttp-tree-NEW
diff -Naur ../vanilla-flat-PREV ../vanilla-flat-NEW > /tmp/delta.patch
patch -p1 -F3 < /tmp/delta.patch    # accept fuzz; resolve any .rej files manually

# Regenerate combined patch:
cd ..
diff -Naur --exclude='*.rej' --exclude='.claude' --exclude='README.md' \
  --exclude='go.mod' --exclude='go.sum' --exclude='CLAUDE.md' \
  vanilla-flat-NEW dhttp-tree-NEW | sed -E 's|vanilla-flat-NEW/|a/|g; s|dhttp-tree-NEW/|b/|g' \
  > /path/to/dhttp/patches/0001-dhttp-combined.patch
cd /path/to/dhttp
echo go1.X.Y > UPSTREAM_TAG
bash scripts/build.sh
```

**(b) Huge rejects on one file (e.g. 22/23 hunks on `transport.go`)** — start from clean vanilla, re-apply dhttp's mods:
```bash
cd ../dhttp-build/work
# Compute dhttp's mods to this file relative to previous Go:
diff -u vanilla-flat-PREV/transport.go dhttp-tree-PREV/transport.go > /tmp/dhttp-mods.patch
# Overwrite scratch with vanilla, apply mods:
cp vanilla-flat-NEW/transport.go dhttp-tree-NEW/transport.go
cd dhttp-tree-NEW
patch -F5 transport.go < /tmp/dhttp-mods.patch
# Fix any remaining rejects by hand; regenerate combined patch as in (a).
```

This was needed for `transport.go` in the go1.25.10 → go1.26.3 upgrade.

### What to watch for at each upgrade

- **New `src/internal/*` deps** — upstream sometimes adds a new transitive dep (e.g. `internal/goexperiment` in 1.26). Build fails with `use of internal package internal/X not allowed`. Add the new package to BOTH `INTERNAL_DEPS` AND `scripts/forward_rewrite.py`'s regex. They got out of sync once (see `c519fa1`).
- **New `.go` files in `src/net/http/`** — show up in the combined patch and vendor automatically. Examples: `csrf.go` in 1.25, `clientconn.go` in 1.26. No special handling unless dhttp wants to modify them.
- **Function restructures** — when upstream moves a function we patch (e.g. `encodeHeaders` → `httpcommon` in 1.25), vendor the new home and re-implement the dhttp hooks there.
- **synctest signature drift** — when test build fails with `cannot use func(t *testing.T,...) as func(t testing.TB,...)`, `sed 's|*testing.T,|testing.TB,|g'` on the affected test functions.
- **`h2_bundle.go`** — regenerated wholesale upstream every minor. Line numbers always shift. Since 1.25 most ordering logic moved to httpcommon; h2_bundle.go now needs less attention.

### Upgrading utls

```bash
go get github.com/refraction-networking/utls@v1.X.Y
go mod tidy
```

**Always re-baseline the JA3/Peetprint** after a utls bump — `HelloChrome_Auto` will pick a different Chrome version, which is the intended behaviour but means existing consumers see a fingerprint shift. Note in the release notes (see v1.24.0 notes for the v1.6.7 → v1.8.2 bump pattern).

## Release tagging

Scheme: `v1.<go-minor>.<dhttp-patch>` — dhttp's minor mirrors Go's minor; dhttp's patch increments when Go ships a new patch *or* when dhttp itself ships a fork-only fix.

Examples from history:
| Event | UPSTREAM_TAG | dhttp tag |
|-------|--------------|-----------|
| First dhttp release on Go 1.24.x | `go1.24.13` | `v1.24.0` |
| Content-Length fix (fork-only) | `go1.24.13` | `v1.24.1` |
| First dhttp release on Go 1.25.x | `go1.25.10` | `v1.25.0` |
| First dhttp release on Go 1.26.x | `go1.26.3` | `v1.26.0` |

Tag and release flow after the PR lands on master:
```bash
git tag -a v1.X.Y -m "dhttp on goN.N.N, utls vX.Y.Z" COMMIT
git push origin master v1.X.Y
gh release create v1.X.Y --title "v1.X.Y — Go N.N.N + utls vX.Y.Z" --latest --notes "..."
```

Release notes always include: upstream Go tag, utls version, `go.mod` directive, supported Go-version range, changes since previous, and (if applicable) a callout to a newer release that supersedes this one for known bugs. The Go module proxy serves the tagged source automatically — no separate artifact upload needed.

## Gotchas (the concrete list)

1. **utls's `tls.X` types are NOT type aliases of `crypto/tls`'s equivalents.** They're independent struct definitions. Mixing crypto/tls and utls types in the same call chain causes `cannot use *utls.Config as *crypto/tls.Config` errors. The Ultraplan got this wrong; the full utls import swap (all 18 client+server files) is required.
2. **`addedGzip` strips Content-Length on every response if you don't guard on Content-Encoding.** dhttp originally extended the auto-decompression from gzip-only to gzip+br+deflate+zstd but dropped the `ce == "gzip"` guard that vanilla net/http uses before stripping. Result: plain (uncompressed) responses lost their `Content-Length` header, breaking length-aware consumers. Fixed in `6094c30` for both transport.go (h1) and h2_bundle.go (h2). Keep the four-codec guard whenever modifying that path. Canary: `TestFileServerMethods`.
3. **`INTERNAL_DEPS` and `scripts/forward_rewrite.py` must stay in sync.** When you add a package to one, add it to the other. Build will fail with `use of internal package internal/X not allowed` if you copy the package but forget to rewrite its imports.
4. **`httpcommon.validateHeaders` must skip the magic keys.** The HeaderOrderKey/PHeaderOrderKey strings contain `:` which fails `httpguts.ValidHeaderFieldName`. Without the skip, `httpcommon.EncodeHeaders` returns `invalid HTTP header name "Header-Order:"`. Same for `transport.go`'s `validateHeaders`.
5. **Overlay files override generated files.** A file in `_overlay/` that has the same basename as a generated file replaces the generated version after the build. Useful for wholesale overrides (the synctest test file is mostly commented out via this pattern); dangerous if you forget you have an overlay and edit the generated file. The build script always copies overlay last.
6. **macOS bash is 3.2.** `mapfile` and `readarray` don't exist; `scripts/build.sh` uses `while read` loops instead. Don't add bashisms that need 4.x.
7. **The generated-file banner must NOT precede `//go:build` directives.** Go's build-tag parsing requires `//go:build` to be at the top of the file (modulo comment lines). `scripts/build.sh` stamps the banner before `package`, after any leading comments — but if you ever stamp before `//go:build`, the tag is ignored and you get spurious cross-compilation breakage. The `internal/goexperiment` files in particular all use `//go:build !goexperiment.X` and were the first to hit this in step 6.
8. **`_overlay/dhttp_client_test.go` and `_overlay/header_order_test.go` MUST be in dhttp-import form, not vanilla.** They're copied verbatim into the repo root with no import rewriting. If you stage them from the reverse-rewrite tree (which has vanilla imports), they'll have `"net/http/httptrace"` instead of `"github.com/dteh/dhttp/httptrace"` and the package won't build. Always source overlay files from the dhttp repo's actual master state, not from intermediate working trees.
9. **`server.go`'s `unencryptedTLSConn` (no U) stays in server.go; `unencryptedUTLSConn` (with U) is in `_overlay/utls_client.go`.** Both exist; the difference is one letter and one is a client-only function. Don't accidentally delete the wrong one when refreshing patches.
10. **GODEBUG-runtime-override-dependent tests don't work in the fork.** The vendored `internal/godebug` package can't propagate runtime overrides set via `t.Setenv("GODEBUG", ...)`. Affects 5 tests (`TestParseCookie`, `TestReadCookies`, `TestReadSetCookies`, `TestFileServerErrorMessages`, `TestServeContentHeadersWithError`). The features themselves work with package-default values — only the per-test override mechanism fails. All listed in `.test_skip.txt`.

## Common search anchors

When you need to find dhttp-specific code across the upgraded source:
```bash
grep -rn "HeaderOrderKey\|PHeaderOrderKey" --include='*.go' .       # all header-ordering hooks
grep -rn "ClientHelloSettings\|tls.UConn\|HelloID" --include='*.go' . # utls touch points
grep -rn "// dhttp:" --include='*.go' .                               # explicit dhttp markers
grep -rn "pHeaderOrder\|sortedKeyValuesBy" --include='*.go' .         # pseudo-header + ordered emit
```

When investigating why a test fails after an upgrade, check `.test_skip.txt` first — many failures have documented rationale.
