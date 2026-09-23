import * as vscode from 'vscode';
import type { AgentTransition, FleetService } from './fleet.ts';
import { isBusy, needsAttention } from '../core/model.ts';
import { STATE_LABEL, agentLabel, basename } from '../core/format.ts';
import { usageWarning } from '../core/usage.ts';

/** Surfaces state transitions the user cares about as VS Code notifications. */
export class Notifier implements vscode.Disposable {
  private readonly sub: vscode.Disposable;
  private readonly usageSub: vscode.Disposable;
  private lastShown = new Map<string, number>();
  /** Limit warnings already shown, by machine, window and reset time: one per window per period. */
  private limitsShown = new Set<string>();

  constructor(private readonly fleet: FleetService) {
    this.sub = fleet.onAgentTransition((t) => this.handle(t));
    this.usageSub = fleet.onDidChange(() => this.checkLimits());
  }

  /** Like the Claude Code pane's banner: once when a window crosses the warning line, once when it is hit. */
  private checkLimits(): void {
    if (!vscode.workspace.getConfiguration('vineyard').get<boolean>('notify.limits', true)) return;
    for (const m of this.fleet.machines()) {
      const w = usageWarning(m.entry.snapshot.usage);
      if (!w) continue;
      const key = `${m.id}:${w.row.key}:${w.row.resetsAt ?? 0}:${w.rejected ? 'hit' : 'warn'}`;
      if (this.limitsShown.has(key)) continue;
      this.limitsShown.add(key);
      void vscode.window.showWarningMessage(`Claude on ${m.name}: ${w.text}`, 'Show Vineyard').then((choice) => {
        if (choice) void vscode.commands.executeCommand('vineyard.focus');
      });
    }
  }

  private handle(t: AgentTransition): void {
    const cfg = vscode.workspace.getConfiguration('vineyard');
    const { current, previous, machine } = t;
    if (!current.alive) return;

    const wantsAttention = cfg.get<boolean>('notify.attention', true) && needsAttention(current.state) && !(previous && needsAttention(previous.state));
    const finished = cfg.get<boolean>('notify.finished', false) && current.state === 'idle' && previous !== undefined && isBusy(previous.state);
    if (!wantsAttention && !finished) return;

    // Debounce: the same agent flapping between two states should not spam.
    const key = `${current.id}:${current.state}`;
    const last = this.lastShown.get(key) ?? 0;
    if (Date.now() - last < 20_000) return;
    this.lastShown.set(key, Date.now());

    const where = `${basename(current.workspacePath)} on ${machine.name}`;
    const headline = wantsAttention ? `${agentLabel(current)} is ${STATE_LABEL[current.state].toLowerCase()}` : `${agentLabel(current)} finished`;
    const detail = current.stateDetail ? ` — ${current.stateDetail}` : '';
    const message = `${headline} (${where})${detail}`;

    const show = wantsAttention ? vscode.window.showWarningMessage : vscode.window.showInformationMessage;
    void show(message, 'Show Transcript', 'Open Terminal').then((choice) => {
      const found = this.fleet.findAgent(current.id);
      if (!found) return;
      const workspace = found.machine.entry.snapshot.workspaces.find((w) => w.path === found.agent.workspacePath);
      const node = { kind: 'agent', machine: found.machine, workspace, agent: found.agent };
      if (choice === 'Show Transcript') void vscode.commands.executeCommand('vineyard.showTranscript', node);
      if (choice === 'Open Terminal') void vscode.commands.executeCommand('vineyard.openTerminal', node);
    });
  }

  dispose(): void {
    this.sub.dispose();
    this.usageSub.dispose();
  }
}
