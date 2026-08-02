# agentd

`agentd` is a small local daemon for scheduled agent jobs. It persists job definitions and run evidence in SQLite, prevents overlap for one job, launches the configured runner, and records process completion.

The current runner executes `omp -p --session-dir <run-directory> -- <invocationRequest>` from the selected workspace. Each run receives an isolated directory under `dataDir/runs/<run-id>`. Standard output and error are drained without blocking and retained up to 1 MiB per stream.

## Configuration

```json
{
  "listen": "127.0.0.1:7337",
  "dataDir": "./data",
  "ompBinary": "omp",
  "ompArgs": ["--append-system-prompt=/absolute/path/to/worker-profile.txt", "--max-time=30m"],
  "ompEnvFiles": {"LINEAR_API_KEY": "/absolute/path/to/linear-api-key"},
  "herdrBinary": "herdr",
  "workspaces": [{"id":"agentd","path":"."}]
}
```

Unknown fields are rejected. There is no authentication, device, push, fleet, node, dashboard, or interactive-session configuration.

`ompArgs` are inserted before agentd's owned `-p --session-dir ... -- <request>` arguments; lifecycle-conflicting arguments are rejected. `ompEnvFiles` loads one value per file into the OMP child environment without storing the value in agentd's configuration or run evidence. Agentd reserves `OMP_SESSION_*`.

Configured workspaces seed the durable registry at daemon startup. Additional existing directories can be registered safely at runtime and survive restarts:

```sh
agentd -config agentd.json workspaces register -id project -name "Project" -path .
agentd -config agentd.json workspaces list
```

Workspace IDs shaped as `herdr:<server>:<workspace>:<mapping>` receive `agentd_status`, `agentd_job`, `agentd_result`, `agentd_job_id`, `agentd_run`, and `agentd_evidence` metadata. Herdr remains visibility-only. To show the concise state in its expanded sidebar:

```toml
[ui.sidebar.spaces]
rows = [["state_icon", "workspace"], ["$agentd_status", "$agentd_job"], ["$agentd_result"]]
```

## Local administration

The CLI is the operator interface and sends commands to the running loopback daemon:

Discover commands and check the running daemon without knowing its HTTP endpoint:

```sh
agentd --help
agentd help jobs
agentd jobs list --help
agentd status
agentd status --json
```

```sh
agentd -config agentd.json jobs create -name nightly -workspace agentd -request "run the nightly task" -cadence 3600
agentd -config agentd.json jobs list
agentd -config agentd.json jobs run <job-id>
agentd -config agentd.json jobs runs <job-id>
```

The loopback HTTP API exposes `GET /health`, `GET/POST /workspaces`, job create/update/list/get, `POST /jobs/{id}/start`, `/pause`, `/run`, `/stop`, and `GET /jobs/{id}/runs`. `/mcp` implements MCP Streamable HTTP JSON-RPC for nine namespaced tools exposing the job operations; browser requests with non-loopback origins are rejected.
