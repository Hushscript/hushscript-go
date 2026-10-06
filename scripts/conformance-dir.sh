#!/usr/bin/env bash
# Prints the directory of a conformance checkout, for the tests that read its fixtures.
# Order: $CONFORMANCE_DIR, then $CONFORMANCE_CONTEXT when it is a local directory, then a shallow
# clone of the pinned tag (cached in the temp directory; needs network). Usage: scripts/conformance-dir.sh
set -euo pipefail

repo="https://github.com/Hushscript/hushscript-sdk-conformance.git"
ref="v0.1.0" # keep in step with compose.yaml and internal/gen ConformanceRef

if [ -n "${CONFORMANCE_DIR:-}" ]; then
  dir="$CONFORMANCE_DIR"
elif [ -n "${CONFORMANCE_CONTEXT:-}" ] && [ -d "$CONFORMANCE_CONTEXT" ]; then
  dir="$CONFORMANCE_CONTEXT"
else
  dir="${TMPDIR:-/tmp}/hushscript-sdk-conformance-$ref"
  if [ ! -d "$dir/fixtures" ]; then
    rm -rf "$dir"
    git clone --quiet --depth 1 --branch "$ref" "$repo" "$dir" >&2
  fi
fi
[ -f "$dir/fixtures/hint-rendering.json" ] || { echo "conformance-dir: no fixtures/hint-rendering.json in $dir" >&2; exit 1; }
realpath "$dir"
