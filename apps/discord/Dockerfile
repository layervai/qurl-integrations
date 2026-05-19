FROM node:22.21.0-alpine3.22@sha256:bd26af08779f746650d95a2e4d653b0fd3c8030c44284b6b98d701c9b5eb66b9

WORKDIR /app

# Ensure production-mode enforcement in index.js regardless of orchestrator env.
# Critical boot checks (HTTPS in BASE_URL, KEY_ENCRYPTION_KEY presence, etc.)
# key off this value.
ENV NODE_ENV=production

# Install deps as root so npm can write into /app/node_modules, then drop
# to the non-root `node` user for the runtime stage. `--omit=dev` keeps
# the production image free of jest + aws-sdk-client-mock; `--ignore-scripts`
# is safe now that the last native-compile dep (better-sqlite3) is gone —
# every remaining dependency is pure-JS with no postinstall steps.
# `npm cache clean --force` after install shaves the ~/.npm cache (~tens
# of MB) from the final image layer; the prior two-stage build dropped
# this incidentally via the `COPY --from=build node_modules` carry-over,
# so a single-stage Dockerfile has to clean explicitly.
COPY --chown=node:node package*.json ./
RUN npm ci --omit=dev --ignore-scripts \
  && npm cache clean --force \
  && chown -R node:node /app/node_modules

# Copy only what the runtime needs. A bare `COPY . .` invalidates the
# build cache on any repo file change (README, tests, CI config) even
# though they're never needed in the runtime image. Enumerating src/
# keeps layer caching effective across unrelated edits.
COPY --chown=node:node src/ ./src/

EXPOSE 3000
USER node

# Port is hardcoded at 3000: ${PORT:-3000} inside a Dockerfile CMD's exec
# form doesn't interpolate at runtime, and EXPOSE is already 3000. If the
# runtime overrides PORT, update EXPOSE + this line together.
# start-period=30s matches the 30s client.login() timeout in index.js —
# the /health endpoint returns 503 until the Discord gateway sends READY.
# NOTE: On Fargate this Dockerfile HEALTHCHECK is cosmetic — ECS uses the
# task-def `healthCheck` (re-added in the infra follow-up). Tune the
# task-def values, not these, for production behavior.
# The probe address (127.0.0.1) MUST match the listener's bind address in
# gateway-health.js — binding ::1 while probing 127.0.0.1 (or vice versa)
# is a classic silent-probe-failure mode.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:3000/health" || exit 1

# Run node directly (not `npm start`) so SIGTERM is delivered to the Node
# process itself — otherwise npm eats the signal and the container hard-kills
# after 10s on deploy, dropping in-flight requests and interrupting graceful
# shutdown.
CMD ["node", "src/index.js"]
