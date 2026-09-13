#!/bin/sh
set -e

# The container starts as root only to fix the ownership of the mounted
# configuration and data directories; the application itself always runs as
# the unprivileged codex user. Host bind mounts created by Docker (or by
# earlier root-running versions) become writable for the app this way.
mkdir -p /app/config /app/data
if [ "$(id -u)" = "0" ]; then
    chown -R codex:codex /app/config /app/data 2>/dev/null || true
    exec su-exec codex:codex /app/codex-meter
fi

exec /app/codex-meter
