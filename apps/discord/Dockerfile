# Build stage — compile native modules (better-sqlite3)
FROM node:22-alpine AS build

RUN apk add --no-cache python3 make g++

WORKDIR /app
COPY package*.json ./
RUN npm ci --omit=dev

# Production stage — minimal image without build tools
FROM node:22-alpine

WORKDIR /app

COPY --from=build /app/node_modules ./node_modules
COPY package*.json ./
COPY . .

RUN mkdir -p /app/data && chown -R node:node /app/data

EXPOSE 3000
USER node
CMD ["npm", "start"]
