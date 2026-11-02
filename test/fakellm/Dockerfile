# Deterministic OpenAI-compatible server for e2e tests (see main.go).
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
COPY test/fakellm/ test/fakellm/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -o bin/fakellm ./test/fakellm

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/bin/fakellm /fakellm
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/fakellm"]
