#!/usr/bin/env bash
# Writes a CycloneDX 1.5 SBOM of the library's runtime dependencies (what a consumer's build resolves
# for the root package) to stdout. Usage: scripts/sbom.sh <version>
set -euo pipefail

version="${1:?version, for example v0.1.0}"
module="$(go list -m)"

go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}' . | sort -u |
  jq -R -s --arg module "$module" --arg version "$version" '
    def purl($path; $v): "pkg:golang/" + ($path | ascii_downcase) + "@" + $v;
    {
      bomFormat: "CycloneDX",
      specVersion: "1.5",
      version: 1,
      metadata: { component: { type: "library", name: $module, version: $version, "bom-ref": purl($module; $version), purl: purl($module; $version) } },
      components: (split("\n") | map(select(length > 0) | split(" ")) |
        map({ type: "library", name: .[0], version: .[1], "bom-ref": purl(.[0]; .[1]), purl: purl(.[0]; .[1]) }))
    }'
