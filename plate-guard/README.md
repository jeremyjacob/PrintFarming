# Bambuddy Plate Guard

`bambuddy-plate-guard` is a local Go service that checks first-layer quality, pauses confirmed failures, restores AMS Filament Backup after a reviewed first layer, runs post-success cooling fans, keeps Bambuddy's queue gated after any terminal queued print, and releases the next print only when fresh camera images show that the build plate is confidently empty.

The queue-release path fails closed:

- It verifies Bambuddy's global `require_plate_clear` setting before accepting webhooks and again before release.
- It authenticates each webhook and binds it to the path printer, printer name, normalized print name, and latest terminal queue-job ID, status, and timestamp.
- It analyzes two newly captured camera snapshots. The finish photo embedded in the webhook is not used as release evidence.
- Both OpenAI assessments must find the normal fixed-camera view usable and show no retained part or obstruction. A full view of every plate edge is not required.
- Camera, OpenAI, Bambuddy, timeout, stale-event, and uncertain-result failures do not send a clear request.
- It maps completed, failed, and stopped webhooks to their exact terminal queue statuses, then rechecks the same queue job and printer gate immediately before calling Bambuddy's `clear-plate` endpoint.
- After a validated successful completion, it runs each supported auxiliary/chamber fan at full speed for five minutes. Startup, acknowledgement, and the timer run outside the worker pool. Every fan command is cancellation-aware and preceded by gate/job ownership checks.
- Active fan ownership is persisted before fan startup. On restart, Plate Guard stops and verifies orphaned fans only while the original terminal gate remains safe to control; changed or active-print ownership is relinquished without fan commands.

The first-layer path is deliberately conservative against false pauses:

- It accepts Bambuddy's `first_layer_complete` event only when the printer is connected and the named print is still `RUNNING` at layer 2 or later.
- It binds the event to the exact active Bambuddy queue-item ID and start time. Non-queue/manual prints are not automatically paused.
- It uses a separate high-precision failure-detection prompt on two fresh, byte-distinct snapshots.
- Both assessments must classify a visible, major physical defect before Plate Guard can pause the print.
- Unclear images, API failures, low confidence, and disagreement let the print continue.
- It rechecks the connection, queue-item ID/start time, print identity, and non-reset layer counter immediately before requesting a pause.
- After a completed review does not confirm a failure, it repeats the same active-print checks and enables AMS Filament Backup through Bambuddy's printer API. It does not enable backup for a confirmed failure, an expired event, or a print that changed during review.
- It never stops, resumes, or starts a printer directly.

Bambuddy 1.2.5 does not provide atomic "control this exact print/gate generation" operations. Plate Guard cancels in-flight fan requests when a newer accepted webhook arrives and rechecks ownership before each command, but a manual action can still change printer state in the narrow interval between a final check and delivery. Do not manually clear a gate or replace/resume a print while Plate Guard is processing the corresponding webhook.

## Requirements

- Bambuddy 1.2.5 or newer; P2S/X2D accessory-fan detection and optional left-aux (`aux2`) control require 1.2.5.2 or newer
- A usable Bambuddy camera snapshot endpoint
- An OpenAI API key with Responses API access to `gpt-5.6-sol`
- Go 1.22 or newer when building from source

## Build And Test

```bash
cd plate-guard
make test
make vet
make build
./bin/bambuddy-plate-guard -version
```

`make build` disables cgo and creates a self-contained binary. The service's only runtime dependencies are network access to Bambuddy and OpenAI.

## Configuration

Configuration is read from environment variables.

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `BAMBUDDY_URL` | Yes | - | Bambuddy base URL, such as `http://100.109.149.34:8000` |
| `BAMBUDDY_API_KEY` | With Bambuddy auth | Empty | Restricted Bambuddy key sent as `X-API-Key` |
| `OPENAI_API_KEY` | Yes | - | OpenAI project API key |
| `OPENAI_MODEL` | No | `gpt-5.6-sol` | Vision model used for plate and first-layer assessments |
| `OPENAI_BASE_URL` | No | `https://api.openai.com/v1` | OpenAI Responses API base URL |
| `OPENAI_IMAGE_DETAIL` | No | `high` | `low`, `high`, or `auto` |
| `WEBHOOK_SECRET` | Yes | - | Shared bearer token for incoming webhooks |
| `LISTEN_ADDR` | No | `127.0.0.1:8787` | HTTP listen address |
| `BAMBUDDY_TIMEZONE` | No | `UTC` | IANA timezone for Bambuddy's offset-free webhook timestamps |
| `EVENT_MAX_AGE` | No | `5m` | Maximum webhook age when external work is accepted; validated fan continuations are exempt |
| `SNAPSHOT_DELAY` | No | `5s` | Delay before the first fresh snapshot |
| `WORKER_COUNT` | No | `4` | Concurrent workers; jobs for one printer remain serialized |
| `AUTO_ENABLE_PLATE_CLEAR` | No | `true` | Attempt to enable `require_plate_clear` during startup |
| `ENABLE_AMS_BACKUP_AFTER_FIRST_LAYER` | No | `true` | Enable AMS Filament Backup after a completed first-layer review does not confirm a failure |
| `POST_PRINT_FAN_DURATION` | No | `5m` | Successful-completion fan-cycle duration from `0s` (disabled) through `1h` |
| `POST_PRINT_FAN_SPEED` | No | `100` | Auxiliary, chamber, and optional left-aux fan speed from 4 to 100 percent |
| `POST_PRINT_FAN_STATE_FILE` | No | `/var/lib/bambuddy-plate-guard/fan-cycles.json` | Durable active-cycle state used for restart cleanup |
| `DRY_RUN` | No | `false` | Analyze and revalidate without sending fan, pause, AMS-backup, or `clear-plate` requests |
| `BAMBUDDY_TIMEOUT` | No | `15s` | Timeout for each Bambuddy request |
| `OPENAI_TIMEOUT` | No | `60s` | Timeout for each OpenAI request |
| `SHUTDOWN_TIMEOUT` | No | `5m` | Maximum drain time; values above `5m` are rejected to match the unit |

Generate a dedicated webhook secret:

```bash
openssl rand -hex 32
```

For local development, keep secrets in the ignored `plate-guard/.env.local` file and restrict it to the current user:

```bash
cp .env.example .env.local
chmod 600 .env.local
set -a
. ./.env.local
set +a
go run .
```

For a non-root local run, set `POST_PRINT_FAN_STATE_FILE` to a writable absolute path or set `POST_PRINT_FAN_DURATION=0s`. The systemd unit creates and protects the default `/var/lib/bambuddy-plate-guard` directory automatically.

Set `BAMBUDDY_TIMEZONE` to the Bambuddy process or container's timezone when it is not UTC, for example `America/Los_Angeles`. This may differ from the Docker host's timezone. Bambuddy 1.2.5 emits offset-free webhook timestamps but stores queue completion timestamps in UTC; the correct setting is required to bind them safely.

Each completion candidate makes up to two OpenAI vision calls. Each first-layer event makes one call when the first assessment is safe or uncertain and two calls when a possible failure needs confirmation. Account for these calls in API budgets and rate limits.

OpenAI still returns a numeric confidence value for diagnostics and journaling, but Plate Guard deliberately ignores it for release and pause decisions. Only the structured visibility, empty, and defective booleans affect control actions.

## Bambuddy Setup

### 1. Enable The Queue Gate

Enable **Require plate clear** in Bambuddy's queue/workflow settings.

At startup, the service reads `GET /api/v1/settings`. If the gate is disabled and `AUTO_ENABLE_PLATE_CLEAR=true`, it attempts:

```http
PATCH /api/v1/settings
Content-Type: application/json

{"require_plate_clear":true}
```

This automatic update works when Bambuddy authentication is disabled. Bambuddy 1.2.5 intentionally denies `settings:update` to operational API keys, so authenticated installations must enable it once in the UI. The service refuses to start if it cannot verify the gate.

### 2. Create A Restricted API Key

If Bambuddy authentication is enabled, create a dedicated key with:

- Read status: enabled
- Control printer: enabled
- Queue, library, inventory, maintenance, archives, projects, and cloud management: disabled

Read status permits the status, queue-read, settings-read, camera-view, and temporary camera-token calls used by the service. Control printer is required for `printers:clear_plate`, fan control, print pause, and the AMS Filament Backup update. The service requests a fresh 60-minute camera token automatically; no camera token belongs in the environment file.

After a completed first-layer review that does not confirm a failure, Plate Guard calls:

```http
POST /api/v1/printers/{printer_id}/ams-backup?enabled=true
```

Set `ENABLE_AMS_BACKUP_AFTER_FIRST_LAYER=false` to disable this behavior. This is a per-printer control request; Plate Guard revalidates the active printer and queue job immediately before sending it.

For a matching `print_complete` event, Plate Guard keeps the queue gated while it calls `POST /api/v1/printers/{printer_id}/fan-speed` for the fans Bambuddy reports through `big_fan1_speed` (`aux`), `big_fan2_speed` (`chamber`), and `left_aux_fan_speed` (`aux2`). P2S/X2D chamber control additionally requires `exhaust_fan_present=true`. It persists ownership, sets each fan to `POST_PRINT_FAN_SPEED`, accepts Bambuddy's quantized speed acknowledgement, and only then starts the asynchronous `POST_PRINT_FAN_DURATION` timer. Fan setup and the wait do not consume worker slots.

At expiry, Plate Guard rechecks ownership before every fan-off request, stops and verifies the fans, then queues empty-plate assessment. A newer accepted lifecycle event cancels the old timer and any in-flight fan request. `PREPARE`, `SLICING`, `RUNNING`, and `PAUSE` all identify an active replacement; Plate Guard relinquishes ownership without issuing further fan commands. Failed/stopped prints never start a cycle and cannot clear their gate until older fan cleanup finishes. Cleanup retries while the service is running, and a shutdown cleanup failure leaves the durable record intact and exits unsuccessfully for recovery on restart.

`DRY_RUN=true` waits for the configured duration and exercises revalidation and assessment timing without sending fan commands. If live fan ownership was persisted by an earlier run, dry-run startup refuses to proceed rather than silently issuing cleanup commands; restart once with `DRY_RUN=false` or stop the recorded fans manually.

Bambuddy 1.2.5 does not consistently enforce an API key's printer allowlist on these routes. Treat the API key as able to read and clear all printers until that is fixed upstream, keep the webhook secret private, and firewall the listener to Bambuddy's network.

### 3. Add The Webhook Provider

Create one notification provider per printer:

| Bambuddy field | Value |
| --- | --- |
| Type | Webhook |
| URL | `http://GUARD_HOST:8787/webhooks/bambuddy/PRINTER_ID` |
| Authorization | The exact value of `WEBHOOK_SECRET` |
| Payload format | Generic |
| Printer | The matching printer |
| Print complete | Enabled |
| Print failed | Enabled |
| Print stopped | Enabled |
| First layer complete | Enabled |
| Other events | Disabled unless used elsewhere |
| Quiet hours | Disabled |
| Daily digest | Disabled |

Bambuddy converts a plain Authorization value to `Bearer VALUE`; that is the format Plate Guard requires. A provider test event is accepted and ignored because only `event=print_complete`, `event=print_failed`, `event=print_stopped`, and `event=first_layer_complete` start assessments.

Finish-photo capture is not required. Bambuddy may include a base64 finish image, but Plate Guard intentionally ignores it and captures fresh snapshots after the configured delay.

Plate Guard does not call Bambuddy's built-in plate detector and does not require its calibration references. Bambuddy plate detection can be disabled when Plate Guard is intended to replace it.

The webhook URL is resolved from inside Bambuddy's process or container. If Bambuddy runs in Docker and Plate Guard runs on the host, use the host's LAN address, Docker bridge gateway, or a configured `host.docker.internal` mapping. Set `LISTEN_ADDR=0.0.0.0:8787` and firewall the port to the Bambuddy host or container network.

## Systemd Installation

Build on the target Linux machine:

```bash
cd plate-guard
make test build
sudo install -o root -g root -m 0755 bin/bambuddy-plate-guard /usr/local/bin/bambuddy-plate-guard
sudo install -d -o root -g root -m 0750 /etc/bambuddy-plate-guard
sudo install -o root -g root -m 0600 .env.example /etc/bambuddy-plate-guard/bambuddy-plate-guard.env
sudo install -o root -g root -m 0644 deploy/systemd/bambuddy-plate-guard.service /etc/systemd/system/bambuddy-plate-guard.service
```

Alternatively, download the matching `linux_amd64` or `linux_arm64` archive from GitHub Releases and verify it against `checksums.txt`. Each archive contains the binary, `.env.example`, this README, and the systemd unit, so the same installation commands can be run from the extracted directory.

To cross-compile manually from macOS or another non-Linux host, replace `ARCH` with `amd64` or `arm64`:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=ARCH go build -trimpath -o bin/bambuddy-plate-guard .
```

Edit the root-owned environment file:

```bash
sudoedit /etc/bambuddy-plate-guard/bambuddy-plate-guard.env
```

At minimum, set `BAMBUDDY_URL`, `OPENAI_API_KEY`, `WEBHOOK_SECRET`, and `BAMBUDDY_API_KEY` when authentication is enabled. Remove or replace every example secret.

Start and verify the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now bambuddy-plate-guard
sudo systemctl status bambuddy-plate-guard
sudo journalctl -u bambuddy-plate-guard -f
curl http://127.0.0.1:8787/healthz
curl http://127.0.0.1:8787/readyz
```

The unit uses a dynamic unprivileged user, a read-only filesystem, no Linux capabilities, and systemd hardening. systemd provides the private writable state directory used only for active fan-cycle recovery; startup verifies that this directory is writable before enabling live fan control. Operational output goes to the journal.

## Safe Commissioning

1. Set `DRY_RUN=true`.
2. Start the service and send Bambuddy's webhook provider test.
3. If the gate was enabled after a print had already finished, clear that pre-existing gate manually; it has no webhook for the daemon to process.
4. Run a first-layer test and inspect the specialized classification and final active-print revalidation in the journal. Confirm that `DRY_RUN=true` neither pauses it nor changes AMS Filament Backup.
5. Complete an ejection test print and confirm dry-run waits five minutes without changing fans, then inspect both empty-plate classifications and the final gate/job revalidation.
6. Confirm the plate-clear gate remains active in Bambuddy, then clear it manually.
7. Set `DRY_RUN=false` and restart the service.
8. Test a healthy first layer and confirm AMS Filament Backup becomes enabled, then test a clearly failed first layer, the live fan cycle, successful ejection, an occupied plate, an obscured or dark camera, and a deliberately failed OpenAI request before loading a production queue.

```bash
sudo systemctl restart bambuddy-plate-guard
```

If an assessment holds a plate, remove the obstruction and use Bambuddy's normal **Clear plate** action. Plate Guard does not retry held gates. Completed, failed, and stopped queue jobs are eligible for automatic release only when the webhook type matches the terminal queue status and every image and revalidation check passes.

If Plate Guard pauses a first-layer failure, inspect the print and use Bambuddy's normal stop or resume control. Plate Guard never resumes automatically.

If a fan, pause, AMS-backup, or `clear-plate` request times out after delivery, the result is inherently unknown: Bambuddy may have processed it before the response was lost. Plate Guard logs this case explicitly and retries safe fan cleanup while it still owns the terminal gate. It does not issue further fan commands after ownership changes to a replacement print.

## HTTP Endpoints

- `GET /healthz`: worker lifecycle/liveness status
- `GET /readyz`: accepting-work, queue-capacity, fan-recovery ownership, Bambuddy-connectivity, and gate-setting check
- `POST /webhooks/bambuddy/{printer_id}`: authenticated Bambuddy notification receiver

The webhook handler acknowledges quickly and performs analysis in a 256-entry bounded background queue. Each printer has an ordered serial queue, while `WORKER_COUNT` limits concurrent active processing across different printers. Fan startup, timers, cleanup, and cleanup-dependent lifecycle waits run outside the worker pool, so several cooling or reconciling printers cannot starve unrelated first-layer or terminal events. During shutdown, new work is rejected, fan timers are cancelled and reconciled immediately, and accepted work drains for up to `SHUTDOWN_TIMEOUT`.

## Releases

Pushing a tag matching `v*`, such as `v0.1.1`, runs `.github/workflows/release.yml`. The workflow tests and vets the module, then publishes executable archives for:

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64

Every archive includes the binary, environment template, systemd unit, and Plate Guard README. Every release includes `checksums.txt` with SHA-256 hashes.
