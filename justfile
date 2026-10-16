# Every recipe runs in containers; nothing here touches host toolchains.
# Examples: `just ci`, `just test 1.27`, `CONFORMANCE_CONTEXT=../hushscript-sdk-conformance just conformance`.
set shell := ["bash", "-euo", "pipefail", "-c"]

export HOST_UID := `id -u`
export HOST_GID := `id -g`
# No scenario may be skipped: an unmapped op fails the conformance run.
export CONFORMANCE_STRICT := env("CONFORMANCE_STRICT", "1")
# The conformance suite is read at the ref in conformance-ref, the one place it is pinned.
conformance_ref := `tr -d '[:space:]' < conformance-ref`
export CONFORMANCE_CONTEXT := env("CONFORMANCE_CONTEXT", "https://github.com/Hushscript/hushscript-sdk-conformance.git#" + conformance_ref)

node_image := "node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392"
hadolint_image := "hadolint/hadolint:v2.15.1@sha256:32dac94127fd60b7b7e3fbfc65e1383b9b5e25c9bfd7b8536de7a539fe68a12d"
actionlint_image := "rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667"
zizmor_image := "ghcr.io/zizmorcore/zizmor:1.31.1@sha256:42d409dbb851d4c5d35a058cf042b828bea1e3f2884378c526eb0974780e18d9"
shellcheck_image := "koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d"
gitleaks_image := "ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f"
sandbox := "--rm --init --label sdk.test=1 --cap-drop ALL --read-only --security-opt no-new-privileges --pids-limit 512 --memory 4g --tmpfs /tmp"
floor := "1.25"
supported := "1.25 1.26 1.27"
# The one version that runs govulncheck; older versions skip it.
vuln_version := "1.27"
# The one place the mutation floor lives: a percentage, enforced on the killed/scored total of all eight files.
mutation_floor := "75"

# unit tests with the race detector and the coverage floor on Go 1.25 (default), 1.26 or 1.27; no network beyond the mock (the hint tests read the conformance fixtures: CONFORMANCE_DIR, else CONFORMANCE_CONTEXT, else a fetch of the pinned ref)
test version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    CONFORMANCE_DIR="$(scripts/conformance-dir.sh)"
    export CONFORMANCE_DIR
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock test
    docker compose run --rm test

# go-mutesting on selected source files (all eight by default), failing below the floor; MUTATION_TIMEOUT seconds (default 21600) bound the run
[positional-arguments]
mutation version=floor *files:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    CONFORMANCE_DIR="$(scripts/conformance-dir.sh)"
    export CONFORMANCE_DIR
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock mutation
    shift
    docker compose run --rm mutation scripts/mutation.sh {{mutation_floor}} "$@"

# nightly shard: go-mutesting on one source file, its killed and scored counts written to <counts_dir>/<file>.counts on the host; fails only on a timeout or output it cannot read, the floor is enforced on the total by mutation-sum
mutation-shard file counts_dir version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    CONFORMANCE_DIR="$(scripts/conformance-dir.sh)"
    export CONFORMANCE_DIR
    mkdir -p "{{counts_dir}}"
    counts="$(realpath "{{counts_dir}}")"
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock mutation
    # The container's root is read-only; the counts directory is its one writable mount, owned by the caller.
    docker compose run --rm --user "$(id -u):$(id -g)" -v "$counts":/counts mutation scripts/mutation.sh --counts "/counts/{{file}}.counts" "{{file}}"

# add up the counts of every nightly shard in counts_dir (plain bash and awk, no container) and enforce the floor on the total
mutation-sum counts_dir:
    #!/usr/bin/env bash
    set -euo pipefail
    scripts/mutation.sh --sum {{mutation_floor}} "{{counts_dir}}"/*.counts

# native fuzz targets, FUZZ_TIME each (default 60s; nightly)
fuzz version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock fuzz
    docker compose run --rm fuzz

# the conformance suite against the mock
conformance version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock conformance
    docker compose run --rm conformance

# every example against the mock
examples version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock examples
    docker compose run --rm examples

# every static check: build, gofumpt, golangci-lint, hadolint, actionlint, zizmor, shellcheck, gitleaks, typos, go mod tidy, govulncheck (newest Go only)
lint version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock lint audit
    docker compose run --rm lint
    run() { docker run {{sandbox}} --network none --user "$(id -u):$(id -g)" -v "$(pwd)":/work:ro -w /work -e HOME=/tmp "$@"; }
    run {{hadolint_image}} /bin/hadolint docker/dev.Dockerfile docker/typos.Dockerfile
    run {{actionlint_image}} -color
    run node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392 timeout 60 node --test scripts/release-candidate.test.mjs
    run --label codex.adhoc=1 {{node_image}} timeout 60 node --test scripts/release-candidate.test.mjs
    run {{zizmor_image}} --pedantic --offline --config zizmor.yml .github
    run {{shellcheck_image}} scripts/*.sh
    run {{gitleaks_image}} git --no-banner --redact --verbose /work
    docker build --quiet -f docker/typos.Dockerfile -t hushscript-sdk-go-typos:local docker
    run hushscript-sdk-go-typos:local --config _typos.toml
    # The one lint step with network access: it asks the module proxy, and on the newest Go the vulnerability database.
    docker compose run --rm -e VULNCHECK={{ if version == vuln_version { "1" } else { "0" } }} audit

# go build on the floor or latest Go, a consumer module that imports the package, and apidiff once a release tag exists
build version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    export GO_VERSION={{version}}
    docker build --quiet --target dev --build-arg GO_VERSION={{version}} -f docker/dev.Dockerfile -t "hushscript-sdk-go-dev:go{{version}}" .
    user="$(id -u):$(id -g)"
    docker run {{sandbox}} --network none --user "$user" -e GOPROXY=off --tmpfs /tmp:exec,size=3g "hushscript-sdk-go-dev:go{{version}}" \
      sh -c 'timeout 600 go build ./... && scripts/consumer-check.sh /work'
    # apidiff against the last tag, once there is one (spec 13.2: after 1.0.0).
    last="$(git tag --list 'v[0-9]*' --sort=-v:refname | head -n1)"
    if [ -z "$last" ]; then
      echo "apidiff: no release tag yet, skipped"
    else
      old="$(mktemp -d)"
      trap 'rm -rf "$old"' EXIT
      git archive "$last" | tar -x -C "$old"
      # The image caches only the current module graph (GOPROXY=off), so the old
      # source is built against today's go.mod/go.sum, as an upgrading consumer would.
      docker run {{sandbox}} --network none --user "$user" -e GOPROXY=off --tmpfs /tmp:exec,size=3g -v "$old":/old:ro "hushscript-sdk-go-dev:go{{version}}" \
        sh -c 'cp -r /old /tmp/old-src && cp /work/go.mod /work/go.sum /tmp/old-src/ && cd /tmp/old-src && old_module="$(go list -m)" && go tool -modfile=/work/tools/go.mod apidiff -m -w /tmp/old.export "$old_module" && cd /work && go tool -modfile=tools/go.mod apidiff -m -incompatible /tmp/old.export github.com/Hushscript/hushscript-sdk-go'
    fi

# regenerate openapi.json and generated/ from the production spec, or from a local spec file (outbound network)
regen spec="":
    #!/usr/bin/env bash
    set -euo pipefail
    export GO_VERSION={{floor}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    extra=()
    if [ -n "${CONFORMANCE_DIR:-}" ]; then
      extra=(-v "$(realpath "$CONFORMANCE_DIR")":/conformance:ro -e CONFORMANCE_DIR=/conformance)
    fi
    spec="{{spec}}"
    # A local file is bind-mounted, so the generator reads exactly the bytes that were diffed.
    if [ -n "$spec" ] && [ -f "$spec" ]; then
      extra+=(-v "$(realpath "$spec")":/spec/openapi.json:ro)
      spec=/spec/openapi.json
    fi
    docker compose build regen
    docker compose run --rm ${extra[@]+"${extra[@]}"} regen timeout 600 go run ./scripts ${spec:+"$spec"}

# every CI gate on each selected runtime; no argument runs Go 1.25, 1.26 and 1.27
ci version="":
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "" { "" } else if version == "1.25" { "" } else if version == "1.26" { "" } else if version == "1.27" { "" } else { error("version must be 1.25, 1.26 or 1.27") } }}
    if [ -n "{{version}}" ]; then versions="{{version}}"; else versions="{{supported}}"; fi
    status=0
    for v in $versions; do
      just lint "$v" || status=$?
      just test "$v" || status=$?
      just conformance "$v" || status=$?
      just examples "$v" || status=$?
      just build "$v" || status=$?
    done
    exit "$status"

# nightly smoke against production; needs SMOKE_TOKEN (outbound network)
smoke:
    #!/usr/bin/env bash
    set -euo pipefail
    : "${SMOKE_TOKEN:?set SMOKE_TOKEN to a token that can read the account}"
    export GO_VERSION={{floor}}
    docker compose build smoke
    docker compose run --rm smoke
