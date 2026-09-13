# No `# syntax=` directive: the builtin BuildKit frontend supports everything
# used here, which keeps local builds working without fetching the frontend
# image from Docker Hub.

FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG APP_VERSION=dev
ARG APP_COMMIT=unknown
ARG APP_BUILD_TIME

WORKDIR /src

COPY go.mod ./
COPY go.sum ./
COPY *.go ./
COPY openapi.yaml ./
COPY web ./web

RUN BUILD_TIMESTAMP="${APP_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
    -ldflags="-s -w -X main.appVersion=${APP_VERSION} -X main.appCommit=${APP_COMMIT} -X main.appBuildTime=${BUILD_TIMESTAMP}" \
    -o /out/codex-meter .

FROM alpine:3.21

ARG APP_VERSION=dev
ARG APP_COMMIT=unknown

LABEL org.opencontainers.image.version="${APP_VERSION}" \
      org.opencontainers.image.revision="${APP_COMMIT}"

RUN apk add --no-cache ca-certificates tzdata su-exec \
    && addgroup -S codex \
    && adduser -S -G codex -u 1000 codex

WORKDIR /app

COPY --from=builder /out/codex-meter /app/codex-meter
COPY config.example.json /app/config.example.json
COPY docker-entrypoint.sh /app/docker-entrypoint.sh

RUN chmod 0755 /app/docker-entrypoint.sh

ENV BIND_ADDR=0.0.0.0:8123

EXPOSE 8123

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q --spider "http://127.0.0.1:8123${BASE_PATH:-}/healthz" || exit 1

# The entrypoint fixes mount ownership and then drops to the codex user, so
# the server process never runs as root.
ENTRYPOINT ["/app/docker-entrypoint.sh"]
