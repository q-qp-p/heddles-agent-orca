# Dockerfile for the agent-orca model-router sidecar.
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
COPY api/v1alpha1/ api/v1alpha1/
COPY cmd/model-router/ cmd/model-router/
COPY internal/acpfs/ internal/acpfs/
COPY internal/executor/ internal/executor/
COPY internal/framework/ internal/framework/
COPY internal/mcp/ internal/mcp/
COPY internal/oauth/ internal/oauth/
COPY internal/podbuilder/ internal/podbuilder/
COPY internal/ratelimit/ internal/ratelimit/
COPY internal/router/*.go internal/router/
COPY internal/security/*.go internal/security/
COPY internal/state/ internal/state/
# END generated go-deps

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
