#!/usr/bin/env bash
# scripts/upgrade.sh <new-upstream-tag>
#
# Bump UPSTREAM_TAG, re-run build.sh, surface any patch rejects.
# After a clean build, run go build + canary tests as a sanity gate.

set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

if [[ $# -ne 1 ]]; then
  echo "Usage: scripts/upgrade.sh <upstream-tag>   (e.g. go1.26.3)" >&2
  exit 2
fi
NEW_TAG="$1"
OLD_TAG=$(<UPSTREAM_TAG)

if [[ "$NEW_TAG" == "$OLD_TAG" ]]; then
  echo "Already on $NEW_TAG; nothing to do." >&2
  exit 0
fi

# Refuse to run on a dirty working tree — losing work to a rebuild gone wrong is bad.
if [[ -n "$(git status --porcelain)" ]]; then
  echo "Working tree is dirty. Commit or stash before upgrading." >&2
  git status --short >&2
  exit 1
fi

echo "==> Upgrading from $OLD_TAG to $NEW_TAG"
echo "$NEW_TAG" > UPSTREAM_TAG

if ! bash scripts/build.sh; then
  echo
  echo "!! build.sh failed. Likely patch rejects." >&2
  echo "   Check the build scratch tree for .rej files." >&2
  echo "   To refresh a patch:" >&2
  echo "     1. Edit the generated source to apply the rejected hunk by hand." >&2
  echo "     2. Regenerate the patch via:" >&2
  echo "        diff -Naur <vanilla-flat> <fixed-tree> > patches/<patch-name>.patch" >&2
  echo "     3. Re-run scripts/upgrade.sh $NEW_TAG." >&2
  echo
  echo "   Restoring UPSTREAM_TAG=$OLD_TAG (re-run build.sh to revert build state)." >&2
  echo "$OLD_TAG" > UPSTREAM_TAG
  exit 1
fi

echo
echo "==> Build clean. Sanity check: go build ./..."
if ! go build ./... 2>&1 | grep -v 'upstream/' | grep -v '^$'; then
  : # output already streamed
fi
go build ./... >/dev/null 2>&1 || {
  echo "!! go build ./... failed for dhttp packages. Investigate." >&2
  exit 1
}

echo
echo "==> Canary tests"
go test -count=1 -timeout 60s -race -run \
  'TestHeaderOrder|TestPHeaderOrder|TestNextProtoUpgrade|TestClientHelloID|TestReadBody|TestDefaultAcceptEncodingHeader|TestWriteSubsetConcurrentHeaderWrite|TestUserAgentMissingHeader|TestTransferWriterHeaderShim' . 2>&1 | tail -20

echo
echo "==> Upgrade to $NEW_TAG done."
echo "    Next: review git diff, run JA3/JA4 smoke vs .fingerprint-baseline.json, commit."
