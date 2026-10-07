ARG GO_VERSION=1.25

FROM golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS go1.25
FROM golang:1.27-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS go1.27
FROM golangci/golangci-lint:v2.14.0@sha256:ad862ba6b3798cbe0fd9fd7408d498fd74fbd2623a92406b2fd3898faf0bf98f AS golangci

# go1.25 and go1.27 above are the digest-pinned images; the build arg only selects one.
# hadolint ignore=DL3006
FROM go${GO_VERSION} AS base
RUN groupadd --gid 10001 dev && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin dev
ENV HOME=/tmp \
    GOFLAGS=-mod=readonly \
    GOCACHE=/tmp/gocache \
    GOLANGCI_LINT_CACHE=/tmp/golangci-lint-cache
WORKDIR /work

# The module cache is filled from the lockfile only; containers that run without network read it from here.
FROM base AS deps
COPY go.mod go.sum ./
COPY tools/go.mod tools/go.sum tools/
RUN go mod download
WORKDIR /work/tools
RUN go mod download
WORKDIR /work

FROM base AS dev
COPY --from=deps /go/pkg/mod /go/pkg/mod
COPY --from=golangci /usr/bin/golangci-lint /usr/local/bin/golangci-lint
COPY --chown=10001:10001 . .
USER 10001:10001
