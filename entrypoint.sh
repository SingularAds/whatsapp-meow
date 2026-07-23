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
#   DEFAULT_SESSION_ID  — the default session, e.g. "smba" (restore fallback only)
#   DB_DIR              — local path, e.g. "/data/whatsapp"  (default: /data/whatsapp)
#   BACKUP_INTERVAL     — seconds between periodic hot backups (default: 120)
#
# The Cloud Run service account must have roles/storage.objectAdmin on the bucket.
# No extra tools needed — uses the GCS JSON REST API + the metadata server token.
#
# Session-persistence guarantees:
#   1. Startup:  restore EVERY session's .db from GCS.
#   2. Shutdown: stop bridge first, then back up all of them.
#   3. Periodic: hot-backup every BACKUP_INTERVAL seconds so a SIGKILL loses at most
#      that many seconds of session/message data instead of everything since last restart.
#
# MULTI-SESSION (fixed 2026-07-22): this script used to back up and restore ONLY
# "${DEFAULT_SESSION_ID}.db". Every other session — additional global onboarding
# numbers (smbb, …), the demo number, and every paired business (biz-<number>) —
# lived only on Cloud Run's EPHEMERAL container disk and was therefore silently
# destroyed by each redeploy/restart, unlinking those numbers with no error
# anywhere. It now persists every *.db in DB_DIR.
#
# CONSISTENT SNAPSHOTS (fixed 2026-07-23): the multi-session fix above still
# corrupted two live sessions ("demo", "smbb") within a day. Root cause: each
# hot backup uploaded the raw .db/.db-wal/.db-shm files as three SEPARATE HTTP
# calls while the bridge kept writing — a message landing mid-backup could tear
# a page mid-copy or pair a base file with a WAL from a different moment.
# Confirmed by downloading the actual corrupted GCS objects and running
# PRAGMA integrity_check locally — "database disk image is malformed" even on
# the .db file ALONE, with no WAL involved.
#
# Fix: back up via SQLite's own `.backup` command instead of copying the live
# files directly. `.backup` uses SQLite's internal locking to produce a single,
# guaranteed-consistent snapshot no matter what the bridge is concurrently
# writing (verified: still integrity_check-clean under 500 concurrent writes
# during the backup). The resulting file is fully self-contained, so sessions
# are now backed up/restored as ONE file each — no .db-wal/.db-shm sidecars.

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

    local tmp status
    tmp="$(mktemp)"
    status=$(curl -sS \
        --connect-timeout "${_CURL_CONNECT_TIMEOUT}" \
        --max-time "${_CURL_MAX_TIME}" \
        --retry 3 \
        --retry-delay 1 \
        -w "%{http_code}" \
        -o "$tmp" \
        -H "Authorization: Bearer ${token}" \
        "https://storage.googleapis.com/storage/v1/b/${bucket}/o/${object}?alt=media" 2>/dev/null
    ) || status="000"

    case "$status" in
        ''|*[!0-9]*) status="000" ;;
    esac

    if [ "$status" -ge 200 ] && [ "$status" -lt 300 ]; then
        mv "$tmp" "$dest"
        return 0
    else
        rm -f "$tmp"
        return 1
    fi
}

# _gcs_list prints every object name in the bucket, one per line.
# Used to discover ALL session DBs at startup (we cannot know the session ids in
# advance — businesses pair at runtime as biz-<number>).
_gcs_list() {
    local bucket="$1" token payload page_token url
    token=$(_gcs_token) || return 1
    page_token=""
    while : ; do
        url="https://storage.googleapis.com/storage/v1/b/${bucket}/o?fields=items(name),nextPageToken&maxResults=1000"
        [ -n "$page_token" ] && url="${url}&pageToken=${page_token}"
        payload=$(curl -sS \
            --connect-timeout "${_CURL_CONNECT_TIMEOUT}" \
            --max-time "${_CURL_MAX_TIME}" \
            --retry 3 \
            --retry-delay 1 \
            -H "Authorization: Bearer ${token}" \
            "$url" 2>/dev/null) || return 1
        # One "name": "..." per line.
        printf '%s' "$payload" | tr ',' '\n' \
            | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
        page_token=$(printf '%s' "$payload" \
            | sed -n 's/.*"nextPageToken"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
        [ -n "$page_token" ] || break
    done
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

# Restore ONE session. Each backed-up .db is a self-contained `.backup` snapshot
# (see _snapshot_db below) — no .db-wal/.db-shm sidecars to restore alongside it.
# Returns 0 if the DB came down, 1 otherwise.
_restore_session() {
    local base="$1"
    if _gcs_download "$_BUCKET" "${base}.db" "${_DB_DIR}/${base}.db"; then
        echo "[entrypoint] Restored ${base}.db ($(wc -c < "${_DB_DIR}/${base}.db") bytes)"
        return 0
    fi
    return 1
}

if [ -n "$_BUCKET" ]; then
    mkdir -p "$_DB_DIR"

    echo "[entrypoint] Restoring ALL session DBs from gs://${_BUCKET}/"
    _restored=0
    _objects=$(_gcs_list "$_BUCKET") || _objects=""

    if [ -n "$_objects" ]; then
        # Iterate main .db objects only; sidecars are pulled by _restore_session.
        for _obj in $_objects; do
            case "$_obj" in
                *.db) ;;
                *) continue ;;
            esac
            _base="${_obj%.db}"
            if _restore_session "$_base"; then
                _restored=$((_restored + 1))
            fi
        done
        echo "[entrypoint] Restored ${_restored} session DB(s)"
    else
        # Listing failed (permissions/network) or the bucket is empty. Fall back to
        # the single default session so this can never be WORSE than the old
        # behaviour, and say so loudly — a silent fallback is how sessions got lost.
        echo "[entrypoint] WARNING: could not list gs://${_BUCKET}/ — falling back to ${_SESSION:-<unset>} only"
        if [ -n "$_SESSION" ]; then
            _restore_session "$_SESSION" \
                || echo "[entrypoint] No existing DB in GCS — starting fresh (first run or after explicit wipe)"
        fi
    fi
else
    echo "[entrypoint] GCS_BACKUP_BUCKET not set — skipping GCS restore"
fi

# ── upload helper: back up every session as a consistent snapshot ────────────

# _snapshot_db writes a GUARANTEED-CONSISTENT copy of a live session db to
# $2, using SQLite's own ".backup" command (not a raw file copy).
#
# WHY THIS EXISTS (found 2026-07-23, both "demo" and "smbb" corrupted in prod):
# the previous approach uploaded the raw .db/.db-wal/.db-shm files as three
# SEPARATE HTTP calls while the bridge kept writing to a live WAL-mode
# database. A message landing mid-backup could tear a page mid-copy or pair a
# base file with a WAL from a different moment. Confirmed by downloading the
# actual corrupted objects from GCS and running PRAGMA integrity_check locally
# — it failed even on the .db file ALONE, with no WAL involved, i.e. the base
# file copy itself was torn, not just mismatched against its WAL.
#
# `.backup` sidesteps this entirely: it uses SQLite's own internal locking to
# produce one self-contained, always-valid snapshot, regardless of what the
# live database is concurrently doing. Verified locally: integrity_check-clean
# even while 500 writes were hammering the source db during the backup call.
# Safe to run from a second process — SQLite is explicitly designed for
# multiple connections against the same WAL-mode file.
#
# Returns 1 (never fatal) if sqlite3 is missing or the backup call fails —
# caller falls back to skipping that session this round rather than uploading
# something unverified.
_snapshot_db() {
    local db="$1" dest="$2"
    if ! command -v sqlite3 >/dev/null 2>&1; then
        echo "[entrypoint] WARNING: sqlite3 CLI not available — cannot safely back up ${db}, skipping this round"
        return 1
    fi
    if ! sqlite3 "$db" ".backup '${dest}'" >/dev/null 2>&1; then
        echo "[entrypoint] WARNING: snapshot failed for ${db} — skipping this round (previous GCS backup, if any, is untouched)"
        return 1
    fi
    return 0
}

_upload_db() {
    if [ -z "$_BUCKET" ]; then
        return 0
    fi

    # Every session the bridge currently holds — the default global number, any
    # additional global numbers, the demo line, and each paired business.
    local db base count names snap_dir
    count=0
    names=""
    snap_dir="$(mktemp -d)"
    set --
    for db in "${_DB_DIR}"/*.db; do
        [ -f "$db" ] || continue          # no matches → literal glob, skip
        base="${db##*/}"; base="${base%.db}"
        # Skip this session's upload entirely on a failed snapshot, rather than
        # uploading a raw (possibly torn) copy — an old, still-good GCS backup
        # is safer than a fresh, corrupted one.
        _snapshot_db "$db" "${snap_dir}/${base}.db" || continue
        set -- "$@" "${base}.db" "${snap_dir}/${base}.db"
        count=$((count + 1))
        names="${names} ${base}"
    done

    if [ "$count" -eq 0 ]; then
        rm -rf "$snap_dir"
        return 1
    fi

    # One metadata-server token for the whole batch.
    _gcs_upload_multi "$_BUCKET" "$@"
    echo "[entrypoint] Uploaded ${count} session DB(s):${names}"
    rm -rf "$snap_dir"
    return 0
}

# ── periodic hot backup (safety net against SIGKILL / OOM kill) ──────────────
# Runs in the background and uploads every BACKUP_INTERVAL seconds while the
# bridge is alive. Each session is snapshotted via SQLite's own .backup command
# immediately before upload (see _snapshot_db) so what gets uploaded is always
# a single, internally-consistent file — safe regardless of concurrent writes.

_periodic_backup() {
    while true; do
        sleep "${_BACKUP_INTERVAL}"
        # Exit loop if bridge process no longer exists.
        kill -0 "${_PID}" 2>/dev/null || break
        if [ -n "$_BUCKET" ]; then
            echo "[entrypoint] Periodic backup of all session DBs"
            _upload_db || true      # no-op when no session DB exists yet
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
#   3. Upload the now-complete .db (via _snapshot_db's .backup, same as the
#      periodic path — cheap and safe even with no concurrent writer left).

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
if [ -n "$_BUCKET" ]; then
    _periodic_backup &
    _BACKUP_PID=$!
fi

wait "$_PID"

# Bridge exited on its own (clean restart/OOM).  Kill the periodic-backup helper
# and do a final upload so Cloud Run's next revision starts with a fresh DB.
[ -n "${_BACKUP_PID}" ] && kill "${_BACKUP_PID}" 2>/dev/null || true
echo "[entrypoint] Bridge exited — performing final GCS backup"
_upload_db || true

 