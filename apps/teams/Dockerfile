# Node 22.23.1 manifest digest (published 2026-07-14; includes linux/arm64).
# Keep the digest literal on every FROM for dependency-age verification.
FROM node:22.23.1-bookworm-slim@sha256:6c74791e557ce11fc957704f6d4fe134a7bc8d6f5ca4403205b2966bd488f6b3 AS build
WORKDIR /app
COPY package*.json ./
# Skip dependency lifecycle hooks. Add a reviewed, explicit rebuild only if
# a native dependency later proves it needs one.
RUN npm ci --ignore-scripts
COPY . ./
RUN npm run build
RUN test -f dist/server.js

FROM node:22.23.1-bookworm-slim@sha256:6c74791e557ce11fc957704f6d4fe134a7bc8d6f5ca4403205b2966bd488f6b3
ENV NODE_ENV=production
WORKDIR /app
COPY --from=build /app/package*.json ./
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force
COPY --from=build /app/dist ./dist
USER node
EXPOSE 3000
CMD ["node", "dist/server.js"]
