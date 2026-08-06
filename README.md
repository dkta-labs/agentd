# agentd

`agentd` is a small local daemon for scheduled agent jobs. It persists job definitions and run evidence in SQLite, prevents overlap for one job, dispatches each run to a normal interactive OMP agent owned by Herdr, and observes that owner in the background.

Agentd does not launch OMP itself. For a configured Herdr workspace mapping (`herdr:<server>:<workspace>:<mapping>`), it creates or reuses a real Herdr tab, starts OMP through `herdr agent start`, submits the bounded request, records the owner target, then waits for the interactive agent to settle. The coordinator is free immediately after dispatch. Agentd restart detaches and reattaches its watcher without stopping a healthy worker.

## Configuration

```json
{
  "listen": "127.0.0.1:7337",
  "dataDir": "./data",
  "herdrBinary": "herdr",
  "agentArgs": [
    "--config=/absolute/path/to/worker-config.yml",
    "--append-system-prompt=/absolute/path/to/worker-profile.txt",
    "--max-time=12h"
  ],
  "agentEnv": {
    "LINEAR_API_KEY_FILE": "/absolute/path/to/linear-api-key"
  },
  "coordinatorTarget": "coordinator",
  "workspaces": [
    {
      "id": "herdr:default:w7:mapping",
      "name": "Project",
      "path": "/absolute/path/to/project"
    }
  ]
}
```

Unknown fields are rejected. `agentArgs` are passed to `herdr agent start ... --kind omp --`; print mode, non-interactive mode, and session ownership flags are rejected. `agentEnv` is applied by Herdr when it creates the worker tab. Prefer file-path environment variables such as `LINEAR_API_KEY_FILE`; do not place secret values in this file. `coordinatorTarget` is optional and lets a worker wake a specific Herdr agent exactly once for irreducible ambiguity or a genuine external blocker.

Configured workspaces seed the durable registry at daemon startup. Additional existing directories can be registered safely at runtime and survive restarts:

```sh
agentd -config agentd.json workspaces register -id project -name "Project" -path .
agentd -config agentd.json workspaces list
```

Runnable OMP jobs require a Herdr-mapped workspace ID. Each job has a stable owner target named `agentd-<job-hash>`. Recurring runs reuse the same idle OMP session, retaining its context and tools. `jobs get` and `jobs list` expose that target as `ownerTarget` while a run is active. The same owner is visible and interactive through `herdr agent list|get|read|focus|attach|prompt`.

When creating a job, `-goal <goal-key>` is optional collision/visibility metadata. A goal key identifies the work being dispatched; it is not an approval gate. Concurrent goals must use distinct worktrees and workspace paths so their work cannot collide. A worker stays within its assigned goal's worktree, branch, issue, and PR.

A worker proceeds autonomously after dispatch, including assigned-scope investigation, edits, tests, commit, push, PR, independent review, merge after required CI, existing deployment, production verification, and fix-forward. It does not stop for approval. If a coordinator target is configured, the worker wakes it exactly once only for irreducible ambiguity or a genuine external blocker; it does not wake the coordinator for approval, progress updates, or routine choices. Agentd continues observing in the background and does not wake or poll the coordinator itself. A normal completion leaves the reusable OMP session idle. `jobs stop` explicitly closes the owner's Herdr tab. If Agentd restarts, it reattaches to the persisted `ownerTarget`; it marks the run interrupted only when that owner no longer exists.

## Local administration

The CLI is the operator interface and sends commands to the running loopback daemon:

Discover commands and check the running daemon without knowing its HTTP endpoint:

```sh
agentd --help
agentd help jobs
agentd jobs list --help
agentd status
agentd status --json
agentd top
agentd top --once
agentd top --interval 5s
```

```sh
agentd -config agentd.json jobs create -name nightly -workspace agentd -request "run the nightly task" -cadence 3600
agentd -config agentd.json jobs list
agentd -config agentd.json jobs run <job-id>
agentd -config agentd.json jobs runs <job-id>
agentd -config agentd.json jobs wait <job-id> --timeout 30m
```

`jobs wait` polls the loopback job and run APIs until requested or active work settles, then emits one JSON object containing the final `job` and `latestRun` evidence. `--timeout` leaves the managed run untouched and exits with status 124; Ctrl-C cancels only the waiting CLI process.

`agentd top` is the read-only terminal visualization for the configured daemon. It refreshes a lifecycle-sorted jobs table with active run, Herdr owner, elapsed time, last/next run, and bounded failure detail. It performs only `GET` requests, fetches run details only for active jobs, and does not inspect transcripts or guess semantic percent complete. Piped output renders one snapshot automatically.

The loopback HTTP API exposes `GET /health`, `GET/POST /workspaces`, job create/update/list/get, `POST /jobs/{id}/start`, `/pause`, `/run`, `/stop`, and `GET /jobs/{id}/runs`. `/mcp` implements MCP Streamable HTTP JSON-RPC for nine namespaced tools exposing the job operations; browser requests with non-loopback origins are rejected.

## GitHub webhook dispatch

`agentd-github-hook` is a separate provider adapter; Agentd core remains a
provider-neutral loopback scheduler. The adapter verifies signed GitHub
deliveries, matches them to existing job IDs, and calls the existing
`POST /jobs/{id}/run` endpoint.

```json
{
  "listen": "127.0.0.1:7338",
  "agentdUrl": "http://127.0.0.1:7337",
  "secretFile": "./github-hook.secret",
  "dataDir": "./github-hook-data",
  "rules": [
    {
      "id": "agentd-pr-merged",
      "event": "pull_request",
      "action": "closed",
      "repository": "dkta-labs/agentd",
      "merged": true,
      "jobId": "job_existing"
    }
  ]
}
```

```sh
chmod 600 github-hook.secret
agentd-github-hook -config github-hook.json
```

GitHub sends events to `POST /github` with `X-GitHub-Event`,
`X-GitHub-Delivery`, and `X-Hub-Signature-256`. The adapter binds only to
loopback, accepts at most one MiB, stores no payloads, and durably deduplicates
successful delivery/rule pairs in its own SQLite file. An invalid signature is
rejected. Unmatched events are acknowledged without dispatch. If Agentd cannot
accept a run, the adapter releases the receipt and returns `503` with
`Retry-After` so the sender can retry. Public ingress, TLS, and tunnel
configuration remain the responsibility of the existing host edge.
