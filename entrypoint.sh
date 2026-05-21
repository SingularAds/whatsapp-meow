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
#   BACKUP_INTERVAL     — seconds between periodic hot backups (default: 120)
#
# The Cloud Run service account must have roles/storage.objectAdmin on the bucket.
# No extra tools needed — uses the GCS JSON REST API + the metadata server token.
#
# Session-persistence guarantees:
#   1. Startup:  restore .db + .db-wal + .db-shm from GCS (SQLite auto-applies WAL on open).
#   2. Shutdown: stop bridge first (lets SQLite checkpoint WAL), then upload all three files.
#   3. Periodic: hot-backup every BACKUP_INTERVAL seconds so a SIGKILL loses at most
#      that many seconds of session/message data instead of everything since last restart.

set -e

_DB_DIR="${DB_DIR:-/data/whatsapp}"
_SESSION="${DEFAULT_SESSION_ID:-}"
_BUCKET="${GCS_BACKUP_BUCKET:-}"
_BACKUP_INTERVAL="${BACKUP_INTERVAL:-120}"
_BACKUP_PID=""

# Network hardening for GCS calls (seconds).
_CURL_CONNECT_TIMEOUT="${CURL_CONNECT_TIMEOUT:-5}"
_CURL_MAX_TIME="${CURL_MAX_TIME:-30}"

# ── helpers ──────────────────────────────────────────────────────────────────

_gcs_token() {
    local payload token
    payload=$(curl -sS \
        --connect-timeout "${_CURL_CONNECT_TIMEOUT}" \
        --max-time "${_CURL_MAX_TIME}" \
        -H "Metadata-Flavor: Google" \
        "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
    ) || {
        echo "[entrypoint] ERROR: unable to fetch metadata token"
        return 1
    }

    token=$(printf '%s' "$payload" | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    if [ -z "$token" ]; then
        echo "[entrypoint] ERROR: metadata token payload did not contain access_token"
        return 1
    fi

    printf '%s' "$token"
}

_gcs_download() {
    local bucket="$1" object="$2" dest="$3"
    local token
    token=$(_gcs_token) || return 1
    # URL-encode slashes in the object name (%2F) if needed — not needed here.
    curl -sS \
        --connect-timeout "${_CURL_CONNECT_TIMEOUT}" \
        --max-time "${_CURL_MAX_TIME}" \
        --retry 3 \
        --retry-delay 1 \
        -H "Authorization: Bearer ${token}" \
        "https://storage.googleapis.com/storage/v1/b/${bucket}/o/${object}?alt=media" \
        -o "${dest}" 2>/dev/null
}

# _gcs_upload_multi fetches a single token and uploads multiple files in one shot.
# Usage: _gcs_upload_multi bucket object1 src1 [object2 src2 ...]
_gcs_upload_multi() {
    local bucket="$1"; shift
    local token
    token=$(_gcs_token) || return 1
    while [ $# -ge 2 ]; do
        local object="$1" src="$2"; shift 2
        [ -f "$src" ] || continue

        local tmp status
        tmp="$(mktemp)"
        status=$(curl -sS \
            --connect-timeout "${_CURL_CONNECT_TIMEOUT}" \
            --max-time "${_CURL_MAX_TIME}" \
            --retry 3 \
            --retry-delay 1 \
            -w "%{http_code}" \
            -o "$tmp" \
            -X POST \
            -H "Authorization: Bearer ${token}" \
            -H "Content-Type: application/octet-stream" \
            --data-binary "@${src}" \
            "https://storage.googleapis.com/upload/storage/v1/b/${bucket}/o?uploadType=media&name=${object}" \
        ) || status="000"

        case "$status" in
            ''|*[!0-9]*) status="000" ;;
        esac

        if [ "$status" -lt 200 ] || [ "$status" -ge 300 ]; then
            echo "[entrypoint] WARNING: upload failed for ${object} (http=${status})"
            # Print a compact server response to aid debugging (permission, quota, etc).
            head -c 300 "$tmp" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g' | sed 's/^/[entrypoint] ERROR: /'
            rm -f "$tmp"
            continue
        fi

        rm -f "$tmp"
    done
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

# ── upload helper: back up main DB + WAL + SHM to GCS ────────────────────────

_upload_db() {
    if [ -z "$_BUCKET" ] || [ -z "$_SESSION" ]; then
        return 0
    fi
    local db="${_DB_DIR}/${_SESSION}.db"
    [ -f "$db" ] || return 1

    local wal="${db}-wal" shm="${db}-shm"

    # Upload main DB plus any WAL/SHM files with a single metadata-server token.
    # SQLite uses .db + .db-wal together to reconstruct the full committed state
    # on the next open, so they must be kept in sync.
    _gcs_upload_multi "$_BUCKET" \
        "${_SESSION}.db"     "$db" \
        "${_SESSION}.db-wal" "$wal" \
        "${_SESSION}.db-shm" "$shm"

    echo "[entrypoint] Uploaded ${_SESSION}.db ($(wc -c < "$db") bytes)"
    return 0
}

# ── periodic hot backup (safety net against SIGKILL / OOM kill) ──────────────
# Runs in the background and uploads every BACKUP_INTERVAL seconds while the
# bridge is alive.  In WAL mode the .db + .db-wal pair is always consistent, so
# copying both files gives a safe point-in-time snapshot even with a live DB.

_periodic_backup() {
    while true; do
        sleep "${_BACKUP_INTERVAL}"
        # Exit loop if bridge process no longer exists.
        kill -0 "${_PID}" 2>/dev/null || break
        if [ -n "$_BUCKET" ] && [ -n "$_SESSION" ] && [ -f "${_DB_DIR}/${_SESSION}.db" ]; then
            echo "[entrypoint] Periodic backup of ${_SESSION}.db"
            _upload_db || true
        fi
    done
}

# ── graceful shutdown: stop bridge first, THEN back up ───────────────────────
#
# BUG FIXED: the previous version uploaded the DB *before* killing the bridge.
# While the bridge was still running, SQLite's WAL file held uncommitted pages
# that had not yet been flushed into the main .db file.  The backup therefore
# captured a stale snapshot, so after the next restart the session appeared
# incomplete or missing entirely.
#
# Correct order:
#   1. Send SIGTERM to bridge → bridge closes its SQLite connection cleanly,
#      which triggers an automatic WAL checkpoint and flushes all data into
#      the main .db file.
#   2. Wait for bridge process to fully exit.
#   3. Upload the now-complete .db (plus WAL/SHM as belt-and-suspenders).

_backup_and_exit() {
    echo "[entrypoint] SIGTERM received — stopping bridge before GCS backup"

    # Kill the periodic-backup helper so it doesn't race with the final upload.
    [ -n "${_BACKUP_PID}" ] && kill "${_BACKUP_PID}" 2>/dev/null || true

    # Stop the bridge and wait for it to flush/checkpoint the WAL.
    kill -TERM "${_PID}" 2>/dev/null || true
    wait "${_PID}" 2>/dev/null || true
    echo "[entrypoint] Bridge stopped — uploading DB to GCS"

    _upload_db
}

trap _backup_and_exit TERM INT

# ── launch bridge ─────────────────────────────────────────────────────────────

echo "[entrypoint] Starting whatsapp-bridge (session=${_SESSION:-unset} db_dir=${_DB_DIR})"
/app/whatsapp-bridge "$@" &
_PID=$!

# Start periodic hot-backup in the background (safety net against SIGKILL).
if [ -n "$_BUCKET" ] && [ -n "$_SESSION" ]; then
    _periodic_backup &
    _BACKUP_PID=$!
fi

wait "$_PID"

# Bridge exited on its own (clean restart/OOM).  Kill the periodic-backup helper
# and do a final upload so Cloud Run's next revision starts with a fresh DB.
[ -n "${_BACKUP_PID}" ] && kill "${_BACKUP_PID}" 2>/dev/null || true
echo "[entrypoint] Bridge exited — performing final GCS backup"
_upload_db || true

 