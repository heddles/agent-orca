# Build the oauth-proxy binary (a generic OAuth 2.0 credential manager for MCP servers).
FROM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY api/ api/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o bin/oauth-proxy ./cmd/oauth-proxy/main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/bin/oauth-proxy .
USER 65532:65532

ENTRYPOINT ["/oauth-proxy"]
