# Build the operator binary
FROM registry.suse.com/bci/golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -o image-operator ./cmd

FROM scratch
WORKDIR /
COPY --from=builder /etc/ssl/ca-bundle.pem /etc/ssl/ca-bundle.pem
COPY --from=builder /workspace/image-operator .
USER 65532:65532
ENTRYPOINT ["/image-operator"]
