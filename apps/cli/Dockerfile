# syntax=docker/dockerfile:1.7

FROM golang:1.26.10-alpine@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY shared/ ./shared/
COPY apps/cli/ ./apps/cli/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# This is a public X25519 trust root. It is safe in image history and build
# provenance; it is stored as a CI secret only to control release readiness.
ARG HUB_TRUST_ROOT_B64=
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -mod=readonly \
    -ldflags="-s -w -X main.version=${VERSION} -X github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub.defaultServerPublicKeyB64=${HUB_TRUST_ROOT_B64}" \
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
