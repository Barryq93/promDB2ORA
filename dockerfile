# Use the official Golang image as the base image
FROM golang:1.20-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy the Go module files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the rest of the application code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o db-monitoring-app cmd/main.go

# Use a smaller base image for the final stage
FROM alpine:latest

# Install dependencies for DB2 and Oracle clients (if needed)
RUN apk --no-cache add libaio libnsl

# Set the working directory
WORKDIR /app

# Copy the built binary from the builder stage
COPY --from=builder /app/db-monitoring-app .

# Copy the configuration file
COPY config.yml .

# Copy the certificates directory (if needed)
COPY certs/ ./certs/

# Expose the port the application will run on
EXPOSE 8080

# Set the entry point for the container
ENTRYPOINT ["./db-monitoring-app", "-config", "config.yml"]