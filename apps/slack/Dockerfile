# Multi-stage build for the Slack bot.
#
# - Build stage: alpine + Go toolchain compiles a static ARM64 binary.
#   CGO is off so the binary doesn't pull glibc/musl at runtime,
#   matching the distroless static base.
# - Runtime stage: distroless/static + nonroot user. No shell, no
#   package manager, no busybox — minimum reachable surface for a
#   process whose only job is HTTP-in / HTTP-out.
#
# Both bases are SHA-pinned so a tag-mover at upstream can't change
# what we ship without a deliberate Dockerfile bump (matches the
# action-pinning posture from CLAUDE.md / dep-age-check).

FROM golang:1.26.2-alpine3.22@sha256:7ef941168f213aa115df2e61364d67682129e99dc8188b734139dea862cc7d31 AS build

WORKDIR /src

# Cache the module download layer separately from the source. Only
# `go.mod`/`go.sum` invalidate the dependency graph; touching .go
# files doesn't force a re-fetch of the dependency cache.
COPY go.mod go.sum ./
RUN go mod download

COPY apps/slack/ ./apps/slack/
COPY shared/ ./shared/

# `version` ldflag is set at build time so the qURL client User-Agent
# pins a deploy to a specific git SHA. Default "dev" stays in for
# local builds (`docker build .`); CI overrides via --build-arg.
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qurl-bot-slack ./apps/slack/cmd/

# Runtime stage. distroless/static has no shell, so HEALTHCHECK can't
# call curl/wget — Fargate health checks come from the ALB target
# group hitting :8080/health, which is a stronger signal than a
# container-local probe anyway.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=build /out/qurl-bot-slack /usr/local/bin/qurl-bot-slack

EXPOSE 8080

# Distroless's nonroot user is uid 65532 — the ALB-only ingress
# means root would offer no real value, and dropping privileges
# closes a defense-in-depth gap if a future syscall (like
# /proc/self/exe rewrites) ever became reachable.
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/qurl-bot-slack"]
