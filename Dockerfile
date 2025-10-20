# ---------------------------
# Stage 1: Build the binary
# ---------------------------
FROM golang:1.22-alpine AS builder

# Install git for fetching modules
RUN apk add --no-cache git ca-certificates

# Set working directory
WORKDIR /app

# Enable Go modules
ENV GO111MODULE=on

# Copy go.mod and go.sum first for caching dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build a statically linked binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o proxyserver .

# ---------------------------
# Stage 2: Minimal runtime
# ---------------------------
FROM alpine:3.20

# Add CA certificates for HTTPS requests
RUN apk add --no-cache ca-certificates

# Set working directory
WORKDIR /app

# Copy the statically compiled binary
COPY --from=builder /app/proxyserver .

# Expose the default port
EXPOSE 8080

# Set environment variables (can override at runtime)
ENV PORT=8080 \
    LOG_LEVEL=INFO \
    CACHE_TTL=2h \
    HEALTH_CHECK_INTERVAL=30m \
    REQUEST_TIMEOUT=30s \
    MAX_RETRIES=3

# Run the binary
ENTRYPOINT ["./proxyserver"]
