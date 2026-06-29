# Build stage
FROM node:20-alpine AS builder

WORKDIR /app

# Copy package files
COPY package.json ./

# Install dependencies
RUN npm install

# Copy source code
COPY . .

# Build the React app
RUN npm run build

# Serve stage
FROM caddy:2-alpine

# Pre-create writable directories for the non-root user
RUN mkdir -p /data /config /tmp && \
    chown -R 65534:65534 /srv /data /config /tmp

# Copy built React app to Caddy's default serve directory
COPY --from=builder --chown=65534:65534 /app/dist /srv

# Copy Caddyfile
COPY --chown=65534:65534 Caddyfile /etc/caddy/Caddyfile

USER 65534

EXPOSE 8080

CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile"]
