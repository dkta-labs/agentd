export type Candidate = 'fleet' | 'focus' | 'ops';
export type NodeState = 'online' | 'busy' | 'offline';
export type SessionState = 'working' | 'needs-input' | 'idle' | 'done' | 'interrupted';

export type Node = {
  id: string;
  name: string;
  location: string;
  platform: string;
  version: string;
  state: NodeState;
  latency: string;
  activeRuns: number;
  workspaceIds: string[];
  lastSeen: string;
};

export type Workspace = {
  id: string;
  name: string;
  branch: string;
  revision: string;
  dirty: boolean;
  nodeIds: string[];
};

export type SessionSummary = {
  id: string;
  title: string;
  workspaceId: string;
  nodeId: string;
  harness: string;
  state: SessionState;
  updated: string;
  summary: string;
  changedFiles: number;
  additions: number;
  deletions: number;
};

export type TimelineItem =
  | { id: string; kind: 'user'; text: string; at: string }
  | { id: string; kind: 'assistant'; text: string; at: string; streaming?: boolean }
  | {
      id: string;
      kind: 'tool';
      tool: string;
      title: string;
      detail: string;
      status: 'running' | 'complete' | 'error';
      duration?: string;
      output?: string[];
    }
  | {
      id: string;
      kind: 'diff';
      file: string;
      additions: number;
      deletions: number;
      lines: { type: 'context' | 'add' | 'remove'; value: string }[];
    }
  | {
      id: string;
      kind: 'interaction';
      title: string;
      body: string;
      options: { id: string; label: string; detail: string }[];
    }
  | {
      id: string;
      kind: 'subagent';
      name: string;
      task: string;
      status: 'working' | 'done';
      progress: string;
    }
  | { id: string; kind: 'error'; title: string; detail: string };

export const nodes: Node[] = [
  {
    id: 'studio',
    name: 'Studio',
    location: 'Home · Seattle',
    platform: 'macOS · arm64',
    version: 'OMP 17.1.3',
    state: 'busy',
    latency: '24 ms',
    activeRuns: 2,
    workspaceIds: ['agentd', 'grimoire'],
    lastSeen: 'now',
  },
  {
    id: 'edge',
    name: 'Edge VPS',
    location: 'Fly · Chicago',
    platform: 'Linux · arm64',
    version: 'OMP 17.1.3',
    state: 'online',
    latency: '41 ms',
    activeRuns: 0,
    workspaceIds: ['agentd'],
    lastSeen: 'now',
  },
  {
    id: 'laptop',
    name: 'Laptop',
    location: 'Last seen in Portland',
    platform: 'macOS · arm64',
    version: 'OMP 17.0.9',
    state: 'offline',
    latency: '—',
    activeRuns: 0,
    workspaceIds: ['agentd', 'notes'],
    lastSeen: '18m ago',
  },
];

export const workspaces: Workspace[] = [
  {
    id: 'agentd',
    name: 'agentd',
    branch: 'main',
    revision: '1b21416',
    dirty: true,
    nodeIds: ['studio', 'edge', 'laptop'],
  },
  {
    id: 'grimoire',
    name: 'grimoire',
    branch: 'feat/semantic-index',
    revision: '71ce4d2',
    dirty: false,
    nodeIds: ['studio'],
  },
  {
    id: 'notes',
    name: 'personal-notes',
    branch: 'main',
    revision: 'c2a8fb1',
    dirty: false,
    nodeIds: ['laptop'],
  },
];

export const sessions: SessionSummary[] = [
  {
    id: 'restart-recovery',
    title: 'Make controller restart recovery safe',
    workspaceId: 'agentd',
    nodeId: 'studio',
    harness: 'OMP',
    state: 'working',
    updated: 'now',
    summary: 'Reconciliation path is implemented; fleet tests are running.',
    changedFiles: 3,
    additions: 84,
    deletions: 17,
  },
  {
    id: 'index-receipts',
    title: 'Index receipts with source citations',
    workspaceId: 'grimoire',
    nodeId: 'studio',
    harness: 'OMP',
    state: 'needs-input',
    updated: '4m',
    summary: 'Waiting for a decision about OCR confidence thresholds.',
    changedFiles: 5,
    additions: 126,
    deletions: 32,
  },
  {
    id: 'release-audit',
    title: 'Audit the public repository boundary',
    workspaceId: 'agentd',
    nodeId: 'edge',
    harness: 'Hermes',
    state: 'done',
    updated: '22m',
    summary: 'Secret scan, module path, license, and public metadata verified.',
    changedFiles: 4,
    additions: 31,
    deletions: 6,
  },
  {
    id: 'mobile-layout',
    title: 'Compare mobile session layouts',
    workspaceId: 'agentd',
    nodeId: 'laptop',
    harness: 'OMP',
    state: 'interrupted',
    updated: '18m',
    summary: 'Node went offline; the durable transcript remains available.',
    changedFiles: 2,
    additions: 45,
    deletions: 12,
  },
];

export const timeline: TimelineItem[] = [
  {
    id: 'u1',
    kind: 'user',
    at: '10:42',
    text: 'Make controller restarts safe while remote OMP sessions are still running. Preserve transcript order and do not replay acknowledged input.',
  },
  {
    id: 'a1',
    kind: 'assistant',
    at: '10:42',
    text: 'I’ll trace ownership recovery from persisted placement through worker reconciliation, then exercise the restart boundary with a real remote session.',
  },
  {
    id: 't1',
    kind: 'tool',
    tool: 'Read',
    title: 'Inspect session placement recovery',
    detail: 'internal/sessions/manager.go · lines 183–240',
    status: 'complete',
    duration: '86 ms',
    output: [
      'Placement metadata already survives in SQLite.',
      'Runtime ownership is reconstructed only during a fresh start.',
      'Need a worker reconciliation handshake keyed by session ID.',
    ],
  },
  {
    id: 's1',
    kind: 'subagent',
    name: 'RecoveryScout',
    task: 'Trace worker reconnect and command acknowledgment paths',
    status: 'done',
    progress: 'Found the idempotency boundary and one stale ownership edge.',
  },
  {
    id: 't2',
    kind: 'tool',
    tool: 'Test',
    title: 'Run fleet restart coverage',
    detail: 'go test ./internal/fleet -run Restart -count=1',
    status: 'running',
    output: ['controller stopped', 'worker retained session', 're-enrollment accepted'],
  },
  {
    id: 'd1',
    kind: 'diff',
    file: 'internal/fleet/service.go',
    additions: 14,
    deletions: 3,
    lines: [
      { type: 'context', value: 'func (s *Service) reconcileSession(record SessionRecord) {' },
      { type: 'remove', value: '    s.owners[record.ID] = record.NodeID' },
      { type: 'add', value: '    owner, online := s.nodes[record.NodeID]' },
      { type: 'add', value: '    if !online { return }' },
      { type: 'add', value: '    s.owners[record.ID] = owner.ID' },
      { type: 'add', value: '    s.commands.AcknowledgeThrough(record.LastCommand)' },
      { type: 'context', value: '}' },
    ],
  },
  {
    id: 'i1',
    kind: 'interaction',
    title: 'Recovery policy',
    body: 'A worker reports a live runtime that the restarted controller has marked interrupted. Which source should win?',
    options: [
      { id: 'worker', label: 'Trust the worker', detail: 'Reattach when the session ID and node credential both match.' },
      { id: 'controller', label: 'Trust the controller', detail: 'Leave interrupted and require an explicit restart.' },
      { id: 'inspect', label: 'Require inspection', detail: 'Show the mismatch and wait for a human decision.' },
    ],
  },
  {
    id: 'a2',
    kind: 'assistant',
    at: '10:45',
    streaming: true,
    text: 'The safe default is worker-authoritative only after mutual identity and monotonic command-sequence checks pass. I’m wiring that invariant into the reconciliation path now…',
  },
];

export const terminalLines = [
  '$ agentd-node -config ~/.config/agentd-node/config.json',
  '10:42:03  node connected     controller=edge.dkta.dev',
  '10:42:03  heartbeat accepted  workspaces=2 active=1',
  '10:42:14  session retained    id=restart-recovery',
  '10:42:17  controller lost     retry=1s',
  '10:42:19  controller restored identity=verified',
  '10:42:19  session reconciled  sequence=184',
  '10:42:20  command stream live pending=0',
];

export function nodeFor(id: string): Node {
  return nodes.find((node) => node.id === id) ?? nodes[0]!;
}

export function workspaceFor(id: string): Workspace {
  return workspaces.find((workspace) => workspace.id === id) ?? workspaces[0]!;
}
