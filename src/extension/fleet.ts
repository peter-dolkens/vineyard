/**
 * FleetService: the extension's in-memory view of the fleet, fed by the local daemon.
 */

import * as vscode from 'vscode';
import type { Agent, FleetEntry, FleetSummary, PeerStatus } from '../core/model.ts';
import { isBusy, needsAttention } from '../core/model.ts';
import { DaemonClient } from './daemonClient.ts';

export interface MachineView {
  id: string;
  name: string;
  /** Best-effort SSH host: the advertised address without port, or the reported hostname. */
  host: string;
  local: boolean;
  entry: FleetEntry;
  online: boolean;
  peer?: PeerStatus;
}

export interface AgentTransition {
  machine: MachineView;
  previous: Agent | undefined;
  current: Agent;
}

export class FleetService implements vscode.Disposable {
  private entries = new Map<string, FleetEntry>();
  private peers: PeerStatus[] = [];
  private readonly subs: vscode.Disposable[] = [];

  private readonly _onDidChange = new vscode.EventEmitter<void>();
  readonly onDidChange = this._onDidChange.event;
  private readonly _onAgentTransition = new vscode.EventEmitter<AgentTransition>();
  readonly onAgentTransition = this._onAgentTransition.event;

  constructor(readonly client: DaemonClient) {
    this.subs.push(
      client.onFleet((f) => {
        const previous = this.entries;
        this.entries = new Map(f.entries.map((e) => [e.snapshot.machineId, e]));
        this.peers = f.peers ?? [];
        for (const e of this.entries.values()) this.diff(previous.get(e.snapshot.machineId), e);
        this._onDidChange.fire();
      }),
      client.onUpdate((e) => {
        const prev = this.entries.get(e.snapshot.machineId);
        this.entries.set(e.snapshot.machineId, e);
        this.diff(prev, e);
        this._onDidChange.fire();
      }),
      client.onPeerStatus((p) => {
        this.peers = p;
        this._onDidChange.fire();
      }),
      client.onStateChange(() => this._onDidChange.fire()),
    );
  }

  get self(): string | undefined {
    return this.client.self ?? this.client.config?.machineId;
  }

  private diff(prev: FleetEntry | undefined, cur: FleetEntry): void {
    if (!prev) return; // first sighting of a machine: nothing to compare, no notifications
    const before = new Map(prev.snapshot.agents.map((a) => [a.id, a]));
    const machine = this.view(cur);
    for (const a of cur.snapshot.agents) {
      const p = before.get(a.id);
      if (!p || p.state !== a.state) this._onAgentTransition.fire({ machine, previous: p, current: a });
    }
  }

  private view(e: FleetEntry): MachineView {
    const id = e.snapshot.machineId;
    const peer = this.peers.find((p) => p.machineId === id);
    const addr = e.snapshot.listen || peer?.addr || '';
    const host = addr ? addr.replace(/:\d+$/, '') : e.snapshot.host.hostname || id;
    return {
      id,
      name: e.snapshot.name || id.split('.')[0] || id,
      host: host || id,
      local: id === this.self,
      entry: e,
      online: e.online,
      peer,
    };
  }

  machines(): MachineView[] {
    const out = [...this.entries.values()].map((e) => this.view(e));
    // Peers we know an address for but have never heard from still deserve a row.
    for (const p of this.peers) {
      if (!this.entries.has(p.machineId)) {
        out.push({
          id: p.machineId,
          name: p.machineId.split('.')[0] || p.machineId,
          host: p.addr.replace(/:\d+$/, ''),
          local: false,
          online: false,
          peer: p,
          entry: {
            snapshot: { machineId: p.machineId, name: p.machineId.split('.')[0] || p.machineId, host: {}, agents: [], workspaces: [], at: 0, seq: 0, hasClaude: true },
            online: false,
            via: 'none',
            lastSeen: p.lastSeen ?? 0,
            receivedAt: 0,
          },
        });
      }
    }
    return out.sort((a, b) => Number(b.local) - Number(a.local) || Number(b.online) - Number(a.online) || a.name.localeCompare(b.name));
  }

  machine(id: string): MachineView | undefined {
    return this.machines().find((m) => m.id === id);
  }

  findAgent(agentId: string): { agent: Agent; machine: MachineView } | undefined {
    for (const m of this.machines()) {
      const agent = m.entry.snapshot.agents.find((a) => a.id === agentId);
      if (agent) return { agent, machine: m };
    }
    return undefined;
  }

  summary(): FleetSummary {
    const s: FleetSummary = { machinesOnline: 0, machinesTotal: 0, agentsLive: 0, attention: 0, busy: 0, idle: 0 };
    for (const m of this.machines()) {
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

  /** Ask every online machine to re-probe now. Cheap: one tiny request each. */
  refreshAll(): void {
    for (const m of this.machines()) {
      if (m.online) void this.client.request('probe', m.id, undefined, 5000).catch(() => undefined);
    }
  }

  dispose(): void {
    for (const s of this.subs) s.dispose();
    this._onDidChange.dispose();
    this._onAgentTransition.dispose();
  }
}
