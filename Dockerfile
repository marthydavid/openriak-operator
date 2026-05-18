# Build stage
FROM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Copy go mod and sum files
COPY go.mod go.mod
COPY go.sum go.sum

# Cache deps before building
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -ldflags="-w -s" -o manager main.go

# Final image - using alpine to include kubectl
FROM alpine:latest
RUN apk --no-cache add ca-certificates kubectl

WORKDIR /

# Copy the binary from builder
COPY --from=builder /workspace/manager .

# Create non-root user
RUN addgroup -S operator && adduser -S operator -G operator
USER operator

ENTRYPOINT ["/manager"]
