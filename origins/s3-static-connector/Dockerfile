# syntax=docker/dockerfile:1

# Stage 1: pinned Envoy (Ubuntu base, BoringSSL). We copy only the
# /usr/local/bin/envoy binary out, so base-image bumps must keep the runtime
# behavior contract green on both arches to catch glibc/library drift.
FROM envoyproxy/envoy:v1.38.2@sha256:af7c3dfdbe10de65cb2db5839ce1adf8904ee41c9d372076d62acab43af6ec41 AS envoy

# Stage 2: pinned nginx-unprivileged (Debian bookworm / glibc) runtime base.
FROM nginxinc/nginx-unprivileged:1.27.3@sha256:6d51e4a8e10dfe334f8e2d15bb81b1ed2580ea9cb874b644acc720eda7022b54
LABEL org.opencontainers.image.source="https://github.com/layervai/qurl-integrations"

USER root
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends bash ca-certificates gettext-base tini; \
    rm -rf /var/lib/apt/lists/*; \
    install -d -o 65532 -g 65532 /etc/qurl/rendered

COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy
COPY templates/ /etc/qurl/templates/
COPY render.sh entrypoint.sh cachectl.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/render.sh /usr/local/bin/entrypoint.sh /usr/local/bin/cachectl.sh /usr/local/bin/envoy; \
    ln -s /usr/local/bin/cachectl.sh /usr/local/bin/qurl-origin-cachectl

ENV LISTEN_ADDR=127.0.0.1:8080 \
    ENVOY_LISTEN_ADDR=127.0.0.1:9090 \
    INDEX_DOCUMENT=index.html \
    CACHE_MAX_SIZE=1g
# CACHE_DEFAULT_TTL is intentionally unset: cache per the object's Cache-Control
# (nginx default). Set it only to force a fallback TTL for metadata-less objects.

# Unprivileged uid matches the qurl-connector sidecar (65532). Both listeners
# bind loopback only (set in the rendered configs); no ports are EXPOSEd —
# the connector reaches nginx over the shared network namespace.
# No Docker HEALTHCHECK: the connector/orchestrator owns liveness and this image
# is not intended to be probed directly outside the shared network namespace.
USER 65532:65532
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
