# Multi-stage build for the Causal Discovery UI
# Stage 1: Build the React SPA; Stage 2: Go BFF for serving SPA + API proxy

# ── Stage 1: React SPA Build ─────────────────────────────────────────────────
FROM node:20-alpine AS ui-builder

WORKDIR /ui
COPY ui/package.json ui/package-lock.json* ./
RUN npm install

COPY ui/index.html ui/vite.config.ts ui/tsconfig.json ./
COPY ui/src/ ./src/

RUN npm run build

# ── Stage 2: Go BFF Runtime ──────────────────────────────────────────────────
FROM golang:1.25-alpine AS go-builder

WORKDIR /build
COPY proxy/bff/main.go ./main.go
RUN CGO_ENABLED=0 GOOS=linux go build -a -o causal-bff ./main.go

# ── Stage 3: Final Image ─────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

WORKDIR /srv

# Copy the built SPA
COPY --from=ui-builder /ui/dist/ ./

# Copy the Go binary
COPY --from=go-builder /build/causal-bff /causal-bff

EXPOSE 8080
USER 65534:65534

ENTRYPOINT ["/causal-bff", "-port", "8080", "-static", "/srv"]