import * as vscode from 'vscode';
import type { Agent, AgentState, Workspace } from '../core/model.ts';
import { STATE_PRIORITY, isBusy, needsAttention } from '../core/model.ts';
import type { FleetService, MachineView } from './fleet.ts';
import { STATE_LABEL, agentLabel, basename, duration, relativeTime, shortModel, tildify, tokens } from '../core/format.ts';

export type Node = MachineNode | WorkspaceNode | AgentNode;

export interface MachineNode {
  kind: 'machine';
  machine: MachineView;
}
export interface WorkspaceNode {
  kind: 'workspace';
  machine: MachineView;
  workspace: Workspace;
}
export interface AgentNode {
  kind: 'agent';
  machine: MachineView;
  workspace: Workspace;
  agent: Agent;
}

function color(id: string): vscode.ThemeColor {
  return new vscode.ThemeColor(id);
}

export function stateIcon(state: AgentState): vscode.ThemeIcon {
  switch (state) {
    case 'question':
      return new vscode.ThemeIcon('question', color('notificationsWarningIcon.foreground'));
    case 'permission':
      return new vscode.ThemeIcon('shield', color('notificationsWarningIcon.foreground'));
    case 'working':
      return new vscode.ThemeIcon('loading~spin', color('charts.blue'));
    case 'thinking':
      return new vscode.ThemeIcon('lightbulb', color('charts.purple'));
    case 'tool':
      return new vscode.ThemeIcon('tools', color('charts.blue'));
    case 'shell':
      return new vscode.ThemeIcon('terminal', color('charts.orange'));
    case 'idle':
      return new vscode.ThemeIcon('circle-large-filled', color('charts.green'));
    case 'exited':
      return new vscode.ThemeIcon('circle-slash', color('disabledForeground'));
    default:
      return new vscode.ThemeIcon('circle-outline', color('disabledForeground'));
  }
}

function dominantState(agents: Agent[]): AgentState | undefined {
  let best: AgentState | undefined;
  for (const a of agents) {
    if (!a.alive) continue;
    if (!best || STATE_PRIORITY[a.state] < STATE_PRIORITY[best]) best = a.state;
  }
  return best;
}

function countByState(agents: Agent[]): string {
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

function machineIcon(m: MachineView): vscode.ThemeIcon {
  const base = m.local ? 'device-desktop' : 'server';
  if (!m.online) return new vscode.ThemeIcon(base, color('disabledForeground'));
  const agents = m.entry.snapshot.agents.filter((a) => a.alive);
  if (agents.some((a) => needsAttention(a.state))) return new vscode.ThemeIcon(base, color('notificationsWarningIcon.foreground'));
  if (agents.some((a) => isBusy(a.state))) return new vscode.ThemeIcon(base, color('charts.blue'));
  return new vscode.ThemeIcon(base, color('charts.green'));
}

export class FleetTree implements vscode.TreeDataProvider<Node> {
  private readonly _onDidChangeTreeData = new vscode.EventEmitter<Node | undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  showHistorical: boolean;
  showExited: boolean;

  constructor(private readonly fleet: FleetService) {
    const c = vscode.workspace.getConfiguration('vineyard');
    this.showHistorical = c.get('showHistoricalWorkspaces', true);
    this.showExited = c.get('showExitedAgents', false);
    fleet.onDidChange(() => this.refresh());
  }

  refresh(): void {
    this._onDidChangeTreeData.fire(undefined);
  }

  private visibleAgents(w: Workspace): Agent[] {
    return this.showExited ? w.agents : w.agents.filter((a) => a.alive);
  }

  private sortMode(tier: 'machines' | 'workspaces' | 'agents', def: string): string {
    return vscode.workspace.getConfiguration('vineyard').get<string>(`sort.${tier}`, def);
  }

  /**
   * Every tier has a user-chosen order (vineyard.sort.*) with a stable name/id tie-break, so rows only
   * move when the chosen key changes, not on every snapshot.
   */
  getChildren(element?: Node): Node[] {
    if (!element) {
      const mode = this.sortMode('machines', 'status');
      const machines = [...this.fleet.machines()].sort((a, b) => {
        if (a.local !== b.local) return a.local ? -1 : 1; // this machine always first
        if (mode === 'status' && a.online !== b.online) return a.online ? -1 : 1;
        if (mode === 'recent') {
          const d = (b.entry.snapshot.at ?? 0) - (a.entry.snapshot.at ?? 0);
          if (d) return d;
        }
        return a.name.localeCompare(b.name) || a.id.localeCompare(b.id);
      });
      return machines.map((machine) => ({ kind: 'machine', machine }));
    }
    if (element.kind === 'machine') {
      const { machine } = element;
      const mode = this.sortMode('workspaces', 'name');
      return machine.entry.snapshot.workspaces
        .filter((w) => this.visibleAgents(w).length > 0 || (this.showHistorical && w.historyCount > 0))
        .sort((a, b) => {
          if (mode === 'attention') {
            const sa = dominantState(this.visibleAgents(a));
            const sb = dominantState(this.visibleAgents(b));
            if ((sa === undefined) !== (sb === undefined)) return sa === undefined ? 1 : -1;
            if (sa !== undefined && sb !== undefined && STATE_PRIORITY[sa] !== STATE_PRIORITY[sb]) return STATE_PRIORITY[sa] - STATE_PRIORITY[sb];
          }
          if (mode === 'recent' || mode === 'attention') {
            const d = (b.lastActivityAt ?? 0) - (a.lastActivityAt ?? 0);
            if (d) return d;
          }
          return basename(a.path).localeCompare(basename(b.path)) || a.path.localeCompare(b.path);
        })
        .map((workspace) => ({ kind: 'workspace', machine, workspace }));
    }
    if (element.kind === 'workspace') {
      const { machine, workspace } = element;
      const mode = this.sortMode('agents', 'recent');
      return this.visibleAgents(workspace)
        .sort((a, b) => {
          if (mode === 'attention') {
            const d = STATE_PRIORITY[a.state] - STATE_PRIORITY[b.state];
            if (d) return d;
          }
          if (mode === 'recent' || mode === 'attention') {
            const d = (b.startedAt ?? 0) - (a.startedAt ?? 0);
            if (d) return d;
          }
          return agentLabel(a).localeCompare(agentLabel(b)) || a.id.localeCompare(b.id);
        })
        .map((agent) => ({ kind: 'agent', machine, workspace, agent }));
    }
    return [];
  }

  getParent(element: Node): Node | undefined {
    if (element.kind === 'agent') return { kind: 'workspace', machine: element.machine, workspace: element.workspace };
    if (element.kind === 'workspace') return { kind: 'machine', machine: element.machine };
    return undefined;
  }

  getTreeItem(node: Node): vscode.TreeItem {
    switch (node.kind) {
      case 'machine':
        return this.machineItem(node.machine);
      case 'workspace':
        return this.workspaceItem(node);
      case 'agent':
        return this.agentItem(node);
    }
  }

  private machineItem(m: MachineView): vscode.TreeItem {
    const snap = m.entry.snapshot;
    const hasChildren = this.getChildren({ kind: 'machine', machine: m }).length > 0;
    const item = new vscode.TreeItem(m.name, hasChildren ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.None);
    item.id = `machine:${m.id}`;
    item.iconPath = machineIcon(m);
    item.contextValue = m.local ? 'machine-local' : m.online ? 'machine' : 'machine-offline';

    const live = snap.agents.filter((a) => a.alive);
    if (m.online) {
      item.description = !snap.hasClaude ? 'no Claude Code' : live.length ? countByState(live) : 'no agents';
    } else {
      const seen = m.entry.lastSeen || snap.at;
      item.description = seen ? `offline · last seen ${relativeTime(seen)}` : m.peer?.lastError ? 'unreachable' : 'never seen';
    }

    const md = new vscode.MarkdownString('', true);
    md.appendMarkdown(`**${m.name}**${m.local ? ' (this machine)' : ''}  \n`);
    md.appendMarkdown(`\`${snap.listen || m.host}\`  \n`);
    if (snap.host.os) md.appendMarkdown(`${snap.host.os}/${snap.host.arch ?? ''}${snap.daemonVersion ? ` · vineyardd ${snap.daemonVersion}` : ''}  \n`);
    md.appendMarkdown(m.online ? `$(pass) Online via ${m.entry.via}` : `$(circle-slash) Offline`);
    if (m.entry.lastSeen) md.appendMarkdown(`  \nLast seen ${relativeTime(m.entry.lastSeen)}`);
    if (snap.at) md.appendMarkdown(`  \nSnapshot ${relativeTime(snap.at)}`);
    if (m.peer?.lastError && !m.online) md.appendMarkdown(`  \n$(warning) ${escapeMd(m.peer.lastError)}`);
    item.tooltip = md;
    return item;
  }

  private workspaceItem(node: WorkspaceNode): vscode.TreeItem {
    const { workspace, machine } = node;
    const agents = this.visibleAgents(workspace);
    const item = new vscode.TreeItem(basename(workspace.path), agents.length ? vscode.TreeItemCollapsibleState.Expanded : vscode.TreeItemCollapsibleState.None);
    item.id = `workspace:${workspace.id}`;
    item.contextValue = agents.length ? 'workspace' : 'workspace-historical';
    const dom = dominantState(agents);
    if (dom) {
      item.iconPath = stateIcon(dom);
      item.description = countByState(agents);
    } else {
      item.iconPath = new vscode.ThemeIcon(workspace.openInIde ? 'folder-active' : 'folder', color('disabledForeground'));
      item.description = workspace.lastActivityAt ? `last ${relativeTime(workspace.lastActivityAt)}` : '';
    }
    const md = new vscode.MarkdownString('', true);
    md.appendMarkdown(`**${tildify(workspace.path, machine.entry.snapshot.host.home)}**  \n${machine.name}  \n`);
    if (workspace.openInIde) md.appendMarkdown(`$(vscode) Open in VS Code on ${machine.name}  \n`);
    md.appendMarkdown(`${agents.length} live agent${agents.length === 1 ? '' : 's'}, ${workspace.historyCount} transcript${workspace.historyCount === 1 ? '' : 's'} on disk`);
    item.tooltip = md;
    return item;
  }

  private agentItem(node: AgentNode): vscode.TreeItem {
    const { agent } = node;
    const item = new vscode.TreeItem(agentLabel(agent), vscode.TreeItemCollapsibleState.None);
    item.id = `agent:${agent.id}`;
    const managedLive = !!agent.managed && !agent.managed.exited;
    item.contextValue = (agent.alive ? `agent-${agent.state}` : 'agent-exited') + (managedLive ? '-managed' : '');
    item.iconPath = stateIcon(agent.state);

    const bits: string[] = [STATE_LABEL[agent.state]];
    if (managedLive) bits.push('managed');
    const model = shortModel(agent.model);
    if (model) bits.push(agent.effort ? `${model} · ${agent.effort}` : model);
    if (agent.lastActivityAt) bits.push(relativeTime(agent.lastActivityAt));
    item.description = bits.join(' · ');
    item.command = { command: 'vineyard.showTranscript', title: 'Show Transcript', arguments: [node] };

    const md = new vscode.MarkdownString('', true);
    md.appendMarkdown(`**${escapeMd(agentLabel(agent))}**  \n`);
    md.appendMarkdown(`$(${stateIcon(agent.state).id}) **${STATE_LABEL[agent.state]}**`);
    if (agent.stateDetail) md.appendMarkdown(` — ${escapeMd(agent.stateDetail)}`);
    md.appendMarkdown('\n\n');
    const rows: [string, string | undefined][] = [
      ['Model', agent.model ? `${shortModel(agent.model)} (${agent.model})` : undefined],
      ['Effort', agent.effort],
      ['Mode', agent.permissionMode],
      ['Context', agent.contextTokens ? `${tokens(agent.contextTokens)} tokens` : undefined],
      ['Branch', agent.gitBranch],
      ['Session', agent.name ? `${agent.name} · ${agent.sessionId}` : agent.sessionId],
      ['PID', agent.pid ? String(agent.pid) : undefined],
      ['Client', [agent.entrypoint, agent.version].filter(Boolean).join(' ')],
      ['Uptime', agent.startedAt ? duration(Date.now() - agent.startedAt) : undefined],
      ['Last activity', agent.lastActivityAt ? relativeTime(agent.lastActivityAt) : undefined],
      ['Registry', agent.registryStatus],
    ];
    for (const [k, v] of rows) if (v) md.appendMarkdown(`${k}: ${escapeMd(v)}  \n`);
    if (agent.lastPrompt) md.appendMarkdown(`\n> ${escapeMd(agent.lastPrompt.slice(0, 300))}${agent.lastPrompt.length > 300 ? '…' : ''}\n`);
    if (agent.pendingTools.length) {
      md.appendMarkdown('\nPending tools:  \n');
      for (const t of agent.pendingTools) md.appendMarkdown(`- \`${t.name}\`${t.summary ? ` ${escapeMd(t.summary)}` : ''}  \n`);
    }
    item.tooltip = md;
    return item;
  }
}

function escapeMd(s: string): string {
  return s.replace(/[\\`*_{}[\]()#+\-!|<>]/g, (c) => `\\${c}`);
}
