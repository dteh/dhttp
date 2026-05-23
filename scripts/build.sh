#!/usr/bin/env bash
# scripts/build.sh — regenerate dhttp from upstream Go + patches/ + overlay/.
#
# Idempotent. Re-running over an already-built tree produces the same output.
# Driven by UPSTREAM_TAG, INTERNAL_DEPS, patches/series, overlay/.

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
TAG=$(<UPSTREAM_TAG)
CACHE="${HOME}/.cache/dhttp-build/go.git"
SCRATCH=$(mktemp -d -t dhttp-build.XXXXXX)
trap 'rm -rf "$SCRATCH"' EXIT

echo "==> Building dhttp from upstream tag: $TAG"

# --- Step 1: Fetch upstream Go at the pinned tag (shallow clone, cached) ---
if [[ ! -d "$CACHE" ]]; then
  mkdir -p "$(dirname "$CACHE")"
  git clone --filter=blob:none --no-checkout https://github.com/golang/go "$CACHE"
fi
git -C "$CACHE" fetch --depth=1 origin "refs/tags/$TAG:refs/tags/$TAG" 2>/dev/null || true
git -C "$CACHE" checkout --force --detach "$TAG"

# --- Step 2: Stage src/net/http + INTERNAL_DEPS into scratch ---
echo "==> Staging upstream tree"
mkdir -p "$SCRATCH/dhttp"
rsync -a "$CACHE/src/net/http/" "$SCRATCH/dhttp/"

while IFS= read -r dep; do
  [[ -z "$dep" || "$dep" =~ ^# ]] && continue
  base=$(basename "$dep")
  if [[ "$dep" == src/net/http/internal ]]; then
    # The chunked.go-and-friends package; already copied above as part of src/net/http/.
    # Filter out subdirs that are tracked separately (ascii, testcert) to avoid double-stamp.
    continue
  fi
  src_path="$CACHE/$dep"
  [[ -d "$src_path" ]] || { echo "Missing upstream dep: $dep" >&2; exit 1; }
  # net/http/internal/ascii and net/http/internal/testcert keep their /internal/ prefix
  # in the final layout (they live at dhttp's internal/ascii and internal/testcert).
  # src/internal/X also lands at dhttp's internal/X.
  if [[ "$dep" == src/net/http/internal/* ]]; then
    dest="$SCRATCH/dhttp/internal/$base"
    # Already copied as part of src/net/http/; remove first to avoid stale files.
    rm -rf "$dest"
    rsync -a "$src_path/" "$dest/"
  else
    dest="$SCRATCH/dhttp/internal/$base"
    mkdir -p "$dest"
    rsync -a "$src_path/" "$dest/"
  fi
done < INTERNAL_DEPS

# --- Step 3: Apply patches (in series order) ---
echo "==> Applying patches"
while IFS= read -r patch; do
  [[ -z "$patch" || "$patch" =~ ^# ]] && continue
  patch_path="$ROOT/patches/$patch"
  [[ -f "$patch_path" ]] || { echo "Missing patch: $patch_path" >&2; exit 1; }
  # Patch paths use the a/ b/ convention (git-style). -p1 strips it.
  (cd "$SCRATCH/dhttp" && patch -p1 --no-backup-if-mismatch -i "$patch_path") || {
    echo "Patch failed: $patch" >&2
    echo "Rejects in $SCRATCH" >&2
    find "$SCRATCH" -name '*.rej' >&2
    trap - EXIT
    exit 1
  }
done < patches/series

# --- Step 4: Apply mechanical import rewrites (forward direction) ---
echo "==> Applying forward import rewrites"
python3 "$ROOT/scripts/forward_rewrite.py" "$SCRATCH/dhttp"

# --- Step 5: Copy scratch tree back into repo root ---
echo "==> Copying generated tree into repo"
# Wipe known-generated files first to catch deletions upstream.
# We use `find` over known dirs to avoid touching repo control files.
GEN_DIRS=(cgi cookiejar fcgi httptest httptrace httputil internal pprof)
for d in "${GEN_DIRS[@]}"; do rm -rf "$ROOT/$d"; done
find "$ROOT" -maxdepth 1 -name '*.go' -delete

rsync -a "$SCRATCH/dhttp/" "$ROOT/"

# --- Step 6: Overlay (fork-only files override generated) ---
# The directory is named _overlay so Go tooling ignores it (otherwise the
# package http files staged there get flagged by go vet / gopls as broken
# because their directory name doesn't match their package declaration).
echo "==> Applying overlay"
rsync -a "$ROOT/_overlay/" "$ROOT/" 2>/dev/null || true

# --- Step 7: Stamp generated header on .go files (skip overlay-named ones) ---
echo "==> Stamping generated header"
OVERLAY_BASENAMES=()
while IFS= read -r line; do
  OVERLAY_BASENAMES+=("$line")
done < <(find "$ROOT/_overlay" -name '*.go' -exec basename {} \;)
BANNER="// Code generated from patches/. DO NOT EDIT."
find "$ROOT" -maxdepth 4 -name '*.go' \
  -not -path "$ROOT/_overlay/*" \
  -not -path "$ROOT/scripts/*" \
  -not -path "$ROOT/upstream/*" \
  -not -path "$ROOT/.git/*" | while read -r f; do
  base=$(basename "$f")
  skip=0
  for ob in "${OVERLAY_BASENAMES[@]}"; do [[ "$base" == "$ob" ]] && { skip=1; break; }; done
  (( skip )) && continue
  # Only stamp if not already stamped.
  head -1 "$f" | grep -qF "$BANNER" || {
    tmp=$(mktemp)
    printf '%s\n\n' "$BANNER" > "$tmp"
    cat "$f" >> "$tmp"
    mv "$tmp" "$f"
  }
done

# --- Step 8: tidy module ---
echo "==> go mod tidy"
(cd "$ROOT" && go mod tidy) || echo "(go mod tidy reported issues; review)" >&2

echo "==> Done. Upstream: $TAG. Patches: $(wc -l < patches/series). Overlay: ${#OVERLAY_BASENAMES[@]} files."
