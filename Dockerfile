FROM golang:1.25 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/whatsapp-bridge .


FROM alpine:3.20

# curl is needed by entrypoint.sh to talk to the GCS JSON API and the
# metadata server for the Cloud Run service-account token.
# dos2unix converts Windows CRLF line endings to LF so the shell script runs
# correctly when built on a Windows host.
RUN apk add --no-cache ca-certificates curl dos2unix ffmpeg tzdata \
    && addgroup -S app \
    && adduser -S -G app app

WORKDIR /app

COPY --from=builder /out/whatsapp-bridge /app/whatsapp-bridge
COPY entrypoint.sh /app/entrypoint.sh
RUN dos2unix /app/entrypoint.sh && chmod +x /app/entrypoint.sh

ENV PORT=3020 \
    DB_DIR=/data/whatsapp \
    LOG_LEVEL=INFO

RUN mkdir -p /data/whatsapp \
    && chown -R app:app /app /data

EXPOSE 3020

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- http://127.0.0.1:${PORT}/api/health >/dev/null 2>&1 || exit 1

USER app

# entrypoint.sh restores smba.db from GCS on startup (fast local disk, no FUSE)
# and backs it up to GCS on graceful shutdown (SIGTERM).
# Set GCS_BACKUP_BUCKET=<bucket-name> in Cloud Run env to enable.
# Without that env var the bridge runs exactly as before (no change in behaviour).
ENTRYPOINT ["/app/entrypoint.sh"]
 