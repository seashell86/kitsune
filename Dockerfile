# Build stage
FROM golang:1.23-alpine AS builder

# Set working directory
WORKDIR /app

# Install build dependencies
RUN apk add --no-cache gcc musl-dev

# Copy go.mod and go.sum (if they exist)
COPY go.mod ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Run tests
RUN go test -v ./...

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o kitsune

# Final stage
FROM alpine:latest

# Add non-root user
RUN adduser -D kitsune

# Copy binary from builder
COPY --from=builder /app/kitsune /usr/local/bin/

# Switch to non-root user
USER kitsune

# Expose default port
EXPOSE 42069

# Run the binary
ENTRYPOINT ["kitsune"]
