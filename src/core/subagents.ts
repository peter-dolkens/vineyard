/**
 * Subagents on the extension side: the daemon sends each session's Agent-tool invocations as a flat
 * list with parent links; these helpers turn that into tree children and, when one is opened on its
 * own, into an Agent-shaped view the chat panel and transcript viewer already understand.
 */

import type { Agent, Subagent } from './model.ts';
import { isBusy, needsAttention } from './model.ts';
import { subagentLabel } from './format.ts';

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
