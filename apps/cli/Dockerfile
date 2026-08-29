# syntax=docker/dockerfile:1.7

FROM golang:1.26.6-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY shared/ ./shared/
COPY apps/cli/ ./apps/cli/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -mod=readonly -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qurl ./apps/cli/cmd/

# The customer image has one executable: qurl. The CA bundle is data required
# for HTTPS control-plane calls; there is no shell, package manager, companion
# connector, or entrypoint wrapper.
FROM scratch
LABEL org.opencontainers.image.source="https://github.com/layervai/qurl-integrations"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/qurl /usr/local/bin/qurl
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/qurl"]
