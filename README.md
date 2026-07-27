# agentd

A local-first Go control plane for durable access to autonomous agent sessions. [OMP](https://github.com/can1357/oh-my-pi) is the first registered harness; [Herdr](https://herdr.dev) is the first registered operating surface. [Hermes](https://github.com/NousResearch/hermes-agent) can use the control plane through MCP.

> Experimental and under active development. Expect breaking changes, incomplete documentation, and rough edges.

## Boundary

```text
You → Hermes personal gateway → agentd controller → local harness adapter → agent runtime
                                         └───────→ agentd-node → harness adapter → agent runtime
                                         ↘ surface adapter → workspace UI
```

Hermes remains the preferred personal interface for conversation, memory, voice, notifications, and delegation. It reaches `agentd` through the local Streamable HTTP MCP endpoint at `/mcp`. `agentd` owns authentication, persistence, notifications, normalized events, and the mobile control plane. Harness adapters own agent process protocols; surface adapters own workspace discovery, output, focus, input, and lifecycle actions.

The initial composition registers the OMP harness and Herdr surface. `agentd` discovers allowlisted Herdr workspaces through `session.snapshot`; each mobile- or Hermes-started OMP session gets an unfocused `agentd · omp` Herdr tab whose status follows the normalized session state. The OMP process remains supervised by `agentd`, so Herdr is an attachment and visibility layer rather than a second process owner.

The authenticated HTTP API is provider-neutral: `surface → workspace → view → target` for operating surfaces and capability-described harness sessions for agents. Herdr socket paths and OMP protocol event names stay inside their adapters.

## Run

```sh
go run ./cmd/agentd -config agentd.example.json
```

Open <http://127.0.0.1:7337> for loopback development. `agentd.example.json` leaves authentication and push disabled so Air can reload without rotating secrets.

`workspaceRoots` is the filesystem security boundary for Herdr discovery. The browser receives opaque workspace IDs and names, never host paths. `dataDir` holds the SQLite WAL database and must not overlap Air's `var` build directory.

### Execution nodes

`agentd-node` binds another machine into the controller without exposing an inbound worker port. It authenticates outbound, reports its OS, architecture, OMP version, load, and configured workspace replicas, then long-polls for session commands. The controller prefers an online worker that has the requested workspace; when none is available, it retains the existing local execution path. Worker disconnects do not cancel owned OMP processes. Commands and events resume when the same worker reconnects, and replayed start commands are idempotent.

The controller and worker configurations use the same workspace shape. Workspace IDs must match across machines; paths remain local to each machine:

```json
{
  "workspaces": [
    {"id": "agentd", "name": "agentd", "path": "/local/path/to/agentd"}
  ]
}
```

Set a bootstrap secret on the controller, then use it only for the worker's first enrollment:

```sh
# Controller
export AGENTD_NODE_ENROLLMENT_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/agentd -config /path/to/controller.json

# Worker machine, using the same bootstrap value for first enrollment
export AGENTD_CONTROLLER_URL="https://agentd.example.ts.net"
export AGENTD_NODE_ENROLLMENT_TOKEN="<controller bootstrap value>"
go run ./cmd/agentd-node -config /path/to/worker.json -name "MacBook"
```

The worker exchanges the bootstrap secret for a random node credential and stores it with mode `0600` under `$XDG_CONFIG_HOME/agentd-node/identity.json` (or `~/Library/Application Support/agentd-node/identity.json` on macOS). Remove the bootstrap variable from the worker after enrollment. Existing workers continue authenticating if the controller restarts without `AGENTD_NODE_ENROLLMENT_TOKEN`; omitting it simply disables new enrollments.

The **Fleet** view shows enrolled nodes, online state, platform, OMP version, active run count, and workspace count. Session history records the selected node and placement reason.

### Secure tailnet and Android PWA

Keep `agentd` on loopback, enable per-device authentication, and let Tailscale terminate HTTPS. First copy `agentd.example.json` to a private deployment config such as `agentd.local.json`; in that copy, change these fields:

```json
{
  "publicUrl": "https://agentd.example.ts.net",
  "auth": {"enabled": true, "cookieSecure": true},
  "push": {"enabled": false, "subject": "mailto:you@example.com"}
}
```

```sh
go build -o agentd ./cmd/agentd
export AGENTD_ENROLLMENT_TOKEN="$(./agentd -generate-enrollment-token)"
tailscale serve --bg http://127.0.0.1:7337
./agentd -config agentd.local.json
```

To pair without typing the token, run `agentd --pair` when the deployed binary is in `PATH`. It discovers `$XDG_CONFIG_HOME/agentd/agentd.json` (or `~/.config/agentd/agentd.json`) and reads `AGENTD_ENROLLMENT_TOKEN` from the adjacent private `agentd.env`; an exported variable takes precedence. For a nonstandard deployment, use `agentd --config /path/to/agentd.json --pair`. Scan the terminal QR code with the Android camera. The credential is carried only in the URL fragment, which is not sent in HTTP requests, and the PWA removes it from the address bar and current history entry synchronously before contacting agentd. Name the device and tap **Connect**. Alternatively, open the Tailscale HTTPS URL in Chrome on Android and paste the setup token. Then use agentd's **Install** action to add the standalone PWA to the launcher. The setup token is consumed transactionally after one device; its hash remains consumed even if that device is revoked. Generate a new token and restart `agentd` before enrolling each additional browser, native client, or Hermes MCP client.

Browser installs receive an HttpOnly, `SameSite=Strict` per-device cookie. Native and MCP enrollment callers receive the same random device credential once in the enrollment response and use it as `Authorization: Bearer <token>`. Only token hashes are stored. Cookie-authenticated mutations also require a same-origin `Origin` header.

To enable encrypted Web Push, generate one stable VAPID key pair:

```sh
./agentd -generate-vapid-keys
export AGENTD_VAPID_PUBLIC_KEY="<publicKey>"
export AGENTD_VAPID_PRIVATE_KEY="<privateKey>"
```

Then set `push.enabled` to `true` in `agentd.local.json` and keep `push.subject` as a `mailto:` or HTTPS URI. Restart with the same VAPID keys, open the installed Android PWA, and tap **Enable**; Chrome requires notification permission to follow a direct user action. `agentd` makes outbound-only, VAPID-authenticated requests to allowlisted Apple, Google, Mozilla, or Windows push services; RFC 8291 encrypts each payload for its device subscription. The gateway remains unreachable from the public internet.

## Hot reload

The repository pins [Air](https://github.com/air-verse/air) as a Go tool dependency. Start the development server with:

```sh
go tool air
```

Air watches Go, HTML, CSS, JavaScript, and JSON files. A change rebuilds `./cmd/agentd`, gracefully interrupts the old process, and launches the new binary with `agentd.example.json`. A failed build leaves the invalid binary stopped until the next successful build.

Production remains an ordinary compiled Go binary:

```sh
go build -o agentd ./cmd/agentd
./agentd -config agentd.example.json
```

### Hermes

For authentication-disabled loopback development, register the service directly:

```yaml
mcp_servers:
  agentd:
    url: http://127.0.0.1:7337/mcp
    enabled: true
    tools:
      allow: all
```

For the secured daemon, give Hermes its own device credential. Rotate `AGENTD_ENROLLMENT_TOKEN`, restart `agentd`, then enroll Hermes:

```sh
curl -fsS http://127.0.0.1:7337/api/v1/devices/enroll \
  -H 'Content-Type: application/json' \
  --data '{"name":"Hermes","token":"<current-one-time-setup-token>"}'
hermes mcp add agentd --url http://127.0.0.1:7337/mcp --auth header
```

Copy the enrollment response's `.token` value—not the setup token—into Hermes' `API key / Bearer token` prompt. Hermes stores it as `MCP_AGENTD_API_KEY` in `~/.hermes/.env` and configures:

```yaml
headers:
  Authorization: Bearer ${MCP_AGENTD_API_KEY}
```


Validate and reload it:

```sh
hermes mcp test agentd
hermes gateway restart
```

The MCP surface provides surface-backed workspace discovery, synchronous OMP delegation, session follow-ups, durable normalized event reads, and abort control. It does not replace Hermes' conversation or memory model.

## Dogfood

The Android PWA installs from Chrome and caches its shell for offline startup. Its default **Work** view flattens live targets by attention priority; **Fleet** retains the complete allowlisted surface → workspace → view → target topology; **Settings** owns installation, device controls, notifications, and Hermes capabilities. Target detail uses a shared server-side output watcher and authenticated EventSource stream, with browser polling only as a reconnect fallback. It deep-links directly to targets and renders only actions advertised by the surface adapter.

Agent launch choices come from `GET /api/v1/capabilities`; the UI sends the selected `harnessId` rather than assuming OMP. Each surface target advertises `presentations` and a `preferredPresentation`, and **Work** opens that preferred mode instead of treating every target as terminal output. Agentd-managed OMP targets prefer `conversation`, which opens the durable native transcript, interactions, composer, and abort control; their explicit `screen` choice retains the Herdr snapshot as a degraded output view rather than claiming exact PTY semantics. Targets without an attached managed session are labeled as screen output and offer **Start conversation** when agent launch is available. An OMP process launched from a Herdr target still runs under `agentd` supervision, and its Herdr tab remains a status attachment. Closing a managed attachment closes the supervised harness session and surface target together.

Sessions, user input intents, interaction responses, and normalized harness events are committed to SQLite before broadcast. Public event kinds include `input_submitted`, `assistant_delta`, `interaction_requested`, `tool_started`, `turn_completed`, and `session_exited`; adapter-only events are wrapped as `adapter_event`. EventSource reconnects honor the greater of the URL cursor and `Last-Event-ID`; a full browser reload reconstructs the transcript exactly once. Push preferences are stored per device, expired browser subscriptions are pruned, and notifications carry opaque surface/target/session IDs rather than terminal titles or output.

Static PWA assets, health, auth status, and enrollment are public. Every surface operation, workspace, session, device-management, and MCP route requires a device credential when authentication is enabled.

HTTP endpoints:

```text
GET    /healthz
GET    /api/v1/auth/status
POST   /api/v1/devices/enroll
GET    /api/v1/devices
DELETE /api/v1/devices/{deviceID}
GET    /api/v1/device
PUT    /api/v1/device/preferences
PUT    /api/v1/device/push
DELETE /api/v1/device/push
GET    /api/v1/capabilities
GET    /api/v1/workspaces
GET    /api/v1/surfaces
GET    /api/v1/surfaces/{surfaceID}/targets/{targetID}/output
GET    /api/v1/surfaces/{surfaceID}/targets/{targetID}/events
POST   /api/v1/surfaces/{surfaceID}/targets/{targetID}/focus
POST   /api/v1/surfaces/{surfaceID}/targets/{targetID}/send
POST   /api/v1/surfaces/{surfaceID}/targets/{targetID}/agent-sessions
DELETE /api/v1/surfaces/{surfaceID}/targets/{targetID}
POST   /api/v1/sessions
GET    /api/v1/sessions
GET    /api/v1/sessions/{id}
POST   /api/v1/sessions/{id}/input
POST   /api/v1/sessions/{id}/interactions/{requestID}/response
POST   /api/v1/sessions/{id}/abort
DELETE /api/v1/sessions/{id}
GET    /api/v1/sessions/{id}/events?after={sequence}
GET    /api/v1/sessions/{id}/stream?after={sequence}
GET    /mcp
POST   /mcp
```

## Test

```sh
go test ./...
```

## Known boundary

Durable transcripts survive `agentd` restarts, but OMP RPC processes started by the old daemon cannot currently be reattached. Those sessions become `interrupted`; the Android PWA keeps their transcript readable and disables input rather than pretending the runtime is live.
