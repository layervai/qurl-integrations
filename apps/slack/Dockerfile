# syntax=docker/dockerfile:1.7
# The `# syntax=` directive opts into the modern BuildKit frontend so
# `apps/slack/Dockerfile.dockerignore` is honored as the per-Dockerfile
# ignore list (default frontend reads only `<context>/.dockerignore`,
# which is the wrong place for a multi-app monorepo).

# Bumped 1.26.3 → 1.26.4 for the net/textproto fix (GO-2026-5039 / GO-2026-5037);
# go.mod requires go 1.26.4. SHA-pinned to the multi-arch index digest, same as
# the rest of the repo's base images — GOTOOLCHAIN stays the image default
# `local`, so there's no unpinned build-time toolchain fetch.
FROM golang:1.26.4-alpine@sha256:f23e8b227fb4493eabe03bede4d5a32d04092da71962f1fb79b5f7d1e6c2a17f AS build

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

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -mod=readonly -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qurl-bot-slack ./apps/slack/cmd/

FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=build /out/qurl-bot-slack /usr/local/bin/qurl-bot-slack

# Documentation-only — actual port comes from `listenAddr` in
# apps/slack/cmd/main.go. If that constant moves, update both.
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/qurl-bot-slack"]
