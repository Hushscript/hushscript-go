# Every recipe runs in containers; nothing here touches host toolchains.
# Examples: `just ci`, `just test 1.26`, `CONFORMANCE_CONTEXT=../hushscript-sdk-conformance just conformance`.
set shell := ["bash", "-euo", "pipefail", "-c"]

export HOST_UID := `id -u`
export HOST_GID := `id -g`

hadolint_image := "hadolint/hadolint:v2.14.0@sha256:27086352fd5e1907ea2b934eb1023f217c5ae087992eb59fde121dce9c9ff21e"
actionlint_image := "rhysd/actionlint:1.7.8@sha256:96d4a8c87dbbfb3bdd324f8fdc285fc3df5261e2decc619a4dd7e8ee52bbfd46"
zizmor_image := "ghcr.io/zizmorcore/zizmor:1.16.0@sha256:a6460f1b6fe3dc08e588d314e73563bdd218ee30c995a4be5e9ee5bd300391ce"
shellcheck_image := "koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d"
gitleaks_image := "ghcr.io/gitleaks/gitleaks:v8.28.0@sha256:cdbb7c955abce02001a9f6c9f602fb195b7fadc1e812065883f695d1eeaba854"
sandbox := "--rm --init --label sdk.test=1 --cap-drop ALL --read-only --security-opt no-new-privileges --pids-limit 512 --memory 4g --tmpfs /tmp"
floor := "1.25"
latest := "1.26"

# unit tests with the race detector and the coverage floor on Go 1.25 (default) or 1.26; no network beyond the mock (the hint tests read the conformance fixtures: CONFORMANCE_DIR, else CONFORMANCE_CONTEXT, else a clone of the pinned tag)
test version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else { error("version must be 1.25 or 1.26") } }}
    export GO_VERSION={{version}}
    CONFORMANCE_DIR="$(scripts/conformance-dir.sh)"
    export CONFORMANCE_DIR
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock test
    docker compose run --rm test

# go-mutesting on the HTTP core (nightly)
mutation version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else { error("version must be 1.25 or 1.26") } }}
    export GO_VERSION={{version}}
    CONFORMANCE_DIR="$(scripts/conformance-dir.sh)"
    export CONFORMANCE_DIR
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock mutation
    docker compose run --rm mutation

# native fuzz targets, FUZZ_TIME each (default 60s; nightly)
fuzz version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else { error("version must be 1.25 or 1.26") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock fuzz
    docker compose run --rm fuzz

# the conformance suite against the mock
conformance version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else { error("version must be 1.25 or 1.26") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock conformance
    docker compose run --rm conformance

# every example against the mock
examples version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else { error("version must be 1.25 or 1.26") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock examples
    docker compose run --rm examples

# every static check: build, gofumpt, golangci-lint, hadolint, actionlint, zizmor, shellcheck, gitleaks, typos, go mod tidy, govulncheck
lint version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else { error("version must be 1.25 or 1.26") } }}
    export GO_VERSION={{version}}
    trap 'docker compose down --remove-orphans >/dev/null 2>&1 || true' EXIT
    docker compose build mock lint audit
    docker compose run --rm lint
    run() { docker run {{sandbox}} --network none --user "$(id -u):$(id -g)" -v "$(pwd)":/work:ro -w /work -e HOME=/tmp "$@"; }
    run {{hadolint_image}} /bin/hadolint docker/dev.Dockerfile docker/typos.Dockerfile
    run {{actionlint_image}} -color
    run {{zizmor_image}} --pedantic --offline --config zizmor.yml .github
    run {{shellcheck_image}} scripts/*.sh
    run {{gitleaks_image}} git --no-banner --redact --verbose /work
    docker build --quiet -f docker/typos.Dockerfile -t hushscript-go-typos:local docker
    run hushscript-go-typos:local --config _typos.toml
    # The one lint step with network access: it asks the module proxy and the vulnerability database.
    docker compose run --rm audit

# go build on the floor or latest Go, a consumer module that imports the package, and apidiff once a release tag exists
build version=floor:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ if version == "1.25" { "" } else if version == "1.26" { "" } else { error("version must be 1.25 or 1.26") } }}
    export GO_VERSION={{version}}
    docker build --quiet --target dev --build-arg GO_VERSION={{version}} -f docker/dev.Dockerfile -t "hushscript-go-dev:go{{version}}" .
    user="$(id -u):$(id -g)"
    docker run {{sandbox}} --network none --user "$user" -e GOPROXY=off --tmpfs /tmp:exec,size=3g "hushscript-go-dev:go{{version}}" \
      sh -c 'timeout 600 go build ./... && scripts/consumer-check.sh /work'
    # apidiff against the last tag, once there is one (spec 13.2: after 0.1.0).
    last="$(git tag --list 'v[0-9]*' --sort=-v:refname | head -n1)"
    if [ -z "$last" ]; then
      echo "apidiff: no release tag yet, skipped"
    else
      old="$(mktemp -d)"
      trap 'rm -rf "$old"' EXIT
      git archive "$last" | tar -x -C "$old"
      docker run {{sandbox}} --network none --user "$user" -e GOPROXY=off --tmpfs /tmp:exec,size=3g -v "$old":/old:ro "hushscript-go-dev:go{{version}}" \
        sh -c 'cd /old && go tool apidiff -m -w /tmp/old.export github.com/Hushscript/hushscript-go && cd /work && go tool apidiff -m -incompatible /tmp/old.export github.com/Hushscript/hushscript-go'
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

# lint, then test and conformance, then examples and build; no argument runs Go 1.25 and 1.26
ci version="":
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -n "{{version}}" ]; then versions="{{version}}"; else versions="{{floor}} {{latest}}"; fi
    first="${versions%% *}"
    just lint "$first"
    for v in $versions; do
      just test "$v"
      just conformance "$v"
    done
    just examples "$first"
    just build "$first"

# nightly smoke against production; needs SMOKE_TOKEN (outbound network)
smoke:
    #!/usr/bin/env bash
    set -euo pipefail
    : "${SMOKE_TOKEN:?set SMOKE_TOKEN to a token that can read the account}"
    export GO_VERSION={{floor}}
    docker compose build smoke
    docker compose run --rm smoke
