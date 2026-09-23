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
  | 'done'
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
  done: 7,
  unknown: 8,
  exited: 9,
};

export function needsAttention(state: AgentState): boolean {
  return state === 'question' || state === 'permission';
}

export function isBusy(state: AgentState): boolean {
  return state === 'working' || state === 'thinking' || state === 'tool';
}

/**
 * Whether Vineyard should pop notifications for this agent. A session on this machine that Vineyard
 * did not start (the Claude Code pane, a terminal) already prompts the user through its own UI, so a
 * Vineyard notification would be a duplicate. Remote sessions and locally managed ones have no other
 * surface here and do notify.
 */
export function notifiesHere(agent: Pick<Agent, 'managed'>, machineLocal: boolean): boolean {
  return !machineLocal || agent.managed !== undefined;
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
  /** Agent-tool invocations under this session, every depth, in spawn order. */
  subagents?: Subagent[];
  /** Shell commands left running in the background, plus ones finished in the last half hour, in start order. */
  tasks?: BackgroundTask[];
}

/**
 * A Bash call made with run_in_background, or a foreground one that outlived its timeout and was
 * moved to the background, tracked until its task notification arrives.
 */
export interface BackgroundTask {
  toolUseId: string;
  taskId?: string;
  description?: string;
  kind?: string;
  /** running | completed | failed | … */
  state: string;
  startedAt?: number;
  endedAt?: number;
}

/**
 * One Agent-tool invocation: its own transcript beside the session's. `parentAgentId` is empty when
 * the session itself spawned it, otherwise it names another subagent of the same session.
 */
export interface Subagent {
  agentId: string;
  parentAgentId?: string;
  depth?: number;
  type?: string;
  description?: string;
  model?: string;
  background?: boolean;
  state: AgentState;
  stateDetail?: string;
  pendingTools?: PendingTool[];
  startedAt?: number;
  lastActivityAt?: number;
  contextTokens?: number;
  transcriptPath?: string;
  toolUseId?: string;
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

/**
 * One row of the model picker Claude Code offers this account, as the harness reported it when the
 * managed session started. `value` is what set_model / --model accept ('default' = Claude Code's own
 * default); `resolvedModel` is the wire id it maps to.
 */
export interface ModelInfo {
  value: string;
  resolvedModel?: string;
  displayName: string;
  description?: string;
  /** Empty or missing when the model takes no effort setting. */
  supportedEffortLevels?: string[];
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
  /** What this session's Claude Code offers in its model picker; absent until it has answered initialize. */
  models?: ModelInfo[];
  /** Slash commands it offers (no leading slash) and the account it is signed in as, from the same handshake. */
  commands?: CommandInfo[];
  account?: string;
  accountOrg?: string;
  accountPlan?: string;
  /** The account's limit report as this session last saw it. */
  usage?: Usage;
}

export interface UsageWindow {
  /** Share used, 0..1. */
  utilization: number;
  /** Epoch ms. */
  resetsAt?: number;
}

/**
 * The account's usage limits as Claude Code reports them from the API's rate-limit headers. Window
 * keys: five_hour, seven_day, seven_day_opus, seven_day_sonnet, seven_day_overage_included.
 */
export interface Usage {
  status: 'allowed' | 'allowed_warning' | 'rejected' | string;
  rateLimitType?: string;
  utilization?: number;
  resetsAt?: number;
  windows?: Record<string, UsageWindow>;
  isUsingOverage?: boolean;
  overageStatus?: string;
  at: number;
}

export interface CommandInfo {
  name: string;
  description?: string;
  argumentHint?: string;
}

/** A file sent with a prompt: an image block for a managed session, inlined text otherwise. Base64 data. */
export interface Attachment {
  name: string;
  mediaType: string;
  data: string;
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
  /** Newest account-limit report from any managed session on the machine; limits are per account. */
  usage?: Usage;
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
