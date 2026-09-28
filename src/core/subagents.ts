/**
 * Subagents on the extension side: the daemon sends each session's Agent-tool invocations as a flat
 * list with parent links; these helpers turn that into tree children and, when one is opened on its
 * own, into an Agent-shaped view the chat panel and transcript viewer already understand.
 */

import type { Agent, AgentState, Subagent } from './model.ts';
import { isBusy, needsAttention } from './model.ts';
import { STATE_LABEL, subagentLabel } from './format.ts';

/** Separator between a session's agent id and a subagent id; never appears in either. */
const SEP = '/';

export function subagentId(parent: Agent, sub: Subagent): string {
  return `${parent.id}${SEP}${sub.agentId}`;
}

export function isSubagentId(id: string): boolean {
  return id.includes(SEP);
}

/** Direct children of `parentAgentId` (undefined for the session's own spawns), in the order sent. */
export function subagentChildren(subs: Subagent[] | undefined, parentAgentId?: string): Subagent[] {
  return (subs ?? []).filter((s) => (s.parentAgentId || undefined) === (parentAgentId || undefined));
}

/** A subagent still doing something, or blocked on something. */
export function subagentActive(sub: Subagent): boolean {
  return isBusy(sub.state) || needsAttention(sub.state);
}

/** Every descendant of `parentAgentId`, depth first. */
export function subagentDescendants(subs: Subagent[] | undefined, parentAgentId?: string): Subagent[] {
  const out: Subagent[] = [];
  for (const c of subagentChildren(subs, parentAgentId)) {
    out.push(c, ...subagentDescendants(subs, c.agentId));
  }
  return out;
}

export function findSubagent(agents: Agent[], id: string): { parent: Agent; sub: Subagent } | undefined {
  if (!isSubagentId(id)) return undefined;
  for (const parent of agents) {
    for (const sub of parent.subagents ?? []) {
      if (subagentId(parent, sub) === id) return { parent, sub };
    }
  }
  return undefined;
}

/**
 * The subagent as an Agent: same session, machine and workspace as its parent, its own transcript
 * and state. `kind` is "subagent" so UI that sends, stops or renames knows not to.
 */
export function subagentAsAgent(parent: Agent, sub: Subagent): Agent {
  return {
    id: subagentId(parent, sub),
    provider: parent.provider,
    machineId: parent.machineId,
    workspacePath: parent.workspacePath,
    sessionId: parent.sessionId,
    pid: parent.pid,
    alive: parent.alive && sub.state !== 'exited',
    name: subagentLabel(sub),
    kind: 'subagent',
    entrypoint: parent.entrypoint,
    version: parent.version,
    state: sub.state,
    stateDetail: sub.stateDetail,
    model: sub.model,
    permissionMode: parent.permissionMode,
    gitBranch: parent.gitBranch,
    startedAt: sub.startedAt,
    lastActivityAt: sub.lastActivityAt,
    contextTokens: sub.contextTokens,
    pendingTools: sub.pendingTools ?? [],
    transcriptPath: sub.transcriptPath,
  };
}

/** The little a state summary needs from a session; the chat webview's own Agent type fits too. */
interface Delegator {
  alive: boolean;
  state: AgentState | string;
  subagents?: Pick<Subagent, 'state'>[];
}

/**
 * Subagents still at work for a session whose own turn has ended. Claude Code reports a session that
 * handed work to background subagents as idle while they run; this is how many it is waiting on.
 */
export function delegatedTo(a: Delegator): Pick<Subagent, 'state'>[] {
  if (!a.alive || isBusy(a.state as AgentState) || needsAttention(a.state as AgentState)) return [];
  return (a.subagents ?? []).filter((s) => subagentActive(s as Subagent));
}

/**
 * The state to show and count a session by. One waiting on its subagents is working; one whose
 * subagent is asking something needs you, as if it asked itself.
 */
export function shownState(a: Delegator): AgentState {
  const subs = delegatedTo(a);
  if (subs.some((s) => s.state === 'question')) return 'question';
  if (subs.some((s) => s.state === 'permission')) return 'permission';
  return subs.length ? 'working' : (a.state as AgentState);
}

/** "2 subagents working", "A subagent is asking a question", or the session's own state. */
export function shownStateLabel(a: Delegator): string {
  const subs = delegatedTo(a);
  if (!subs.length) return STATE_LABEL[a.state as AgentState] ?? String(a.state);
  const shown = shownState(a);
  if (shown === 'question') return 'A subagent is asking a question';
  if (shown === 'permission') return 'A subagent is waiting for permission';
  return `${subs.length} subagent${subs.length === 1 ? '' : 's'} working`;
}

/** Whether the session is idle itself but waiting on subagents: shown with its own icon. */
export function isDelegating(a: Delegator): boolean {
  return delegatedTo(a).length > 0;
}
