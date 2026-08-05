# Agentd boundary

Agentd schedules and supervises configured agent-runner invocations.

Core owns:

- job definitions and schedules;
- at-most-one active invocation per job;
- runner launch, wait, and explicit stop;
- durable run identity and lifecycle state;
- restart interruption recovery.

Core does not own:

- agent planning, goals, delegation, evaluation, or quality policy;
- provider-specific conversation, interaction, tool, or event semantics;
- user interfaces, dashboards, devices, notifications, or fleet management;
- work-tracker, knowledge-management, deployment, or publishing policy.

Runner integrations must remain replaceable. Keep the core seam limited to lifecycle behavior required by a concrete integration; do not add speculative remote, observer, capability, or orchestration frameworks.

Canonical rationale: `~/grimoire/decisions/2026-07-30-agentd-scheduled-agent-supervisor.html`.

Implementation authority: [DKT-139](https://linear.app/dkta-labs/issue/DKT-139/dispatch-agentd-work-as-detached-interactive-herdr-agents). Historical core-cutover evidence: [DKT-67](https://linear.app/dkta-labs/issue/DKT-67/cut-agentd-over-to-the-minimal-scheduled-agent-supervisor).
