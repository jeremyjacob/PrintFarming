# Bambuddy Plate Guard

`bambuddy-plate-guard` is a local Go service that checks first-layer quality, pauses confirmed failures, keeps Bambuddy's queue gated after a successful queued print, and releases the next print only when fresh camera images show that the build plate is confidently empty.

The queue-release path fails closed:

- It verifies Bambuddy's global `require_plate_clear` setting before accepting webhooks and again before release.
- It authenticates each webhook and binds it to the path printer, printer name, normalized print name, and latest terminal queue-job ID, status, and timestamp.
- It analyzes two newly captured camera snapshots. The finish photo embedded in the webhook is not used as release evidence.
- Both OpenAI assessments must show a visible, empty plate at or above the confidence threshold.
- Camera, OpenAI, Bambuddy, timeout, stale-event, and uncertain-result failures do not send a clear request.
- It queries completed, failed, cancelled, aborted, and skipped queue jobs, then rechecks the same successful terminal job and printer gate immediately before calling Bambuddy's `clear-plate` endpoint.

The first-layer path is deliberately conservative against false pauses:

- It accepts Bambuddy's `first_layer_complete` event only when the printer is connected and the named print is still `RUNNING` at layer 2 or later.
- It binds the event to the exact active Bambuddy queue-item ID and start time. Non-queue/manual prints are not automatically paused.
- It uses a separate high-precision failure-detection prompt on two fresh, byte-distinct snapshots.
- Both assessments must show a visible, major physical defect at or above `FIRST_LAYER_FAILURE_THRESHOLD`.
- Unclear images, API failures, low confidence, and disagreement let the print continue.
- It rechecks the connection, queue-item ID/start time, print identity, and non-reset layer counter immediately before requesting a pause.
- It never stops, resumes, or starts a printer directly.

Bambuddy 1.2.5 does not provide atomic "clear this exact gate generation" or "pause this exact print generation" operations. A manual action can theoretically change printer state between a final status check and its control request. Do not manually clear a gate or replace/resume a print while Plate Guard is processing the corresponding webhook.

## Requirements

- Bambuddy 1.2.5 or newer
- A usable Bambuddy camera snapshot endpoint
- An OpenAI API key with Responses API access to `gpt-5.6-terra`
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
| `OPENAI_MODEL` | No | `gpt-5.6-terra` | Vision model used for plate and first-layer assessments |
| `OPENAI_BASE_URL` | No | `https://api.openai.com/v1` | OpenAI Responses API base URL |
| `OPENAI_IMAGE_DETAIL` | No | `high` | `low`, `high`, or `auto` |
| `WEBHOOK_SECRET` | Yes | - | Shared bearer token for incoming webhooks |
| `LISTEN_ADDR` | No | `127.0.0.1:8787` | HTTP listen address |
| `BAMBUDDY_TIMEZONE` | No | `UTC` | IANA timezone for Bambuddy's offset-free webhook timestamps |
| `EVENT_MAX_AGE` | No | `5m` | Maximum webhook and queued-job age |
| `SNAPSHOT_DELAY` | No | `5s` | Delay before the first fresh snapshot |
| `EMPTY_CONFIDENCE_THRESHOLD` | No | `0.95` | Minimum confidence required from each assessment |
| `FIRST_LAYER_FAILURE_THRESHOLD` | No | `0.99` | Minimum confidence from both defect assessments; values below `0.99` are rejected |
| `WORKER_COUNT` | No | `4` | Concurrent workers; jobs for one printer remain serialized |
| `AUTO_ENABLE_PLATE_CLEAR` | No | `true` | Attempt to enable `require_plate_clear` during startup |
| `DRY_RUN` | No | `false` | Analyze and revalidate without sending pause or `clear-plate` requests |
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

Set `BAMBUDDY_TIMEZONE` to the Bambuddy process or container's timezone when it is not UTC, for example `America/Los_Angeles`. This may differ from the Docker host's timezone. Bambuddy 1.2.5 emits offset-free webhook timestamps but stores queue completion timestamps in UTC; the correct setting is required to bind them safely.

Each completion candidate makes up to two OpenAI vision calls. Each first-layer event makes one call when the first assessment is safe or uncertain and two calls when a possible failure needs confirmation. Account for these calls in API budgets and rate limits.

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

Read status permits the status, queue-read, settings-read, camera-view, and temporary camera-token calls used by the service. Control printer is required for both `printers:clear_plate` and print pause. The service requests a fresh 60-minute camera token automatically; no camera token belongs in the environment file.

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
| First layer complete | Enabled |
| Other events | Disabled unless used elsewhere |
| Quiet hours | Disabled |
| Daily digest | Disabled |

Bambuddy converts a plain Authorization value to `Bearer VALUE`; that is the format Plate Guard requires. A provider test event is accepted and ignored because only `event=print_complete` and `event=first_layer_complete` start assessments.

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

The unit uses a dynamic unprivileged user, a read-only filesystem, no Linux capabilities, and systemd hardening. It writes operational output only to the journal.

## Safe Commissioning

1. Set `DRY_RUN=true`.
2. Start the service and send Bambuddy's webhook provider test.
3. If the gate was enabled after a print had already finished, clear that pre-existing gate manually; it has no webhook for the daemon to process.
4. Run a first-layer test and inspect the specialized classification and final active-print revalidation in the journal. Confirm that `DRY_RUN=true` does not pause it.
5. Complete an ejection test print and inspect both empty-plate classifications and the final gate/job revalidation.
6. Confirm the plate-clear gate remains active in Bambuddy, then clear it manually.
7. Set `DRY_RUN=false` and restart the service.
8. Test a healthy first layer, a clearly failed first layer, successful ejection, an occupied plate, an obscured or dark camera, and a deliberately failed OpenAI request before loading a production queue.

```bash
sudo systemctl restart bambuddy-plate-guard
```

If an assessment holds a plate, remove the obstruction and use Bambuddy's normal **Clear plate** action. Plate Guard does not retry held gates. Failed, stopped, aborted, and cancelled prints are not auto-cleared.

If Plate Guard pauses a first-layer failure, inspect the print and use Bambuddy's normal stop or resume control. Plate Guard never resumes automatically.

If a pause or `clear-plate` request times out after delivery, the result is inherently unknown: Bambuddy may have processed it before the response was lost. Plate Guard logs this case explicitly.

## HTTP Endpoints

- `GET /healthz`: worker lifecycle/liveness status
- `GET /readyz`: accepting-work, queue-capacity, Bambuddy-connectivity, and gate-setting check
- `POST /webhooks/bambuddy/{printer_id}`: authenticated Bambuddy notification receiver

The webhook handler acknowledges quickly and performs analysis in a 256-entry bounded background queue. Each printer has an ordered serial queue, while `WORKER_COUNT` limits concurrent processing across different printers. A backlog for one printer does not occupy the other printer workers. During shutdown, new work is rejected and accepted work drains for up to `SHUTDOWN_TIMEOUT`.

## Releases

Pushing a tag matching `v*`, such as `v0.1.1`, runs `.github/workflows/release.yml`. The workflow tests and vets the module, then publishes executable archives for:

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64

Every archive includes the binary, environment template, systemd unit, and Plate Guard README. Every release includes `checksums.txt` with SHA-256 hashes.
