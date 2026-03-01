# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /station ./cmd/station

# Runtime stage
FROM alpine:3.21

# ffmpeg is called as a subprocess by the processor workers.
RUN apk add --no-cache ffmpeg ca-certificates

COPY --from=builder /station /usr/local/bin/station

EXPOSE 8080

ENTRYPOINT ["station"]
