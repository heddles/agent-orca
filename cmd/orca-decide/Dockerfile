# Dockerfile for orca-decide, the local decision model agent dispatch asks which
# agent a request is for. It embeds through a llama.cpp server beside it; the
# model's weights are in cmd/orca-decide/Dockerfile.models.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
# A go.mod replace to a local directory needs that directory's go.mod before the download.
# BEGIN generated go-mod-replaces (hack/gen-docker-copies; run `make docker-deps`, do not edit)
COPY third_party/switchyard/bindings/go/go.mod third_party/switchyard/bindings/go/go.mod
COPY third_party/switchyard/bindings/go/go.sum third_party/switchyard/bindings/go/go.sum
# END generated go-mod-replaces
RUN go mod download
# BEGIN generated go-deps (hack/gen-docker-copies; run `make docker-deps`, do not edit)
COPY cmd/orca-decide/ cmd/orca-decide/
COPY internal/dispatch/decision/ internal/dispatch/decision/
COPY internal/dispatch/localdecider/ internal/dispatch/localdecider/
# END generated go-deps

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o bin/orca-decide ./cmd/orca-decide

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/bin/orca-decide .
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/orca-decide"]
