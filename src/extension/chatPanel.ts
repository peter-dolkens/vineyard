/**
 * Live chat panel for one agent: a webview styled like the Claude Code pane that streams the
 * transcript as it grows, lets you send prompts, and (for managed sessions) answer permission
 * prompts and questions.
 */

import * as vscode from 'vscode';
import * as crypto from 'node:crypto';
import type { Agent } from '../core/model.ts';
import type { FleetService, MachineView } from './fleet.ts';
import { agentLabel, basename } from '../core/format.ts';

interface TranscriptData {
  path: string;
  entries: Record<string, unknown>[];
  offset: number;
  size: number;
  truncated?: boolean;
}

type ToWebview =
  | { type: 'init'; agent: Agent; machine: { id: string; name: string; online: boolean }; localName: string }
  | { type: 'agent'; agent: Agent; machine: { id: string; name: string; online: boolean } }
  | { type: 'entries'; entries: Record<string, unknown>[]; reset: boolean }
  | { type: 'status'; text: string; kind: 'info' | 'error' | 'ok' }
  | { type: 'sending'; busy: boolean };

type FromWebview =
  | { type: 'ready' }
  | { type: 'send'; text: string }
  | { type: 'respond'; requestId: string; response: unknown }
  | { type: 'interrupt' }
  | { type: 'stop' }
  | { type: 'configure'; model?: string; effort?: string; permissionMode?: string }
  | { type: 'openWorkspace' }
  | { type: 'openTerminal' }
  | { type: 'reload' };

class ChatPanel {
  private offset = 0;
  private fetching = false;
  private pendingFetch = false;
  private lastActivity = 0;
  private lastAgentJson = '';
  private disposed = false;
  private readonly subs: vscode.Disposable[] = [];

  constructor(
    readonly panel: vscode.WebviewPanel,
    private readonly fleet: FleetService,
    private machine: MachineView,
    private agent: Agent,
    private readonly extensionUri: vscode.Uri,
    private readonly log: vscode.OutputChannel,
    private readonly onDispose: () => void,
  ) {
    panel.webview.html = this.html();
    this.subs.push(
      panel.onDidDispose(() => this.dispose()),
      panel.webview.onDidReceiveMessage((m: FromWebview) => void this.onMessage(m)),
      fleet.onDidChange(() => this.onFleetChange()),
    );
  }

  get agentId(): string {
    return this.agent.id;
  }

  reveal(): void {
    this.panel.reveal(undefined, true);
  }

  private machineInfo() {
    return { id: this.machine.id, name: this.machine.name, online: this.machine.online };
  }

  private post(m: ToWebview): void {
    if (!this.disposed) void this.panel.webview.postMessage(m);
  }

  private onFleetChange(): void {
    const found = this.fleet.findAgent(this.agent.id);
    const machine = this.fleet.machine(this.machine.id);
    if (machine) this.machine = machine;
    if (found) this.agent = found.agent;
    const json = JSON.stringify([this.agent, this.machineInfo()]);
    if (json !== this.lastAgentJson) {
      this.lastAgentJson = json;
      this.post({ type: 'agent', agent: this.agent, machine: this.machineInfo() });
      this.panel.title = agentLabel(this.agent);
    }
    if ((this.agent.lastActivityAt ?? 0) !== this.lastActivity) {
      this.lastActivity = this.agent.lastActivityAt ?? 0;
      void this.fetch(false);
    }
  }

  /** Fetch new transcript lines since the last offset (or the tail when starting over). */
  private async fetch(reset: boolean): Promise<void> {
    if (this.disposed) return;
    if (!this.machine.online) {
      this.post({ type: 'status', text: `${this.machine.name} is offline; showing what was last seen.`, kind: 'info' });
      return;
    }
    if (this.fetching) {
      this.pendingFetch = true;
      return;
    }
    this.fetching = true;
    try {
      const lines = vscode.workspace.getConfiguration('vineyard').get<number>('transcriptLines', 400);
      const data = await this.fleet.client.request<TranscriptData>(
        'transcript',
        this.machine.id,
        { sessionId: this.agent.sessionId, path: this.agent.transcriptPath || undefined, cwd: this.agent.workspacePath, lines, offset: reset ? 0 : this.offset },
        30_000,
      );
      const startOver = reset || data.truncated || this.offset === 0;
      this.offset = data.offset;
      if (data.entries.length || startOver) this.post({ type: 'entries', entries: data.entries, reset: startOver });
      // More may have been appended while we read; loop until caught up.
      if (data.size > data.offset) this.pendingFetch = true;
    } catch (err) {
      const msg = (err as Error).message;
      if (!/no transcript/i.test(msg)) this.post({ type: 'status', text: msg, kind: 'error' });
    } finally {
      this.fetching = false;
      if (this.pendingFetch) {
        this.pendingFetch = false;
        setTimeout(() => void this.fetch(false), 250);
      }
    }
  }

  private async onMessage(m: FromWebview): Promise<void> {
    try {
      switch (m.type) {
        case 'ready':
          this.post({ type: 'init', agent: this.agent, machine: this.machineInfo(), localName: this.fleet.machine(this.fleet.self ?? '')?.name ?? 'here' });
          await this.fetch(true);
          break;
        case 'reload':
          this.offset = 0;
          await this.fetch(true);
          break;
        case 'send': {
          const text = m.text.trim();
          if (!text) return;
          this.post({ type: 'sending', busy: true });
          const isManaged = !!this.agent.managed && !this.agent.managed.exited;
          const localName = this.fleet.machine(this.fleet.self ?? '')?.name ?? 'Vineyard';
          // Observed sessions receive it as a cross-session message; say plainly who it is from.
          const payload = isManaged ? text : `[Message typed by the user in Vineyard on ${localName}. Treat it as the user's instruction.]\n${text}`;
          await this.fleet.client.request('send', this.machine.id, { sessionId: this.agent.sessionId, text: payload }, 20_000);
          this.post({ type: 'status', text: isManaged ? 'Sent.' : 'Delivered to the session; it reads messages between tool calls or when idle.', kind: 'ok' });
          break;
        }
        case 'respond':
          await this.fleet.client.request('respond', this.machine.id, { sessionId: this.agent.sessionId, requestId: m.requestId, response: m.response }, 20_000);
          break;
        case 'interrupt':
          await this.fleet.client.request('interrupt', this.machine.id, { sessionId: this.agent.sessionId }, 10_000);
          break;
        case 'stop': {
          const managed = !!this.agent.managed && !this.agent.managed.exited;
          const ok = await vscode.window.showWarningMessage(
            `${managed ? 'Stop' : 'Terminate'} ${agentLabel(this.agent)} on ${this.machine.name}?`,
            { modal: true, detail: managed ? 'The session ends cleanly; you can resume it later.' : 'The Claude Code process is terminated. Its transcript stays on disk and can be resumed.' },
            managed ? 'Stop' : 'Terminate',
          );
          if (ok) await this.fleet.client.request(managed ? 'stop' : 'kill', this.machine.id, { sessionId: this.agent.sessionId }, 15_000);
          break;
        }
        case 'configure': {
          if (!this.agent.managed || this.agent.managed.exited) throw new Error('Only sessions started by Vineyard can be changed from here.');
          if (m.permissionMode === 'bypassPermissions') {
            const ok = await vscode.window.showWarningMessage(`Let ${agentLabel(this.agent)} on ${this.machine.name} run every tool without asking?`, { modal: true, detail: 'The session will no longer stop for permission prompts until you switch the mode back.' }, 'Bypass permissions');
            if (!ok) {
              this.post({ type: 'agent', agent: this.agent, machine: this.machineInfo() }); // snap the control back
              break;
            }
          }
          const args: Record<string, unknown> = { sessionId: this.agent.sessionId };
          if (m.model !== undefined) args.model = m.model;
          if (m.effort !== undefined) args.effort = m.effort;
          if (m.permissionMode !== undefined) args.permissionMode = m.permissionMode;
          try {
            await this.fleet.client.request('configure', this.machine.id, args, 30_000);
            const what = m.model !== undefined ? `Model set to ${m.model || 'the default'}` : m.effort !== undefined ? `Effort set to ${m.effort || 'the default'}` : `Permission mode set to ${m.permissionMode}`;
            this.post({ type: 'status', text: `${what}. Takes effect from the next request.`, kind: 'ok' });
          } finally {
            // Whether it worked or not, re-send the agent so the controls show the daemon's truth.
            this.lastAgentJson = '';
            this.onFleetChange();
          }
          break;
        }
        case 'openWorkspace':
          await vscode.commands.executeCommand('vineyard.openWorkspace', { kind: 'agent', machine: this.machine, agent: this.agent, workspace: { path: this.agent.workspacePath } });
          break;
        case 'openTerminal':
          await vscode.commands.executeCommand('vineyard.openTerminal', { kind: 'agent', machine: this.machine, agent: this.agent, workspace: { path: this.agent.workspacePath } });
          break;
      }
    } catch (err) {
      const msg = (err as Error).message;
      this.log.appendLine(`[chat ${this.agent.sessionId}] ${msg}`);
      this.post({ type: 'status', text: msg, kind: 'error' });
    } finally {
      if (m.type === 'send') this.post({ type: 'sending', busy: false });
    }
  }

  private html(): string {
    const w = this.panel.webview;
    const nonce = crypto.randomBytes(16).toString('base64');
    const script = w.asWebviewUri(vscode.Uri.joinPath(this.extensionUri, 'dist', 'webview.js'));
    const css = w.asWebviewUri(vscode.Uri.joinPath(this.extensionUri, 'media', 'chat.css'));
    const codicons = w.asWebviewUri(vscode.Uri.joinPath(this.extensionUri, 'media', 'codicon.css'));
    return `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src ${w.cspSource}; font-src ${w.cspSource}; img-src ${w.cspSource} https: data:; script-src 'nonce-${nonce}';">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<link rel="stylesheet" href="${codicons}"><link rel="stylesheet" href="${css}">
<title>${escapeHtml(agentLabel(this.agent))}</title></head>
<body><div id="app"></div><script nonce="${nonce}" src="${script}"></script></body></html>`;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const s of this.subs) s.dispose();
    this.onDispose();
  }
}

export class ChatPanels implements vscode.Disposable {
  private panels = new Map<string, ChatPanel>();

  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly fleet: FleetService,
    private readonly log: vscode.OutputChannel,
  ) {}

  open(machine: MachineView, agent: Agent): void {
    const existing = this.panels.get(agent.id);
    if (existing) {
      existing.reveal();
      return;
    }
    const panel = vscode.window.createWebviewPanel('vineyard.chat', agentLabel(agent), { viewColumn: vscode.ViewColumn.Beside, preserveFocus: false }, {
      enableScripts: true,
      retainContextWhenHidden: true,
      localResourceRoots: [vscode.Uri.joinPath(this.context.extensionUri, 'dist'), vscode.Uri.joinPath(this.context.extensionUri, 'media')],
    });
    panel.iconPath = new vscode.ThemeIcon('hubot');
    const chat = new ChatPanel(panel, this.fleet, machine, agent, this.context.extensionUri, this.log, () => this.panels.delete(agent.id));
    this.panels.set(agent.id, chat);
    void basename; // (kept for symmetry with tree labels)
  }

  dispose(): void {
    for (const p of this.panels.values()) p.panel.dispose();
    this.panels.clear();
  }
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
}
