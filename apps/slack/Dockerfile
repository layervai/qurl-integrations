# syntax=docker/dockerfile:1.7
# The `# syntax=` directive opts into the modern BuildKit frontend so
# `apps/slack/Dockerfile.dockerignore` is honored as the per-Dockerfile
# ignore list (default frontend reads only `<context>/.dockerignore`,
# which is the wrong place for a multi-app monorepo).

# `--platform=$BUILDPLATFORM` pins the build stage to the runner's
# native arch so amd64 CI runners don't pay QEMU's emulation cost
# while compiling. Go cross-compiles natively, so the binary is still
# `linux/arm64` (set via `GOARCH=$TARGETARCH` below) — the runtime
# image stays on the requested target platform.
FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# `shared/` is copied before `apps/slack/` so an app-only edit doesn't
# invalidate the `shared/` layer's cache. Most PRs touch one app.
COPY shared/ ./shared/
COPY apps/slack/ ./apps/slack/

# `VERSION` ARG sits below the source COPYs so a `--build-arg
# VERSION=...` change only invalidates the build layer, not the
# preceding source/dep layers. Don't move it up.
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -mod=readonly -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qurl-bot-slack ./apps/slack/cmd/

FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=build /out/qurl-bot-slack /usr/local/bin/qurl-bot-slack

# Documentation-only — actual port comes from `listenAddr` in
# apps/slack/cmd/main.go. If that constant moves, update both.
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/qurl-bot-slack"]
