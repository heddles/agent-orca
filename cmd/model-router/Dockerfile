# Dockerfile for the agent-orca model-router sidecar.
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
    go build -a -o bin/model-router ./cmd/model-router/main.go

FROM python:3.12-slim
WORKDIR /
COPY --from=builder /workspace/bin/model-router .
# Prometheus runtime decision logging metrics on :9091
EXPOSE 8080 8081 8082 9090 9091

# Run as a non-root numeric UID (same as distroless nonroot).
USER 65532:65532

ENTRYPOINT ["/model-router"]
