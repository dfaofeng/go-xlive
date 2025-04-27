# Dockerfile for the event service (cmd/event/Dockerfile)

# ---- Build Stage ----
# Use an official Go runtime as a parent image
FROM golang:1.21-alpine AS builder

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go mod and sum files to download dependencies first
# This leverages Docker cache layers
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code into the container
COPY . .

# Build the Go app specifically for the event service
# Statically linked binary suitable for minimal runtime images
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o event ./cmd/event/main.go

# ---- Runtime Stage ----
# Start from a minimal base image
FROM alpine:latest

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy the pre-built binary file from the builder stage
COPY --from=builder /app/event .

# Copy configuration files if needed by the application at runtime
# COPY configs/config.yaml ./configs/config.yaml

# Expose port if the service listens on one (e.g., for gRPC)
# Replace with the actual port used by the event service if applicable
# EXPOSE 50054

# Command to run the executable
# Ensure the application reads configuration from environment variables or a path accessible within the container
CMD ["./event"]