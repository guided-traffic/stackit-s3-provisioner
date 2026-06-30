# Build stage
FROM golang:1.26.3-alpine AS builder

# Build arguments for metadata
ARG BUILD_NUMBER
ARG GIT_COMMIT
ARG BUILD_TIME

# Set build arguments for cross-compilation
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application with cross-compilation support
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -a -installsuffix cgo \
    -ldflags="-w -s -X main.version=${BUILD_NUMBER:-dev} -X main.commit=${GIT_COMMIT:-unknown} -X main.buildTime=${BUILD_TIME:-unknown}" \
    -o manager ./cmd/main.go

# Final stage - using distroless for minimal attack surface
FROM gcr.io/distroless/static-debian12:nonroot

ARG BUILD_NUMBER
ARG GIT_COMMIT
ARG BUILD_TIME

LABEL org.opencontainers.image.title="StackIT S3 Provisioner" \
      org.opencontainers.image.description="A Kubernetes operator that provisions StackIT Object Storage buckets, workload credentials and isolation policies" \
      org.opencontainers.image.vendor="Guided Traffic" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.documentation="https://github.com/guided-traffic/stackit-s3-provisioner" \
      org.opencontainers.image.source="https://github.com/guided-traffic/stackit-s3-provisioner" \
      org.opencontainers.image.version="${BUILD_NUMBER:-dev}" \
      org.opencontainers.image.revision="${GIT_COMMIT:-unknown}" \
      org.opencontainers.image.created="${BUILD_TIME:-0}"

# distroless images run as non-root user 65532 (nonroot) by default
# distroless includes ca-certificates and tzdata

WORKDIR /app

# Copy the binary from builder stage
COPY --from=builder /app/manager .

# Expose metrics and health probe ports
EXPOSE 8080 8081

ENTRYPOINT ["./manager"]
