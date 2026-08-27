ARG TARGETOS
ARG TARGETARCH

FROM golang:1.22-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod ./
COPY go.sum ./
COPY *.go ./
COPY openapi.yaml ./
COPY web ./web

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/codex-meter .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/codex-meter /app/codex-meter
COPY config.example.json /app/config.example.json

ENV BIND_ADDR=0.0.0.0:8123

EXPOSE 8123

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q --spider "http://127.0.0.1:8123${BASE_PATH:-}/healthz" || exit 1

ENTRYPOINT ["/app/codex-meter"]
