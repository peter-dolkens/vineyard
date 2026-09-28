/**
 * What the web app's actions do, the same flows as the extension's commands (src/extension/
 * extension.ts, setup.ts) with the app's sheets and dialogs in place of quick picks and modals.
 */

import type { Agent } from '../core/model.ts';
import { agentLabel, basename, tildify } from '../core/format.ts';
import type { SettingsDefaults } from '../extension/sessionPrefs.ts';
import type { Api, ServerInfo } from './api.ts';
import type { FleetStore, MachineView } from './store.ts';
import { R } from './routes.ts';
import { sessionPrefs, settings } from './settings.ts';
import { actionSheet, confirm, copyBlock, dialog, h, prompt, toast } from './ui.ts';

/** How a session's state reads in a sentence ("… is running a tool"). */
const STATE_WORDS: Record<string, string> = {
  working: 'working', thinking: 'thinking', tool: 'running a tool', shell: 'in a shell', permission: 'waiting for permission', question: 'asking a question', idle: 'idle',
};

export interface SessionSummary {
  sessionId: string;
  cwd: string;
  mtime: number;
  title?: string;
  firstPrompt?: string;
  lastPrompt?: string;
  model?: string;
  gitBranch?: string;
  turns?: number;
}

export class Flows {
  constructor(
    private readonly api: Api,
    private readonly store: FleetStore,
    private readonly info: () => ServerInfo | undefined,
    private readonly navigate: (route: string) => void,
    /** The route on screen now, so a flow does not open what is already open. */
    private readonly currentRoute: () => string,
  ) {}

  /** Run a flow, turning its failure into an error banner. */
  run(what: Promise<unknown>): void {
    void what.catch((err: Error) => toast(err.message, 'error'));
  }

  /** A live session on an online machine that Vineyard does not drive: the Claude pane, a terminal. */
  canTakeOver(m: MachineView, a: Agent): boolean {
    return m.online && a.alive && a.kind !== 'subagent' && !(a.managed && !a.managed.exited);
  }

  /** Open a chat, taking an observed session over first when the setting says so. */
  openChat(m: MachineView, a: Agent): void {
    if (this.canTakeOver(m, a) && settings().observed === 'takeOver') {
      this.run(this.takeOver(m, a, true));
      return;
    }
    this.navigate(R.chat(a.id));
  }

  /**
   * End an observed session's process and resume it as a child of its daemon, carrying a pending
   * question over (see DESIGN.md, "Taking over a session"). Idle and question-waiting sessions lose
   * nothing; one mid-turn loses the running tool's output, so that asks first.
   */
  async takeOver(m: MachineView, a: Agent, auto = false): Promise<void> {
    if (!this.canTakeOver(m, a)) {
      this.navigate(R.chat(a.id));
      return;
    }
    if (a.state !== 'idle' && a.state !== 'question') {
      const what = a.state === 'permission' ? 'The tool waiting for permission is dropped; Claude continues without it.' : "The turn in progress is cut short: the running tool's output is lost and Claude continues without it.";
      const r = await dialog({
        title: `${agentLabel(a)} is ${STATE_WORDS[a.state] ?? a.state}. Take it over now?`,
        message: `${what} The pane or terminal it runs in on ${m.name} loses the session; the transcript is kept and continues here.`,
        buttons: [...(auto ? [{ label: 'Just watch', value: 'watch' }] : []), { label: 'Take over', value: 'take', destructive: true }],
      });
      if (r.button === 'watch') this.navigate(R.chat(a.id));
      if (r.button !== 'take') return;
    }
    toast(`Taking over ${agentLabel(a)}…`);
    const res = await this.api.request<{ sessionId: string }>('takeover', m.id, { sessionId: a.sessionId }, 30_000);
    await this.openWhenManaged(m, res.sessionId);
  }

  /** Wait for a just-started session to report as managed, then open its chat. */
  openWhenManaged(m: MachineView, sessionId: string): Promise<void> {
    const id = `${m.id}::${sessionId}`;
    return new Promise((resolve) => {
      const check = () => {
        const found = this.store.findAgent(id);
        if (!found?.agent.managed || found.agent.managed.exited) return false;
        off();
        clearTimeout(timer);
        if (this.currentRoute() !== R.chat(id)) this.navigate(R.chat(id));
        resolve();
        return true;
      };
      const off = this.store.onChange(() => void check());
      const timer = setTimeout(() => {
        off();
        toast('The agent started but has not reported yet; it will appear shortly.', 'info');
        resolve();
      }, 15_000);
      check();
    });
  }

  /**
   * Start (or resume) a managed session with the choices last made in that workspace, the default
   * permission mode from Settings when none was, and no first prompt; everything is adjustable in
   * the chat. A resumed session keeps its own settings.
   */
  async spawn(m: MachineView, cwd: string, resume?: string): Promise<void> {
    if (!m.online) throw new Error(`${m.name} is offline`);
    let remembered = {};
    if (!resume) {
      // The basis rides along so the daemon can drop a choice whose settings default has changed.
      const prefs = sessionPrefs.get(m.id, cwd);
      remembered = { ...prefs, permissionMode: prefs.permissionMode || settings().defaultPermissionMode || undefined };
    }
    toast(resume ? 'Resuming the session…' : `Starting an agent in ${basename(cwd)}…`);
    const res = await this.api.request<{ sessionId: string; defaults?: SettingsDefaults; stale?: string[] | null }>('spawn', m.id, { cwd, ...remembered, resume }, 30_000);
    if (!resume && res.defaults) await sessionPrefs.reconcile(m.id, cwd, res.stale ?? [], res.defaults);
    await this.openWhenManaged(m, res.sessionId);
  }

  /** New agent on a machine: in `cwd`, or pick one of its workspaces or type a path. */
  newAgent(m: MachineView, cwd?: string): void {
    if (cwd) return this.run(this.spawn(m, cwd));
    const known = [...m.entry.snapshot.workspaces].sort((a, b) => (b.lastActivityAt ?? 0) - (a.lastActivityAt ?? 0)).slice(0, 12);
    actionSheet(`New agent on ${m.name}`, [
      ...known.map((w) => ({ label: basename(w.path), detail: tildify(w.path, m.entry.snapshot.host.home), run: () => this.run(this.spawn(m, w.path)) })),
      {
        label: 'Other folder…',
        run: () =>
          this.run(
            (async () => {
              const home = m.entry.snapshot.host.home;
              const path = await prompt('Workspace folder', { message: `Absolute path on ${m.name}`, value: home ? home + '/' : '', placeholder: '/path/to/project', label: 'Start' });
              if (path?.trim()) await this.spawn(m, path.trim());
            })(),
          ),
      },
    ]);
  }

  /** Resume a past session picked from the history list. */
  async resumePast(m: MachineView, s: SessionSummary): Promise<void> {
    const live = m.entry.snapshot.agents.find((a) => a.alive && a.sessionId === s.sessionId);
    const title = s.title || s.firstPrompt || s.sessionId.slice(0, 8);
    if (live) {
      const r = await dialog({
        title: 'That session is still running',
        message: 'Resuming it in a second process would have two writers on one transcript.',
        buttons: [{ label: 'Resume anyway', value: 'resume', destructive: true }, { label: 'Open chat', value: 'open', primary: true }],
      });
      if (r.button === 'open') return this.openChat(m, live);
      if (r.button !== 'resume') return;
    } else if (!(await confirm(`Resume “${title.slice(0, 80)}”?`, `It continues on ${m.name} under Vineyard's control, in ${basename(s.cwd)}.`, 'Resume'))) return;
    await this.spawn(m, s.cwd, s.sessionId);
  }

  /** Resume an agent's session as a managed child (Resume Under Vineyard Control). */
  async resumeManaged(m: MachineView, a: Agent): Promise<void> {
    if (a.alive && !(await confirm(`${agentLabel(a)} is still running on ${m.name}`, 'Resuming it in a second process would have two writers on one transcript. Stop it there first, or continue anyway?', 'Continue anyway', true))) return;
    await this.spawn(m, a.workspacePath, a.sessionId);
  }

  async stop(m: MachineView, a: Agent): Promise<void> {
    const managed = !!a.managed && !a.managed.exited;
    const ok = await confirm(
      `${managed ? 'Stop' : 'Terminate'} ${agentLabel(a)} on ${m.name}?`,
      managed ? 'The session ends cleanly; you can resume it later.' : 'The Claude Code process is sent SIGTERM (killed after 5 s if it ignores it). Its transcript stays on disk and can be resumed.',
      managed ? 'Stop' : 'Terminate',
      true,
    );
    if (ok) await this.api.request(managed ? 'stop' : 'kill', m.id, { sessionId: a.sessionId }, 15_000);
  }

  async rename(m: MachineView, a: Agent): Promise<void> {
    const title = await prompt('Rename session', { message: `On ${m.name}`, value: a.title || a.name || '', label: 'Rename' });
    if (!title?.trim()) return;
    await this.api.request('rename', m.id, { sessionId: a.sessionId, title: title.trim(), path: a.transcriptPath || undefined, cwd: a.workspacePath }, 20_000);
    toast(`Renamed to “${title.trim()}”.`, 'ok');
  }

  /**
   * Sign a machine in to Claude: its daemon runs `claude auth login`, the sign-in page opens on this
   * phone (a link, so the tap opens it) and the code it shows comes back here.
   */
  login(m: MachineView): Promise<void> {
    if (!m.online) return Promise.reject(new Error(`${m.name} is offline`));
    return new Promise((resolve, reject) => {
      const go = (console: boolean) =>
        (async () => {
          const start = await this.api.request<{ id: string; url: string }>('login', m.id, { action: 'start', console }, 60_000);
          const body = h('div');
          const link = h('a', 'link-btn', 'Open the sign-in page');
          link.href = start.url;
          link.target = '_blank';
          link.rel = 'noopener';
          body.append(link);
          const r = await dialog({ title: `Sign in on ${m.name}`, message: 'Sign in on the page, then paste the code it shows you here.', body, input: { placeholder: 'authorization code' }, buttons: [{ label: 'Submit', value: 'ok', primary: true }] });
          if (r.button !== 'ok' || !r.text.trim()) {
            await this.api.request('login', m.id, { action: 'cancel', id: start.id }, 10_000).catch(() => undefined);
            return;
          }
          toast(`Signing in on ${m.name}…`);
          const res = await this.api.request<{ ok: boolean; message: string }>('login', m.id, { action: 'code', id: start.id, code: r.text.trim() }, 150_000);
          toast(`Claude on ${m.name}: ${res.message}`, 'ok');
          this.refresh();
        })().then(resolve, reject);
      actionSheet(
        `Sign in to Claude on ${m.name}`,
        [
          { label: 'Claude subscription', detail: 'claude.ai account (Pro / Max / Team)', run: () => go(false) },
          { label: 'Anthropic Console', detail: 'API usage billing', run: () => go(true) },
        ],
        undefined,
        resolve,
      );
    });
  }

  async wake(m: MachineView): Promise<void> {
    if (m.online) return toast(`${m.name} is already awake.`, 'ok');
    const res = await this.api.request<{ awake: boolean; hardwareAddresses?: number }>('wake', undefined, { machineId: m.id, addr: '' }, 10_000);
    if (res.awake) return;
    toast(`Waking ${m.name}${res.hardwareAddresses ? ' (Wake-on-LAN)' : ' (sleep-proxy nudge)'}…`);
    const online = await new Promise<boolean>((resolve) => {
      const done = (v: boolean) => {
        clearTimeout(t);
        off();
        resolve(v);
      };
      const off = this.store.onChange(() => this.store.machine(m.id)?.online && done(true));
      const t = setTimeout(() => done(false), 45_000);
    });
    if (online) toast(`${m.name} is awake.`, 'ok');
    else toast(`${m.name} did not wake within 45 s. It may be off, on another network, or not set to wake for network access.`, 'error');
  }

  async remove(m: MachineView): Promise<void> {
    if (m.local) return toast('This machine is always shown. Uninstall its daemon with "vineyardd uninstall".', 'info');
    if (!(await confirm(`Remove ${m.name} from the fleet?`, 'Every member drops it and refuses its connections. It can rejoin with an invite code.', 'Remove', true))) return;
    await this.api.request('removepeer', undefined, { machineId: m.id, addr: '' }, 10_000);
    toast(`${m.name} removed.`, 'ok');
    this.navigate(R.home);
  }

  async invite(): Promise<void> {
    const res = await this.api.request<{ code: string; expiresInSeconds: number }>('invite', undefined, undefined, 10_000);
    const mins = Math.round(res.expiresInSeconds / 60);
    await dialog({
      title: 'Invite code',
      message: `Single use, valid ${mins} minutes. On the new machine run Vineyard: Join Fleet with Invite Code in VS Code, or vineyardd join <code>.`,
      body: copyBlock(res.code),
      buttons: [{ label: 'Done', value: 'cancel' }],
    });
  }

  async restartDaemon(m: MachineView): Promise<void> {
    if (!(await confirm(`Restart the daemon on ${m.name}?`, 'Sessions started by Vineyard on this machine end with it (they can be resumed). The app reconnects on its own.', 'Restart', true))) return;
    await this.api.restartDaemon();
    toast('Daemon restarting…', 'ok');
  }

  /** Ask every online machine to re-probe now: one tiny request each. */
  refresh(): void {
    for (const m of this.store.allMachines()) if (m.online) void this.api.request('probe', m.id, undefined, 5000).catch(() => undefined);
  }

  localName(): string {
    return this.info()?.name ?? 'this machine';
  }
}
