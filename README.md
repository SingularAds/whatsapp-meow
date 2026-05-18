# dando-whatsapp-meow

## Production Deployment (GCP Cloud Run)

### Critical: Environment Variables

The following env vars **must** be set on the Cloud Run service. If any are missing the bridge will use the local defaults and fail silently.

| Variable | Required | Example |
|---|---|---|
| `WEBHOOK_URL` | **YES** | `https://boomreception-api-504457548316.europe-west1.run.app` |
| `WEBHOOK_SECRET` | YES | `boom2027` |
| `BRIDGE_USERNAME` | YES | `recepte` |
| `BRIDGE_PASSWORD` | YES | `boom2027` |
| `DEFAULT_SESSION_ID` | YES | `smba` |
| `DB_DIR` | YES | `/data/whatsapp` |
| `PUBLIC_URL` | YES | `https://whatsapp-bridge-504457548316.us-central1.run.app` |
| `LOG_LEVEL` | no | `INFO` |

**WEBHOOK_URL is the most common misconfiguration.** If it is missing or set to `localhost`, all incoming WhatsApp messages are silently discarded and the Python backend receives nothing. The `/api/health` endpoint returns `webhook_ok: false` when localhost is detected — check it immediately after any deployment.

### Critical: SQLite Session Persistence

Cloud Run containers have an **ephemeral filesystem**. The WhatsApp session (`smba.db`) is lost on every container restart, redeployment, or scale-to-zero event. Without persistent storage the session must be re-paired after every restart.

**Fix: Mount a GCS bucket at `/data/whatsapp`**

1. Create a GCS bucket (in the same region as the bridge, e.g. `us-central1`):
   ```
   gsutil mb -l us-central1 gs://whatsapp-bridge-sessions-<project-id>
   ```

2. Grant the Cloud Run service account `Storage Object Admin` on the bucket:
   ```
   gsutil iam ch serviceAccount:<SA_EMAIL>:objectAdmin gs://whatsapp-bridge-sessions-<project-id>
   ```

3. In the Cloud Run service configuration → **Volume Mounts** → Add Volume:
   - Type: **Cloud Storage bucket**
   - Bucket: `whatsapp-bridge-sessions-<project-id>`
   - Mount path: `/data/whatsapp`

4. Set `DB_DIR=/data/whatsapp` in the service environment variables.

After this the `smba.db` file survives container restarts and redeployments. You only need to pair the device once.

### Verify after deployment

```bash
curl https://whatsapp-bridge-504457548316.us-central1.run.app/api/health
```

Expected response:
```json
{
  "status": "ok",
  "webhook_url": "https://boomreception-api-504457548316.europe-west1.run.app",
  "webhook_ok": true,
  "db_dir": "/data/whatsapp",
  "session_id": "smba"
}
```

If `webhook_ok` is `false`, the `WEBHOOK_URL` env var is not set correctly on the Cloud Run service.
