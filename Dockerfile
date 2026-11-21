# Multi-stage, CGO-free multi-arch build (linux/amd64 + linux/arm64)
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/mcpgram ./cmd/mcpgram

FROM alpine:3.21

RUN apk add --no-cache ca-certificates wget tzdata \
    && adduser -D -u 10001 mcpgram \
    && mkdir -p /data \
    && chown -R mcpgram:mcpgram /data

COPY --from=builder /out/mcpgram /usr/local/bin/mcpgram

USER mcpgram
WORKDIR /data

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/mcpgram"]
CMD ["serve"]
