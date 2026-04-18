# Build stage — compile native modules (better-sqlite3)
FROM node:22.21.0-alpine3.22 AS build

RUN apk add --no-cache python3 make g++

WORKDIR /app
COPY package*.json ./
# NOTE: --ignore-scripts would block supply-chain attacks via postinstall
# hooks, but better-sqlite3 needs its build script to fall back to
# compile-from-source when no prebuilt binary matches this arch. Kept
# scripts enabled for now; the build stage isolates them from the
# production image (see the FROM below).
RUN npm ci --omit=dev

# Production stage — minimal image without build tools
FROM node:22.21.0-alpine3.22

WORKDIR /app

# Ensure production-mode enforcement in index.js regardless of orchestrator env.
# Critical boot checks (HTTPS in BASE_URL, KEY_ENCRYPTION_KEY presence, etc.)
# key off this value.
ENV NODE_ENV=production

COPY --from=build --chown=node:node /app/node_modules ./node_modules
COPY --chown=node:node package*.json ./
# Copy only what the runtime needs. A bare `COPY . .` invalidates the
# build cache on any repo file change (README, tests, CI config) even
# though they're never needed in the runtime image. Enumerating src/
# keeps layer caching effective across unrelated edits.
COPY --chown=node:node src/ ./src/

RUN mkdir -p /app/data && chown -R node:node /app/data

EXPOSE 3000
USER node

# Port is hardcoded at 3000: ${PORT:-3000} inside a Dockerfile CMD's exec
# form doesn't interpolate at runtime, and EXPOSE is already 3000. If the
# runtime overrides PORT, update EXPOSE + this line together.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- "http://localhost:3000/health" || exit 1

# Run node directly (not `npm start`) so SIGTERM is delivered to the Node
# process itself — otherwise npm eats the signal and the container hard-kills
# after 10s on deploy, dropping in-flight requests and interrupting graceful
# shutdown.
CMD ["node", "src/index.js"]
