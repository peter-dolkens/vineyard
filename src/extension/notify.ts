import * as vscode from 'vscode';
import type { AgentTransition, FleetService } from './fleet.ts';
import { isBusy, needsAttention } from '../core/model.ts';
import { STATE_LABEL, agentLabel, basename } from '../core/format.ts';

/** Surfaces state transitions the user cares about as VS Code notifications. */
export class Notifier implements vscode.Disposable {
  private readonly sub: vscode.Disposable;
  private lastShown = new Map<string, number>();

  constructor(private readonly fleet: FleetService) {
    this.sub = fleet.onAgentTransition((t) => this.handle(t));
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
  }
}
