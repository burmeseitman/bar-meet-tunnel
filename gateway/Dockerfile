# Pinned builder; refresh the digest when updating Go.
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS builder
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
