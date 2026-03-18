# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod ./
# RUN go mod download # (Need to go mod tidy first locally)

COPY . .
RUN go build -o /bar-meet-gateway ./gateway/main.go

# Run stage
FROM alpine:latest
WORKDIR /

COPY --from=builder /bar-meet-gateway /bar-meet-gateway
EXPOSE 80 9000

CMD ["/bar-meet-gateway"]
