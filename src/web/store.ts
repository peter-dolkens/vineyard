/**
 * The web app's view of the fleet, fed by the server's event stream: the same picture the extension's
 * FleetService keeps (src/extension/fleet.ts), without VS Code. Also what the menus list at each level,
 * in the order the user chose, so the screens only lay rows out.
 */

import type { Agent, AgentState, BackgroundTask, FleetEntry, FleetSummary, PeerStatus, Subagent, Workspace } from '../core/model.ts';
import { STATE_PRIORITY, isBusy, isLongUnseen, needsAttention } from '../core/model.ts';
import { findSubagent, subagentActive, subagentAsAgent, subagentChildren, subagentDescendants } from '../core/subagents.ts';
import { taskActive } from '../core/tasks.ts';
import { agentLabel, basename } from '../core/format.ts';
import type { LinkState, ServerMsg } from './api.ts';

export interface MachineView {
  id: string;
  name: string;
  local: boolean;
  entry: FleetEntry;
  online: boolean;
  peer?: PeerStatus;
}

export interface Transition {
  machine: MachineView;
  previous: Agent | undefined;
  current: Agent;
}

/** What the lists show and in which order; the Settings screen edits these (settings.ts stores them). */
export interface ViewPrefs {
  showHistorical: boolean;
  showExited: boolean;
  showFinishedSubagents: boolean;
  hideUnseenDays: number;
  sortMachines: 'status' | 'name' | 'recent';
  sortWorkspaces: 'name' | 'recent' | 'attention';
  sortAgents: 'recent' | 'name' | 'attention';
}

export class FleetStore {
  state: LinkState = 'offline';
  error: string | undefined;
  self: string | undefined;
  /** True once a fleet message has arrived, so screens can tell "loading" from "empty". */
  loaded = false;
  private entries = new Map<string, FleetEntry>();
  private peers: PeerStatus[] = [];
  private changeFns: (() => void)[] = [];
  private transitionFns: ((t: Transition) => void)[] = [];

  onChange(fn: () => void): () => void {
    this.changeFns.push(fn);
    return () => (this.changeFns = this.changeFns.filter((f) => f !== fn));
  }

  onTransition(fn: (t: Transition) => void): void {
    this.transitionFns.push(fn);
  }

  private changed(): void {
    for (const fn of this.changeFns) fn();
  }

  apply(m: ServerMsg): void {
    switch (m.t) {
      case 'state':
        this.state = m.state;
        this.error = m.error || undefined;
        break;
      case 'fleet': {
        const previous = this.entries;
        this.self = m.self;
        this.entries = new Map(m.entries.map((e) => [e.snapshot.machineId, e]));
        this.peers = m.peers ?? [];
        this.loaded = true;
        for (const e of this.entries.values()) this.diff(previous.get(e.snapshot.machineId), e);
        break;
      }
      case 'update': {
        const prev = this.entries.get(m.entry.snapshot.machineId);
        this.entries.set(m.entry.snapshot.machineId, m.entry);
        this.diff(prev, m.entry);
        break;
      }
      case 'peerstatus':
        this.peers = m.peers ?? [];
        break;
    }
    this.changed();
  }

  private diff(prev: FleetEntry | undefined, cur: FleetEntry): void {
    if (!prev) return; // first sighting of a machine: nothing to compare
    const before = new Map(prev.snapshot.agents.map((a) => [a.id, a]));
    const machine = this.view(cur);
    for (const a of cur.snapshot.agents) {
      const p = before.get(a.id);
      if (!p || p.state !== a.state) for (const fn of this.transitionFns) fn({ machine, previous: p, current: a });
    }
  }

  private view(e: FleetEntry): MachineView {
    const id = e.snapshot.machineId;
    return { id, name: e.snapshot.name || id.split('.')[0] || id, local: id === this.self, entry: e, online: e.online, peer: this.peers.find((p) => p.machineId === id) };
  }

  /** Every machine, including peers known by address that have never reported. */
  allMachines(): MachineView[] {
    const out = [...this.entries.values()].map((e) => this.view(e));
    for (const p of this.peers) {
      if (this.entries.has(p.machineId)) continue;
      const name = p.machineId.split('.')[0] || p.machineId;
      out.push({
        id: p.machineId,
        name,
        local: false,
        online: false,
        peer: p,
        entry: { snapshot: { machineId: p.machineId, name, host: {}, agents: [], workspaces: [], at: 0, seq: 0, hasClaude: true }, online: false, via: 'none', lastSeen: p.lastSeen ?? 0, receivedAt: 0 },
      });
    }
    return out;
  }

  machine(id: string): MachineView | undefined {
    return this.allMachines().find((m) => m.id === id);
  }

  /** A session by id, or a subagent by "<session id>/<agent id>" as an Agent-shaped view with its parent. */
  findAgent(agentId: string): { agent: Agent; machine: MachineView; parent?: Agent } | undefined {
    for (const m of this.allMachines()) {
      const agent = m.entry.snapshot.agents.find((a) => a.id === agentId);
      if (agent) return { agent, machine: m };
      const sub = findSubagent(m.entry.snapshot.agents, agentId);
      if (sub) return { agent: subagentAsAgent(sub.parent, sub.sub), machine: m, parent: sub.parent };
    }
    return undefined;
  }

  workspaceOf(machine: MachineView, path: string): Workspace | undefined {
    return machine.entry.snapshot.workspaces.find((w) => w.path === path);
  }

  summary(): FleetSummary {
    const s: FleetSummary = { machinesOnline: 0, machinesTotal: 0, agentsLive: 0, attention: 0, busy: 0, idle: 0 };
    for (const m of this.allMachines()) {
      s.machinesTotal++;
      if (!m.online) continue;
      s.machinesOnline++;
      for (const a of m.entry.snapshot.agents) {
        if (!a.alive) continue;
        s.agentsLive++;
        if (needsAttention(a.state)) s.attention++;
        else if (isBusy(a.state)) s.busy++;
        else s.idle++;
      }
    }
    return s;
  }

  /** Live agents on online machines waiting for an answer, longest waiting first. */
  attention(): { agent: Agent; machine: MachineView }[] {
    const out: { agent: Agent; machine: MachineView }[] = [];
    for (const m of this.allMachines()) {
      if (!m.online) continue;
      for (const a of m.entry.snapshot.agents) if (a.alive && needsAttention(a.state)) out.push({ agent: a, machine: m });
    }
    return out.sort((x, y) => STATE_PRIORITY[x.agent.state] - STATE_PRIORITY[y.agent.state] || (x.agent.lastActivityAt ?? 0) - (y.agent.lastActivityAt ?? 0));
  }
}

// ---- what each menu level lists -----------------------------------------------------------------

export function machinesFor(all: MachineView[], p: ViewPrefs, now = Date.now()): MachineView[] {
  return [...all]
    .filter((m) => !isLongUnseen({ local: m.local, online: m.online, lastSeen: m.entry.lastSeen || m.entry.snapshot.at }, p.hideUnseenDays, now))
    .sort((a, b) => {
      if (a.local !== b.local) return a.local ? -1 : 1; // this machine always first
      if (p.sortMachines === 'status' && a.online !== b.online) return a.online ? -1 : 1;
      if (p.sortMachines === 'recent') {
        const d = (b.entry.snapshot.at ?? 0) - (a.entry.snapshot.at ?? 0);
        if (d) return d;
      }
      return a.name.localeCompare(b.name) || a.id.localeCompare(b.id);
    });
}

export function visibleAgents(w: Workspace, p: ViewPrefs): Agent[] {
  return p.showExited ? w.agents : w.agents.filter((a) => a.alive);
}

export function dominantState(agents: Agent[]): AgentState | undefined {
  let best: AgentState | undefined;
  for (const a of agents) {
    if (!a.alive) continue;
    if (!best || STATE_PRIORITY[a.state] < STATE_PRIORITY[best]) best = a.state;
  }
  return best;
}

export function workspacesFor(m: MachineView, p: ViewPrefs): Workspace[] {
  return m.entry.snapshot.workspaces
    .filter((w) => visibleAgents(w, p).length > 0 || (p.showHistorical && w.historyCount > 0))
    .sort((a, b) => {
      if (p.sortWorkspaces === 'attention') {
        const sa = dominantState(visibleAgents(a, p));
        const sb = dominantState(visibleAgents(b, p));
        if ((sa === undefined) !== (sb === undefined)) return sa === undefined ? 1 : -1;
        if (sa !== undefined && sb !== undefined && STATE_PRIORITY[sa] !== STATE_PRIORITY[sb]) return STATE_PRIORITY[sa] - STATE_PRIORITY[sb];
      }
      if (p.sortWorkspaces === 'recent' || p.sortWorkspaces === 'attention') {
        const d = (b.lastActivityAt ?? 0) - (a.lastActivityAt ?? 0);
        if (d) return d;
      }
      return basename(a.path).localeCompare(basename(b.path)) || a.path.localeCompare(b.path);
    });
}

export function agentsFor(w: Workspace, p: ViewPrefs): Agent[] {
  return visibleAgents(w, p).sort((a, b) => {
    if (p.sortAgents === 'attention') {
      const d = STATE_PRIORITY[a.state] - STATE_PRIORITY[b.state];
      if (d) return d;
    }
    if (p.sortAgents === 'recent' || p.sortAgents === 'attention') {
      const d = (b.startedAt ?? 0) - (a.startedAt ?? 0);
      if (d) return d;
    }
    return agentLabel(a).localeCompare(agentLabel(b)) || a.id.localeCompare(b.id);
  });
}

/** A session's subagents as a flat list in tree order, each with its depth, honouring the finished toggle. */
export function subagentRows(agent: Agent, p: ViewPrefs): { sub: Subagent; depth: number }[] {
  const out: { sub: Subagent; depth: number }[] = [];
  const walk = (parentAgentId: string | undefined, depth: number) => {
    for (const s of subagentChildren(agent.subagents, parentAgentId)) {
      const show = p.showFinishedSubagents || subagentActive(s) || subagentDescendants(agent.subagents, s.agentId).some(subagentActive);
      if (!show) continue;
      out.push({ sub: s, depth });
      walk(s.agentId, depth + 1);
    }
  };
  walk(undefined, 0);
  return out;
}

export function tasksFor(agent: Agent, p: ViewPrefs): BackgroundTask[] {
  const tasks = agent.tasks ?? [];
  return p.showFinishedSubagents ? tasks : tasks.filter(taskActive);
}

/** "2 need you · 1 working · 3 idle" over live agents. */
export function countByState(agents: Agent[]): string {
  const live = agents.filter((a) => a.alive);
  const attention = live.filter((a) => needsAttention(a.state)).length;
  const busy = live.filter((a) => isBusy(a.state)).length;
  const idle = live.length - attention - busy;
  const parts: string[] = [];
  if (attention) parts.push(`${attention} need${attention === 1 ? 's' : ''} you`);
  if (busy) parts.push(`${busy} working`);
  if (idle) parts.push(`${idle} idle`);
  return parts.join(' · ');
}
