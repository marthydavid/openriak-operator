# Build stage — cross-compile for the target platform on the host platform.
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-w -s" -o manager cmd/main.go

# Final image — alpine is multi-arch, kubectl is available for amd64 and arm64.
FROM alpine:3.20
RUN apk --no-cache add ca-certificates kubectl

WORKDIR /

COPY --from=builder /workspace/manager .

RUN addgroup -S operator && adduser -S operator -G operator
USER operator

ENTRYPOINT ["/manager"]
