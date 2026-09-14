# Pinned builder; refresh the digest when updating Go.
FROM golang:1.26.8-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY gateway ./gateway
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bar-meet-gateway ./gateway

# No shell or OS package set in the runtime image.
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /bar-meet-gateway /bar-meet-gateway
USER 65532:65532
ENV PUBLIC_ADDR=:8080
EXPOSE 8080 9000
ENTRYPOINT ["/bar-meet-gateway"]
