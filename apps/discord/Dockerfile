# Build stage — compile native modules (better-sqlite3)
FROM node:22.11.0-alpine3.20 AS build

RUN apk add --no-cache python3 make g++

WORKDIR /app
COPY package*.json ./
RUN npm ci --omit=dev

# Production stage — minimal image without build tools
FROM node:22.11.0-alpine3.20

WORKDIR /app

COPY --from=build /app/node_modules ./node_modules
COPY package*.json ./
COPY . .

RUN mkdir -p /app/data && chown -R node:node /app/data

EXPOSE 3000
USER node

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:3000/health || exit 1

# Run node directly (not `npm start`) so SIGTERM is delivered to the Node
# process itself — otherwise npm eats the signal and the container hard-kills
# after 10s on deploy, dropping in-flight requests and interrupting graceful
# shutdown.
CMD ["node", "src/index.js"]
