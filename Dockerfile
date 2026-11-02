# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.sum ./
# A go.mod replace to a local directory needs that directory's go.mod before the download.
# BEGIN generated go-mod-replaces (hack/gen-docker-copies; run `make docker-deps`, do not edit)
COPY third_party/switchyard/bindings/go/go.mod third_party/switchyard/bindings/go/go.mod
COPY third_party/switchyard/bindings/go/go.sum third_party/switchyard/bindings/go/go.sum
# END generated go-mod-replaces
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source
# BEGIN generated go-deps (hack/gen-docker-copies; run `make docker-deps`, do not edit)
COPY api/v1alpha1/ api/v1alpha1/
COPY cmd/*.go cmd/
COPY internal/acpfs/ internal/acpfs/
COPY internal/apiserver/ internal/apiserver/
COPY internal/checkpoint/ internal/checkpoint/
COPY internal/controller/ internal/controller/
COPY internal/egress/ internal/egress/
COPY internal/executor/ internal/executor/
COPY internal/framework/ internal/framework/
COPY internal/githubapp/ internal/githubapp/
COPY internal/mcp/ internal/mcp/
COPY internal/oauth/ internal/oauth/
COPY internal/openshell/ internal/openshell/
COPY internal/podbuilder/ internal/podbuilder/
COPY internal/postgresql/ internal/postgresql/
COPY internal/rag/*.go internal/rag/
COPY internal/ratelimit/ internal/ratelimit/
COPY internal/router/*.go internal/router/
COPY internal/security/ internal/security/
COPY internal/state/ internal/state/
COPY internal/webhook/ internal/webhook/
# END generated go-deps

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o bin/manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/bin/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
