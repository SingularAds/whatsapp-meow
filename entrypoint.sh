#!/bin/sh
# entrypoint.sh — wrapper that restores / backs up the WhatsApp session DB
# to/from a GCS bucket so the bridge always runs SQLite on fast local disk.
#
# Why not use a GCS FUSE mount?
#   GCS FUSE translates every filesystem call into a GCS API call.  SQLite's
#   WAL mode issues dozens of small writes per message, each costing 50-200 ms
#   against the GCS API.  The result is a 2-3 minute message processing delay.
#   This script avoids that by copying the DB to local ephemeral disk at startup
#   and copying it back to GCS only on graceful shutdown (SIGTERM).
#
# Required env vars:
#   GCS_BACKUP_BUCKET   — GCS bucket name (no gs:// prefix), e.g. "whatsapp-bridge-sessions-504457548316"
#   DEFAULT_SESSION_ID  — session to restore, e.g. "smba"
#   DB_DIR              — local path, e.g. "/data/whatsapp"  (default: /data/whatsapp)
#
# The Cloud Run service account must have roles/storage.objectAdmin on the bucket.
# No extra tools needed — uses the GCS JSON REST API + the metadata server token.

set -e

_DB_DIR="${DB_DIR:-/data/whatsapp}"
_SESSION="${DEFAULT_SESSION_ID:-}"
_BUCKET="${GCS_BACKUP_BUCKET:-}"

# ── helpers ──────────────────────────────────────────────────────────────────

_gcs_token() {
    curl -sf \
        -H "Metadata-Flavor: Google" \
        "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
    | sed 's/.*"access_token":"\([^"]*\)".*/\1/'
}

_gcs_download() {
    local bucket="$1" object="$2" dest="$3"
    local token
    token=$(_gcs_token)
    # URL-encode slashes in the object name (%2F) if needed — not needed here.
    curl -sf \
        -H "Authorization: Bearer ${token}" \
        "https://storage.googleapis.com/storage/v1/b/${bucket}/o/${object}?alt=media" \
        -o "${dest}" 2>/dev/null
}

_gcs_upload() {
    local bucket="$1" object="$2" src="$3"
    local token
    token=$(_gcs_token)
    curl -sf \
        -X POST \
        -H "Authorization: Bearer ${token}" \
        -H "Content-Type: application/octet-stream" \
        --data-binary "@${src}" \
        "https://storage.googleapis.com/upload/storage/v1/b/${bucket}/o?uploadType=media&name=${object}" \
        > /dev/null 2>&1
}

# ── startup restore ───────────────────────────────────────────────────────────

if [ -n "$_BUCKET" ] && [ -n "$_SESSION" ]; then
    mkdir -p "$_DB_DIR"

    echo "[entrypoint] Attempting to restore ${_SESSION}.db from gs://${_BUCKET}/"

    # Main DB file
    if _gcs_download "$_BUCKET" "${_SESSION}.db" "${_DB_DIR}/${_SESSION}.db"; then
        echo "[entrypoint] Restored ${_SESSION}.db ($(wc -c < "${_DB_DIR}/${_SESSION}.db") bytes)"
    else
        echo "[entrypoint] No existing DB in GCS — starting fresh (first run or after explicit wipe)"
    fi

    # WAL and SHM sidecar files (may not exist — ignore failures)
    _gcs_download "$_BUCKET" "${_SESSION}.db-wal" "${_DB_DIR}/${_SESSION}.db-wal" || true
    _gcs_download "$_BUCKET" "${_SESSION}.db-shm" "${_DB_DIR}/${_SESSION}.db-shm" || true
else
    echo "[entrypoint] GCS_BACKUP_BUCKET or DEFAULT_SESSION_ID not set — skipping GCS restore"
fi

# ── graceful shutdown: back up DB before the container exits ─────────────────

_backup_and_exit() {
    echo "[entrypoint] SIGTERM received — backing up session DB to GCS before exit"
    if [ -n "$_BUCKET" ] && [ -n "$_SESSION" ] && [ -f "${_DB_DIR}/${_SESSION}.db" ]; then
        if _gcs_upload "$_BUCKET" "${_SESSION}.db" "${_DB_DIR}/${_SESSION}.db"; then
            echo "[entrypoint] Backup of ${_SESSION}.db complete"
        else
            echo "[entrypoint] WARNING: GCS backup failed — session will need re-pairing after next start"
        fi
    fi
    # Forward SIGTERM to the bridge process so it shuts down cleanly.
    kill -TERM "$_PID" 2>/dev/null || true
    wait "$_PID" 2>/dev/null || true
}

trap _backup_and_exit TERM INT

# ── launch bridge ─────────────────────────────────────────────────────────────

echo "[entrypoint] Starting whatsapp-bridge (session=${_SESSION:-unset} db_dir=${_DB_DIR})"
/app/whatsapp-bridge "$@" &
_PID=$!
wait "$_PID"

 