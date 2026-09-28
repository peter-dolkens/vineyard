/**
 * The web app's side of one chat: what src/extension/chatPanel.ts does for a VS Code webview, for the
 * same chat UI (src/webview/main.ts) running in an iframe (frame.ts). It streams the transcript from
 * the owning machine, passes agent updates in, and carries out what the chat asks for, with the app's
 * own dialogs where VS Code would show a modal.
 */

import type { Agent, Attachment, Usage } from '../core/model.ts';
import { agentLabel } from '../core/format.ts';
import { attachmentMediaType, attachmentProblem } from '../core/attachments.ts';
import { subagentId } from '../core/subagents.ts';
import type { SettingsDefaults } from '../extension/sessionPrefs.ts';
import type { Api, ServerInfo } from './api.ts';
import type { FleetStore, MachineView } from './store.ts';
import { sessionPrefs, settings } from './settings.ts';
import { confirm, localId } from './ui.ts';

const HELP_URL = 'https://github.com/peter-dolkens/vineyard#readme';
const ISSUES_URL = 'https://github.com/peter-dolkens/vineyard/issues/new';

interface TranscriptData {
  path: string;
  entries: Record<string, unknown>[];
  offset: number;
  size: number;
  truncated?: boolean;
}

interface PendingAttachment extends Attachment {
  id: string;
  size: number;
}

/** A file the frame picked, pasted or had dropped on it (images already scaled down), as raw bytes. */
export interface PickedFile {
  /** The frame's id for it, so its thumbnail finds the chip. */
  id?: string;
  name: string;
  buffer: ArrayBuffer;
}

type FromFrame =
  | { type: 'ready' }
  | { type: 'send'; text: string }
  | { type: 'respond'; requestId: string; response: unknown }
  | { type: 'interrupt' }
  | { type: 'stop' }
  | { type: 'configure'; model?: string; effort?: string; permissionMode?: string; transient?: boolean }
  | { type: 'login' }
  | { type: 'rename'; title: string }
  | { type: 'openWorkspace' }
  | { type: 'takeOver' }
  | { type: 'openTerminal' }
  | { type: 'reload' }
  | { type: 'attach' }
  | { type: 'attachFiles'; files: PickedFile[] }
  | { type: 'removeAttachment'; id: string }
  | { type: 'slash'; text: string; confirm?: string }
  | { type: 'action'; id: string }
  | { type: 'openSubagent'; agentId: string }
  | { type: 'stopTask'; taskId: string }
  | { type: 'nav'; to: 'back' };

export interface ChatDeps {
  api: Api;
  store: FleetStore;
  info: () => ServerInfo | undefined;
  navigate(route: string): void;
  back(): void;
  subagentRoute(id: string): string;
  settingsRoute(): string;
  historyRoute(machineId: string, cwd: string): string;
  takeOver(machine: MachineView, agent: Agent): Promise<void>;
  login(machine: MachineView): Promise<void>;
}

export class ChatHost {
  private offset = 0;
  private fetching = false;
  private pendingFetch = false;
  private lastActivity = 0;
  private lastAgentJson = '';
  private ready = false;
  private initSent = false;
  private disposed = false;
  private attachments: PendingAttachment[] = [];
  private agent: Agent | undefined;
  private machine: MachineView | undefined;
  private readonly unsubscribe: () => void;
  private readonly onWindowMessage = (ev: MessageEvent) => {
    if (ev.source !== this.frame.contentWindow || ev.origin !== location.origin) return;
    void this.onMessage(ev.data as FromFrame);
  };

  constructor(
    private readonly frame: HTMLIFrameElement,
    readonly agentId: string,
    private readonly deps: ChatDeps,
  ) {
    window.addEventListener('message', this.onWindowMessage);
    this.unsubscribe = deps.store.onChange(() => this.onFleetChange());
    this.lookUp();
  }

  /** The agent this chat shows, once the fleet has it. */
  get current(): { agent: Agent; machine: MachineView } | undefined {
    return this.agent && this.machine ? { agent: this.agent, machine: this.machine } : undefined;
  }

  private lookUp(): void {
    const found = this.deps.store.findAgent(this.agentId);
    if (found) {
      this.agent = found.agent;
      this.machine = found.machine;
    }
  }

  private machineInfo(): { id: string; name: string; online: boolean; usage?: Usage } {
    const m = this.machine!;
    return { id: m.id, name: m.name, online: m.online, usage: m.entry.snapshot.usage };
  }

  private post(m: unknown): void {
    if (!this.disposed) this.frame.contentWindow?.postMessage(m, location.origin);
  }

  private status(text: string, kind: 'info' | 'error' | 'ok'): void {
    this.post({ type: 'status', text, kind });
  }

  private get managedLive(): boolean {
    return !!this.agent?.managed && !this.agent.managed.exited;
  }

  private sendInit(): void {
    if (!this.ready || this.initSent || !this.agent || !this.machine) return;
    this.initSent = true;
    this.lastAgentJson = JSON.stringify([this.agent, this.machineInfo()]);
    this.lastActivity = this.agent.lastActivityAt ?? 0;
    this.post({ type: 'init', agent: this.agent, machine: this.machineInfo(), localName: this.deps.info()?.name ?? 'here', extVersion: this.deps.info()?.version });
    this.postAttachments();
    void this.fetch(true);
  }

  private onFleetChange(): void {
    this.lookUp();
    if (!this.agent || !this.machine) {
      if (this.ready && this.deps.store.loaded) this.status('This session is no longer in the fleet.', 'info');
      return;
    }
    if (!this.initSent) return this.sendInit();
    const json = JSON.stringify([this.agent, this.machineInfo()]);
    if (json !== this.lastAgentJson) {
      this.lastAgentJson = json;
      this.post({ type: 'agent', agent: this.agent, machine: this.machineInfo() });
    }
    if ((this.agent.lastActivityAt ?? 0) !== this.lastActivity) {
      this.lastActivity = this.agent.lastActivityAt ?? 0;
      void this.fetch(false);
    }
  }

  /** Fetch transcript lines written since the last offset (or the tail when starting over). */
  private async fetch(reset: boolean): Promise<void> {
    if (this.disposed || !this.agent || !this.machine) return;
    if (!this.machine.online) {
      this.status(`${this.machine.name} is offline; showing what was last seen.`, 'info');
      return;
    }
    if (this.fetching) {
      this.pendingFetch = true;
      return;
    }
    this.fetching = true;
    try {
      const a = this.agent;
      const data = await this.deps.api.request<TranscriptData>('transcript', this.machine.id, { sessionId: a.sessionId, path: a.transcriptPath || undefined, cwd: a.workspacePath, lines: settings().transcriptLines, offset: reset ? 0 : this.offset }, 30_000);
      const startOver = reset || !!data.truncated || this.offset === 0;
      this.offset = data.offset;
      if (data.entries.length || startOver) this.post({ type: 'entries', entries: data.entries, reset: startOver });
      if (data.size > data.offset) this.pendingFetch = true; // more arrived while we read
    } catch (err) {
      const msg = (err as Error).message;
      if (!/no transcript/i.test(msg)) this.status(msg, 'error');
    } finally {
      this.fetching = false;
      if (this.pendingFetch && !this.disposed) {
        this.pendingFetch = false;
        setTimeout(() => void this.fetch(false), 250);
      }
    }
  }

  private async onMessage(m: FromFrame): Promise<void> {
    if (m.type === 'ready') {
      this.ready = true;
      this.initSent = false;
      this.offset = 0;
      if (this.agent) this.sendInit();
      else if (this.deps.store.loaded) this.status('This session is not in the fleet (any more).', 'info');
      return;
    }
    if (m.type === 'nav') return this.deps.back();
    const agent = this.agent;
    const machine = this.machine;
    if (!agent || !machine) return;
    try {
      switch (m.type) {
        case 'reload':
          this.offset = 0;
          await this.fetch(true);
          break;
        case 'send': {
          const text = m.text.trim();
          const attachments: Attachment[] = this.attachments.map(({ name, mediaType, data }) => ({ name, mediaType, data }));
          if (!text && !attachments.length) return;
          this.post({ type: 'sending', busy: true });
          const managed = this.managedLive;
          const via = this.deps.info()?.name ?? 'a Vineyard machine';
          // Observed sessions get it as a cross-session message; say plainly who it is from.
          const payload = managed ? text : `[Message typed by the user in the Vineyard web app (served by ${via}). Treat it as the user's instruction.]\n${text}`;
          await this.deps.api.request('send', machine.id, { sessionId: agent.sessionId, text: payload, attachments: attachments.length ? attachments : undefined }, 60_000);
          this.attachments = [];
          this.postAttachments();
          this.status(managed ? 'Sent.' : 'Delivered to the session; it reads messages between tool calls or when idle.', 'ok');
          break;
        }
        case 'attach':
          this.status('Pick files from the chat’s + button.', 'info');
          break;
        case 'attachFiles':
          this.addAttachments(m.files);
          break;
        case 'removeAttachment':
          this.attachments = this.attachments.filter((a) => a.id !== m.id);
          this.postAttachments();
          break;
        case 'slash': {
          if (!this.managedLive) throw new Error('Slash commands only work in sessions started by Vineyard.');
          if (m.confirm && !(await confirm(m.text.split(/\s+/)[0]!, m.confirm, 'Continue', true))) break;
          await this.deps.api.request('send', machine.id, { sessionId: agent.sessionId, text: m.text }, 20_000);
          this.status(`Sent ${m.text.split(/\s+/)[0]}.`, 'ok');
          break;
        }
        case 'action':
          this.action(m.id, agent, machine);
          break;
        case 'openSubagent': {
          const sub = agent.subagents?.find((s) => s.agentId === m.agentId);
          if (!sub) throw new Error('That subagent is no longer listed.');
          this.deps.navigate(this.deps.subagentRoute(subagentId(agent, sub)));
          break;
        }
        case 'respond':
          await this.deps.api.request('respond', machine.id, { sessionId: agent.sessionId, requestId: m.requestId, response: m.response }, 20_000);
          break;
        case 'interrupt':
          await this.deps.api.request('interrupt', machine.id, { sessionId: agent.sessionId }, 10_000);
          break;
        case 'stopTask':
          try {
            await this.deps.api.request('stoptask', machine.id, { sessionId: agent.sessionId, taskId: m.taskId }, 25_000);
          } catch (err) {
            this.post({ type: 'taskStopFailed', taskId: m.taskId });
            throw new Error(`Could not stop task ${m.taskId}: ${(err as Error).message}`);
          }
          break;
        case 'stop': {
          const managed = this.managedLive;
          if (!managed && agent.provider === 'codex') throw new Error('A Codex thread started elsewhere has no process Vineyard can end; stop it in the app that runs it.');
          const ok = await confirm(
            `${managed ? 'Stop' : 'Terminate'} ${agentLabel(agent)} on ${machine.name}?`,
            managed ? 'The session ends cleanly; you can resume it later.' : 'The Claude Code process is terminated. Its transcript stays on disk and can be resumed.',
            managed ? 'Stop' : 'Terminate',
            true,
          );
          if (ok) await this.deps.api.request(managed ? 'stop' : 'kill', machine.id, { sessionId: agent.sessionId }, 15_000);
          break;
        }
        case 'configure':
          await this.configure(m, agent, machine);
          break;
        case 'login':
          await this.deps.login(machine);
          break;
        case 'rename': {
          const title = m.title.trim();
          if (!title) break;
          await this.deps.api.request('rename', machine.id, { sessionId: agent.sessionId, title, path: agent.transcriptPath || undefined, cwd: agent.workspacePath }, 20_000);
          this.status(`Renamed to “${title}”.`, 'ok');
          break;
        }
        case 'takeOver':
          await this.deps.takeOver(machine, agent);
          break;
        case 'openWorkspace':
        case 'openTerminal':
          this.status('That needs VS Code on a computer.', 'info');
          break;
      }
    } catch (err) {
      this.status((err as Error).message, 'error');
      if (m.type === 'send') this.post({ type: 'sendFailed' });
      if (m.type === 'configure') this.resendAgent();
    } finally {
      if (m.type === 'send') this.post({ type: 'sending', busy: false });
    }
  }

  private resendAgent(): void {
    this.lastAgentJson = '';
    this.onFleetChange();
  }

  private async configure(m: Extract<FromFrame, { type: 'configure' }>, agent: Agent, machine: MachineView): Promise<void> {
    if (!this.managedLive) throw new Error('Only sessions started by Vineyard can be changed from here.');
    if (m.permissionMode === 'bypassPermissions') {
      const ok = await confirm(`Let ${agentLabel(agent)} on ${machine.name} run every tool without asking?`, 'The session will no longer stop for permission prompts until you switch the mode back.', 'Bypass permissions', true);
      if (!ok) return this.resendAgent(); // snap the control back
    }
    const args: Record<string, unknown> = { sessionId: agent.sessionId };
    if (m.model !== undefined) args.model = m.model;
    if (m.effort !== undefined) args.effort = m.effort;
    if (m.permissionMode !== undefined) args.permissionMode = m.permissionMode;
    try {
      const res = await this.deps.api.request<{ defaults?: SettingsDefaults }>('configure', machine.id, args, 30_000);
      const what = m.model !== undefined ? `Model set to ${m.model || 'the default'}` : m.effort !== undefined ? `Effort set to ${m.effort || 'the default'}` : `Permission mode set to ${m.permissionMode}`;
      this.status(`${what}. Takes effect from the next request.`, 'ok');
      // The next session in this workspace starts with the same choice (see sessionPrefs.ts).
      if (!m.transient) await sessionPrefs.remember(machine.id, agent.workspacePath, { model: m.model, effort: m.effort, permissionMode: m.permissionMode }, res?.defaults);
    } finally {
      this.resendAgent(); // either way, show the daemon's truth
    }
  }

  /** "/" menu entries the frame did not handle itself. */
  private action(id: string, agent: Agent, machine: MachineView): void {
    switch (id) {
      case 'settings':
        this.deps.navigate(this.deps.settingsRoute());
        break;
      case 'history':
        this.deps.navigate(this.deps.historyRoute(machine.id, agent.workspacePath));
        break;
      case 'help':
        window.open(HELP_URL, '_blank', 'noopener');
        break;
      case 'report':
        window.open(ISSUES_URL, '_blank', 'noopener');
        break;
      default:
        this.status('That needs VS Code on a computer.', 'info');
    }
  }

  private addAttachments(files: PickedFile[]): void {
    const managed = this.managedLive;
    const skipped: string[] = [];
    for (const f of files) {
      const bytes = new Uint8Array(f.buffer);
      const problem = attachmentProblem(f.name, bytes, managed);
      if (problem) {
        skipped.push(`${f.name}: ${problem}`);
        continue;
      }
      this.attachments.push({ id: f.id ?? localId(), name: f.name, mediaType: attachmentMediaType(f.name), size: bytes.byteLength, data: base64(bytes) });
    }
    this.postAttachments();
    if (skipped.length) this.status(`Not attached. ${skipped.join('; ')}.`, 'error');
  }

  private postAttachments(): void {
    this.post({ type: 'attachments', items: this.attachments.map(({ id, name, mediaType, size }) => ({ id, name, mediaType, size })) });
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    window.removeEventListener('message', this.onWindowMessage);
    this.unsubscribe();
  }
}

export function base64(bytes: Uint8Array): string {
  let s = '';
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(s);
}
