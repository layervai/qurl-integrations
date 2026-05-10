# syntax=docker/dockerfile:1.7
# The `# syntax=` directive opts into the modern BuildKit frontend so
# `apps/slack/Dockerfile.dockerignore` is honored as the per-Dockerfile
# ignore list (default frontend reads only `<context>/.dockerignore`,
# which is the wrong place for a multi-app monorepo).

FROM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build

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

# arm64 target matches the ECS Fargate task definition (Graviton).
# `-trimpath` strips local build paths from the binary, `-mod=readonly`
# refuses any mutation of go.mod/go.sum during the build (catches
# accidental network reaches that would invalidate reproducibility),
# `-s -w` strips DWARF + symbol tables for a smaller image.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -mod=readonly -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qurl-bot-slack ./apps/slack/cmd/

# distroless/static-debian12 is the smallest viable runtime for a
# CGO=0 Go binary: glibc-free, shell-free, package-manager-free, with
# only ca-certificates baked in. The `:nonroot` tag runs as UID 65532
# by default — combined with `USER nonroot:nonroot` below, this is
# defense-in-depth against orchestrator misconfig.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=build /out/qurl-bot-slack /usr/local/bin/qurl-bot-slack

# Documentation-only — actual port comes from `listenAddr` in
# apps/slack/cmd/main.go. If that constant moves, update both.
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/qurl-bot-slack"]
