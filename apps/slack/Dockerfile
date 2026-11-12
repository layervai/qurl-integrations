# syntax=docker/dockerfile:1.7
# The `# syntax=` directive opts into the modern BuildKit frontend so
# `apps/slack/Dockerfile.dockerignore` is honored as the per-Dockerfile
# ignore list (default frontend reads only `<context>/.dockerignore`,
# which is the wrong place for a multi-app monorepo).

# Go 1.26.11 fixes GO-2026-6859 and matches go.mod.
# SHA-pinned to the multi-arch index digest, same as
# the rest of the repo's base images — GOTOOLCHAIN stays the image default
# `local`, so there's no unpinned build-time toolchain fetch.
# Compile on the builder CPU; TARGETOS/TARGETARCH select the runtime output.
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# `internal/` and `shared/` are copied before `apps/slack/` so an app-only edit
# doesn't invalidate shared-code layers. Most PRs touch one app.
COPY internal/ ./internal/
COPY shared/ ./shared/
COPY apps/slack/ ./apps/slack/

# `VERSION` ARG sits below the source COPYs so a `--build-arg
# VERSION=...` change only invalidates the build layer, not the
# preceding source/dep layers. Don't move it up.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -mod=readonly -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qurl-bot-slack ./apps/slack/cmd/

FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=build /out/qurl-bot-slack /usr/local/bin/qurl-bot-slack

# Documentation-only — actual port comes from `listenAddr` in
# apps/slack/cmd/main.go. If that constant moves, update both.
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/qurl-bot-slack"]
