# QR Code Generation Debug Guide

## Overview
Comprehensive debugging has been added to the QR code generation pipeline to help track failures in production. All debug logs are prefixed with `[QR_*]` tags for easy filtering.

## Debug Log Categories

### 1. **Success Logs** - `[QR_SUCCESS]`
- Indicates successful QR operations
- Example: `[QR_SUCCESS] QR connection fully established`
- `[QR_SUCCESS] GetQRPayload returning QR payload`

### 2. **Error Logs** - `[QR_ERROR]`
- Critical failures that need immediate attention
- Includes error type and detailed context
- Example: `[QR_ERROR] Failed to open QR channel`
- `[QR_ERROR] Client.Connect() failed`
- `[QR_ERROR] WaitForConnection timed out after login event`

### 3. **Debug Logs** - `[QR_DEBUG]`
- Detailed flow tracking for debugging
- Shows state transitions and channel status
- Helps track goroutine initialization and synchronization
- Examples:
  - `[QR_DEBUG] Starting QR connection flow`
  - `[QR_DEBUG] New QR code generated`
  - `[QR_DEBUG] First QR code ready, unblocking waiters`
  - `[QR_DEBUG] Waiting for QR payload to become available`

### 4. **Timeout Logs** - `[QR_TIMEOUT]`
- Specific timeout events that need attention
- `[QR_TIMEOUT] QR code expired without being scanned`
- `[QR_TIMEOUT] Timeout waiting for QR code to become available`

### 5. **Handler Logs** - `[QR_HANDLER_*]`, `[QR_CURRENT_*]`
- HTTP request/response level tracking
- `[QR_HANDLER] Session already paired, returning 409 Conflict`
- `[QR_HANDLER_SUCCESS] Returning QR payload, 200 OK`
- `[QR_CURRENT_DEBUG] Polling for current QR payload`

## Key Debug Points in Code

### In `connectWithQR()` - `client/whatsapp.go:657`
1. **QR Flow Initialization** (lines 657-658)
   - Logs when QR connection starts
   - Useful to see if QR goroutine is being spawned

2. **QR Channel Opening** (lines 676)
   - Logs when attempting to open WhatsApp QR channel
   - If this fails, you'll see the exact error

3. **Client Connection** (lines 687-688)
   - Logs when calling `client.Connect()`
   - Shows if connection fails with error type

4. **QR Code Generation** (lines 710-720)
   - Logs each QR code generated with:
     - Code length
     - Code prefix (first 10 chars for identification)
   - Will log if empty QR code is received

5. **Login Event** (lines 723-739)
   - Logs login event reception
   - Tracks WaitForConnection 30-second timeout
   - Critical for identifying authentication failures

6. **QR Timeout Events** (lines 740-747)
   - Logs when QR expires without scan
   - Clears stale payloads immediately

### In `GetQRPayload()` - `client/whatsapp.go:771-865`
1. **Entry Point** (line 779)
   - Logs function call with requested timeout

2. **Session Pairing Check** (lines 791-797)
   - Logs if session already paired
   - Includes phone number and status

3. **Channel Ready Waiting** (lines 804-822)
   - Logs polling attempts to get readyCh
   - Shows number of attempts before success
   - Indicates if manual goroutine launch was needed

4. **QR Payload Waiting** (lines 829-851)
   - Logs when blocking for first QR payload
   - Detailed logging for all three outcomes:
     - Success: payload returned with prefix
     - Context cancellation: logs error
     - Timeout: logs timeout with duration

### In `EnsureSession()` - `client/whatsapp.go:254-305`
1. **Session Existence Check**
   - Logs if session already exists with current status
2. **New Session Creation**
   - Logs session creation start
   - Logs store container creation errors
   - Logs device retrieval errors
3. **Initial Connection Decision**
   - Logs whether starting QR flow (unpaired) or reconnect flow (paired)
   - Includes phone number for paired sessions

### In HTTP Handlers - `handlers/qr.go`
1. **POST /api/qr-payload**
   - Request received with session ID and timeout
   - Success: payload length and prefix
   - Error: error type, timeout value, and error message
   - Pairing conflict: phone number and status

2. **GET /api/qr-current/:session_id**
   - Debug: polling requests with session ID
   - Debug: when no active payload exists
   - Success: payload length returned

## How to Filter Production Logs

### Using grep (command line)
```bash
# See all QR debug logs
grep '\[QR_' application.log

# See only errors
grep '\[QR_ERROR\]' application.log

# See only successes
grep '\[QR_SUCCESS\]' application.log

# See specific session
grep 'session.*mySessionId' application.log | grep '\[QR_'

# See timeout issues
grep '\[QR_TIMEOUT\]' application.log
```

### Using Log Aggregation (CloudRun, ECS, etc.)
```
resource.type="cloud_run_revision"
severity >= ERROR
message=~"\[QR_"
```

## Common Issues and What to Look For

### Issue 1: "QR code generation failed" / GetQRPayload times out
**Look for these logs:**
1. `[QR_DEBUG] Starting QR connection flow` - Should see this
2. `[QR_ERROR] Failed to open QR channel` - Check for this error
3. `[QR_TIMEOUT] Timeout waiting for QR code to become available` - Indicates QR never generated
4. `[QR_DEBUG] New QR code generated` - Missing means WhatsApp didn't send code event

**Investigation steps:**
1. Check WhatsApp API connectivity
2. Check if WebSocket connection is failing
3. Verify no permission/authentication issues with WhatsApp client

### Issue 2: Session already paired error
**Look for these logs:**
1. `[QR_HANDLER] Session already paired` - Expected, should reconnect instead
2. Check the phone number in logs to confirm it's the right device

### Issue 3: WaitForConnection timeout
**Look for these logs:**
1. `[QR_DEBUG] Login event received from WhatsApp` - Seen
2. `[QR_ERROR] WaitForConnection timed out after login event` - Then fails
3. Issue is in the authentication handshake post-login

### Issue 4: QR code expired
**Look for these logs:**
1. `[QR_TIMEOUT] QR code expired without being scanned`
2. This is expected behavior, QR should auto-restart after 2 seconds

## Log Fields Reference

Every log entry includes:
- **session**: Session ID for the connection
- **error**: Error message if applicable
- **error_type**: Go type of error (e.g., `*os.PathError`)
- **timeout**: Duration being waited for
- **payload_len**: Length of QR payload string
- **payload_prefix**: First 10-20 characters of QR payload for identification
- **phone**: WhatsApp phone number (when session is paired)
- **status**: Current session status (connected/disconnected/connecting/needs_pairing)
- **code_prefix**: First 10 characters of QR code
- **requested_timeout**: HTTP request timeout value

## Performance Notes

The added logging:
- Uses structured logging (slog) for efficient parsing
- Debug logs use low-level log.Debug which can be disabled in production
- Info, Warn, and Error logs are always captured
- No allocation of large strings (payloads are only partially logged)
- Minimal performance impact in production

## Environment Configuration

To adjust logging levels for QR debugging:

### For development/debugging:
```env
LOG_LEVEL=debug
```
This will show all `[QR_DEBUG]` logs for detailed tracing.

### For production:
```env
LOG_LEVEL=info
```
This will show errors, warnings, successes, and handler logs, but skip debug details.

