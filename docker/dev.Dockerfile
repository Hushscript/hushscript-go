ARG GO_VERSION=1.26

FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS go1.26
FROM golang:1.27-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS go1.27
FROM golangci/golangci-lint:v2.14.0@sha256:ad862ba6b3798cbe0fd9fd7408d498fd74fbd2623a92406b2fd3898faf0bf98f AS golangci

# go1.26 and go1.27 above are the digest-pinned images; the build arg only selects one.
# hadolint ignore=DL3006
FROM go${GO_VERSION} AS base
RUN groupadd --gid 10001 dev && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin dev
ENV HOME=/tmp \
    GOTOOLCHAIN=local \
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
