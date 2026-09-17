/**
 * Wire types shared with the Go daemon (daemon/internal/model). Field names must match exactly.
 */

export type AgentState =
  | 'question'
  | 'permission'
  | 'working'
  | 'thinking'
  | 'tool'
  | 'shell'
  | 'idle'
  | 'exited'
  | 'unknown';

/** Lower number = needs attention sooner. */
export const STATE_PRIORITY: Record<AgentState, number> = {
  question: 0,
  permission: 1,
  working: 2,
  thinking: 3,
  tool: 4,
  shell: 5,
  idle: 6,
  unknown: 7,
  exited: 8,
};

export function needsAttention(state: AgentState): boolean {
  return state === 'question' || state === 'permission';
}

export function isBusy(state: AgentState): boolean {
  return state === 'working' || state === 'thinking' || state === 'tool';
}

export interface PendingTool {
  id: string;
  name: string;
  summary?: string;
}

export interface Agent {
  id: string;
  provider: 'claude' | string;
  machineId: string;
  workspacePath: string;
  sessionId: string;
  pid?: number;
  alive: boolean;
  name?: string;
  title?: string;
  kind?: string;
  entrypoint?: string;
  version?: string;
  state: AgentState;
  stateDetail?: string;
  registryStatus?: string;
  model?: string;
  effort?: string;
  permissionMode?: string;
  gitBranch?: string;
  lastPrompt?: string;
  startedAt?: number;
  lastActivityAt?: number;
  contextTokens?: number;
  pendingTools: PendingTool[];
  transcriptPath?: string;
  /** Set when the daemon on that machine spawned the session and controls it. */
  managed?: ManagedInfo;
}

export interface PendingRequest {
  requestId: string;
  toolName: string;
  displayName?: string;
  input?: Record<string, unknown>;
  toolUseId?: string;
  requiresUserInteraction?: boolean;
  suggestions?: unknown[];
  description?: string;
  at: number;
}

export interface ManagedInfo {
  sessionId: string;
  pid: number;
  cwd: string;
  name?: string;
  model?: string;
  effort?: string;
  permissionMode?: string;
  startedAt: number;
  ready: boolean;
  resumed?: boolean;
  pending?: PendingRequest;
  turns: number;
  costUsd?: number;
  exited: boolean;
  exitedAt?: number;
  lastError?: string;
}

export interface Workspace {
  id: string;
  machineId: string;
  path: string;
  agents: Agent[];
  historyCount: number;
  lastActivityAt?: number;
  openInIde: boolean;
}

export interface HostInfo {
  hostname?: string;
  os?: string;
  arch?: string;
  home?: string;
  claudeDir?: string;
  now?: number;
  macs?: string[];
}

export interface Snapshot {
  machineId: string;
  name: string;
  host: HostInfo;
  agents: Agent[];
  workspaces: Workspace[];
  at: number;
  seq: number;
  daemonVersion?: string;
  listen?: string;
  hasClaude: boolean;
}

export interface FleetEntry {
  snapshot: Snapshot;
  online: boolean;
  via: 'self' | 'direct' | 'cache' | string;
  lastSeen: number;
  receivedAt: number;
}

export interface PeerStatus {
  machineId: string;
  addr: string;
  connected: boolean;
  lastError?: string;
  lastSeen?: number;
}

export interface FleetSummary {
  machinesOnline: number;
  machinesTotal: number;
  agentsLive: number;
  attention: number;
  busy: number;
  idle: number;
}
