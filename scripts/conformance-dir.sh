#!/usr/bin/env bash
# Prints the directory of a conformance checkout, for the tests that read its fixtures.
# Order: $CONFORMANCE_DIR, then $CONFORMANCE_CONTEXT when it is a local directory, then a shallow
# shallow fetch of the pinned ref, a tag or a commit SHA (cached in the temp directory; needs network).
# Usage: scripts/conformance-dir.sh
set -euo pipefail

repo="https://github.com/Hushscript/hushscript-sdk-conformance.git"
ref="$(tr -d '[:space:]' < "$(dirname "$0")/../conformance-ref")" # the one place the ref is pinned

if [ -n "${CONFORMANCE_DIR:-}" ]; then
  dir="$CONFORMANCE_DIR"
elif [ -n "${CONFORMANCE_CONTEXT:-}" ] && [ -d "$CONFORMANCE_CONTEXT" ]; then
  dir="$CONFORMANCE_CONTEXT"
else
  dir="${TMPDIR:-/tmp}/hushscript-sdk-conformance-$ref"
  if [ ! -d "$dir/fixtures" ]; then
    rm -rf "$dir"
    # init + fetch, not clone --branch: a branch or tag name and a full commit SHA both work
    git init --quiet "$dir" >&2
    git -C "$dir" fetch --quiet --depth 1 "$repo" "$ref" >&2
    git -C "$dir" -c advice.detachedHead=false checkout --quiet FETCH_HEAD >&2
  fi
fi
[ -f "$dir/fixtures/hint-rendering.json" ] || { echo "conformance-dir: no fixtures/hint-rendering.json in $dir" >&2; exit 1; }
realpath "$dir"
