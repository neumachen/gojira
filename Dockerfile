# syntax=docker/dockerfile:1.7
#
# Dockerfile for gojira — multi-stage build producing a minimal Alpine
# runtime image containing only the binary, the CA bundle needed for
# HTTPS to Jira Cloud, and a non-root user.
#
# Image philosophy
#
#   builder stage  : full Go toolchain on Alpine; compiles a static,
#                    stripped, trimpath'd binary with CGO disabled.
#   runtime stage  : Alpine 3.23 with ca-certificates and a non-root
#                    user; nothing else.
#
# Why Alpine over distroless or scratch
#
#   Alpine 3.23 keeps a shell available for `docker exec`-style
#   debugging while staying small (about 7.5 MB base layer).
#   gcr.io/distroless/static:nonroot would shave roughly 5 MB more
#   and is a reasonable swap if you never need a shell inside the
#   image — to switch, replace the runtime FROM line with
#   `gcr.io/distroless/static:nonroot` and drop the apk add and
#   adduser RUN steps. scratch is not viable because gojira talks
#   HTTPS to Jira Cloud and needs ca-certificates.
#
# Why not tini or dumb-init
#
#   gojira is a one-shot CLI that handles its own signals via the
#   urfave/cli/v3 wiring in cmd/gojira/main.go. Wrapping it in an
#   init process would add a layer without changing behavior. If
#   gojira ever becomes a long-running daemon, reconsider.
#
# Build args you can override at build time
#
#   GO_VERSION       Go toolchain version used by the builder.
#                    Pinned to the version declared in go.mod.
#   ALPINE_VERSION   Alpine version for both stages.
#   GOJIRA_VERSION   String embedded in the binary's version output.
#                    Defaults to "dev" so unstamped local builds are
#                    obvious; CI should pass the real release tag.

# -----------------------------------------------------------------------
# Stage 1: build
# -----------------------------------------------------------------------

ARG GO_VERSION=1.26.3
ARG ALPINE_VERSION=3.23

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

# git is needed for `go mod download` to resolve module paths that go
# through the GitHub-backed module proxy. ca-certificates is needed
# for HTTPS to proxy.golang.org and any module replace targets.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache module downloads in a separate layer keyed on go.mod and
# go.sum. This avoids re-downloading deps when only source files change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Now copy the source. The .dockerignore file controls what actually
# arrives here; in particular .git, .aider*, docs/, and local scripts
# are excluded.
COPY . .

# Compile a static, stripped binary suitable for the minimal runtime
# stage. CGO disabled so the resulting binary has no glibc or musl
# runtime dependency.
#
#   -s -w        : strip symbol table and DWARF info; smaller binary,
#                  slightly harder to attach a debugger to (acceptable
#                  for a deployed CLI).
#   -trimpath    : strip absolute paths so the binary does not leak
#                  build-host filesystem layout.
#   -buildvcs=false : skip embedding VCS state from the build context
#                  so the build works against a tarball checkout too.
ARG GOJIRA_VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH:-amd64} \
    GOFLAGS=-buildvcs=false \
    go build \
        -trimpath \
        -ldflags="-s -w -X 'github.com/neumachen/gojira.Version=${GOJIRA_VERSION}'" \
        -o /out/gojira \
        ./cmd/gojira

# Sanity check: the built binary actually runs and reports its own
# version. This doubles as a smoke test inside the builder stage so a
# broken build is caught before the runtime stage even starts.
RUN /out/gojira --version

# -----------------------------------------------------------------------
# Stage 2: runtime
# -----------------------------------------------------------------------

FROM alpine:${ALPINE_VERSION} AS runtime

# Install only what gojira needs at runtime:
#   ca-certificates : HTTPS to Jira Cloud and to GitHub PR URLs.
#   tzdata          : Jira timestamps carry timezone offsets;
#                     time.LoadLocation needs the zoneinfo database.
#
# --no-cache keeps the apk metadata out of the final image layer.
RUN apk add --no-cache \
        ca-certificates \
        tzdata

# Create a dedicated non-root user with a fixed UID/GID. 65532 is the
# de-facto nonroot UID used by distroless and many other minimal
# images, which makes the image interchangeable with a distroless swap
# and predictable for volume-permission planning on the host.
RUN addgroup -g 65532 -S gojira && \
    adduser  -u 65532 -S -G gojira -h /home/gojira gojira

# The output directory the container writes Markdown into. The user
# is expected to bind-mount a host directory here at runtime (see
# docker-compose.yml). Pre-create it with the right ownership so
# crawls work even without an explicit volume.
RUN mkdir -p /output && chown gojira:gojira /output

# Copy the binary from the builder stage with explicit ownership and
# a read+execute mode. No write bit; nothing inside the container
# should rewrite the binary.
COPY --from=builder --chown=root:root --chmod=0555 /out/gojira /usr/local/bin/gojira

# Default output location, overridable at runtime via the
# --output-dir flag or GOJIRA_OUTPUT_DIR env var.
ENV GOJIRA_OUTPUT_DIR=/output

# Run as the non-root user. After this point the container cannot
# install packages or modify system files.
USER gojira:gojira
WORKDIR /home/gojira

# The CLI is the entrypoint. `docker run gojira:<tag>` with no
# arguments prints help and exits non-zero; `docker run gojira:<tag>
# crawl --site ... EXAMPLE-1` runs a crawl.
ENTRYPOINT ["/usr/local/bin/gojira"]

# No CMD: leaving it empty means `docker run gojira` shows usage and
# exits 1, which is the right default for a CLI tool with no implicit
# subcommand.

# OCI image annotations help registries and consumers identify the
# image. Build tooling can override these via --label or oci-labels.
LABEL org.opencontainers.image.title="gojira" \
      org.opencontainers.image.description="Recursively mirror Jira issue graphs into Markdown." \
      org.opencontainers.image.source="https://github.com/neumachen/gojira" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.documentation="https://github.com/neumachen/gojira#readme"
