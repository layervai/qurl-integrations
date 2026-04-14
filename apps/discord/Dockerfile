FROM node:22-alpine

# Install build dependencies for better-sqlite3
RUN apk add --no-cache python3 make g++

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install dependencies
RUN npm ci --omit=dev

# Copy source
COPY . .

# Create data directory for SQLite and set ownership
RUN mkdir -p /app/data && chown -R node:node /app/data

# Expose port
EXPOSE 3000

# Run as non-root
USER node

# Start the bot
CMD ["npm", "start"]
