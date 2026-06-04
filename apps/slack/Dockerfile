# syntax=docker/dockerfile:1.7
# The `# syntax=` directive opts into the modern BuildKit frontend so
# `apps/slack/Dockerfile.dockerignore` is honored as the per-Dockerfile
# ignore list (default frontend reads only `<context>/.dockerignore`,
# which is the wrong place for a multi-app monorepo).

FROM golang:1.26.3-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS build

# go.mod requires go 1.26.4 (GO-2026-5039 / GO-2026-5037 net/textproto fix),
# but this base image is SHA-pinned at 1.26.3 and ships GOTOOLCHAIN=local, so a
# `go` command would fail "requires go >= 1.26.4". GOTOOLCHAIN=auto lets the
# build fetch the matching toolchain (verified against the Go checksum DB) at
# build time. We don't bump the base image to golang:1.26.4-alpine because the
# repo's check-docker-age gate requires base images ≥14 days old, and the patch
# is too fresh; swap to a pinned 1.26.4 base once it ages past that gate.
ENV GOTOOLCHAIN=auto

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
