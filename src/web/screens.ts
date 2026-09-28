/**
 * The web app's screens: the extension's tree as a stack of menus (machines → workspaces → agents →
 * details), plus the chat, past sessions, the daemon log and settings. Each screen re-renders from
 * the store while it is on top; the rows it lists are patched in place (ui.ts).
 */

import type { Agent, Usage } from '../core/model.ts';
import { isBusy, needsAttention } from '../core/model.ts';
import { STATE_LABEL, agentLabel, basename, describeVia, duration, relativeTime, shortModel, subagentLabel, tildify, tokens } from '../core/format.ts';
import { subagentActive, subagentId } from '../core/subagents.ts';
import { clock, taskActive, taskElapsed, taskLabel, taskStateLabel } from '../core/tasks.ts';
import { resetsIn, usageRows, usageWarning, WARN_PERCENT } from '../core/usage.ts';
import type { Api, PairedDevice, ServerInfo } from './api.ts';
import { privateConnection } from './pair.ts';
import { ChatHost } from './chatHost.ts';
import type { Flows, SessionSummary } from './flows.ts';
import { R, parentRoute, parseRoute, type Route } from './routes.ts';
import { DEFAULTS, settings, updateSettings, type Settings } from './settings.ts';
import { agentsFor, countByState, dominantState, machinesFor, subagentRows, tasksFor, workspacesFor, type FleetStore, type MachineView } from './store.ts';
import { STATE_ICON, STATE_TINT, actionSheet, confirm, copyBlock, copyText, dialog, h, icon, renderSections, toast, type Row, type Section } from './ui.ts';

export interface Ctx {
  api: Api;
  store: FleetStore;
  flows: Flows;
  info(): ServerInfo | undefined;
  navigate(route: string): void;
  back(): void;
}

interface NavAction {
  icon?: string;
  text?: string;
  label: string;
  onTap: () => void;
}

export abstract class Screen {
  readonly el = h('div', 'screen');
  protected readonly nav = h('header', 'nav');
  protected readonly content = h('div', 'content');
  protected readonly body = h('div', 'sections');
  private readonly backBtn = h('button', 'nav-back');
  private readonly backLabel = h('span', 'nav-back-label');
  protected readonly titleEl = h('div', 'nav-title');
  private readonly actionsEl = h('div', 'nav-actions');
  private actionsSig = '';

  constructor(
    readonly route: string,
    protected readonly ctx: Ctx,
  ) {
    this.backBtn.append(icon('chevron-left'), this.backLabel);
    this.backBtn.onclick = () => ctx.back();
    this.nav.append(this.backBtn, this.titleEl, this.actionsEl);
    this.content.append(this.body);
    this.el.append(this.nav, this.content, h('div', 'edge-shadow'));
    this.content.addEventListener('scroll', () => this.onScroll(), { passive: true });
  }

  abstract title(): string;
  abstract render(): void;

  parent(): string {
    return parentRoute(parseRoute(this.route), (id) => this.ctx.store.findAgent(id)?.agent.workspacePath);
  }

  /** Called after the screen is in the page. */
  mount(): void {}
  destroy(): void {}

  protected onScroll(): void {
    this.nav.classList.toggle('scrolled', this.content.scrollTop > 2);
  }

  setBack(label: string | undefined): void {
    this.backBtn.hidden = label === undefined;
    this.backLabel.textContent = label && label.length > 14 ? 'Back' : label ?? '';
  }

  protected setTitle(t: string): void {
    if (this.titleEl.textContent !== t) this.titleEl.textContent = t;
  }

  protected setActions(actions: NavAction[]): void {
    const sig = actions.map((a) => a.icon ?? a.text).join('|');
    if (sig === this.actionsSig) {
      [...this.actionsEl.children].forEach((b, i) => ((b as HTMLButtonElement).onclick = actions[i]!.onTap));
      return;
    }
    this.actionsSig = sig;
    this.actionsEl.replaceChildren(
      ...actions.map((a) => {
        const b = h('button', 'nav-btn' + (a.text ? ' text' : ''));
        if (a.icon) b.append(icon(a.icon));
        else b.textContent = a.text ?? '';
        b.setAttribute('aria-label', a.label);
        b.onclick = a.onTap;
        return b;
      }),
    );
  }

  protected machineOr(id: string, render: (m: MachineView) => void): void {
    const m = this.ctx.store.machine(id);
    if (m) return render(m);
    this.setTitle('Machine');
    renderSections(this.body, [{ key: 'gone', block: this.ctx.store.loaded ? messageCard('Not in the fleet', `No machine called ${id} is known here.`) : spinner() }]);
  }
}

// ---- shared pieces ------------------------------------------------------------------------------

function spinner(): HTMLElement {
  return h('div', 'spinner');
}

function messageCard(title: string, text?: string, kind: '' | 'warn' | 'error' = '', action?: { label: string; onTap: () => void }): HTMLElement {
  return fillCard(h('div'), title, text, kind, action);
}

function fillCard(c: HTMLElement, title: string, text?: string, kind: '' | 'warn' | 'error' = '', action?: { label: string; onTap: () => void }): HTMLElement {
  c.className = `card ${kind}`;
  c.append(h('div', 'card-title', title));
  if (text) c.append(h('div', 'card-text', text));
  if (action) {
    const row = h('div', 'card-actions');
    const b = h('button', 'pill-btn', action.label);
    b.onclick = action.onTap;
    row.append(b);
    c.append(row);
  }
  return c;
}

/** One persistent block per screen whose contents are rebuilt each render. */
class Block {
  readonly el: HTMLElement;
  private sig = '';
  constructor(cls: string) {
    this.el = h('div', cls);
  }
  /** Rebuild only when the signature changes, so taps inside survive idle renders. */
  set(sig: string, build: (el: HTMLElement) => void): HTMLElement {
    if (sig !== this.sig) {
      this.sig = sig;
      this.el.replaceChildren();
      build(this.el);
    }
    return this.el;
  }
}

function machineIcon(m: MachineView): string {
  return m.local ? 'device-desktop' : 'server';
}

function machineTint(m: MachineView): string {
  if (!m.online) return 'var(--gray)';
  const live = m.entry.snapshot.agents.filter((a) => a.alive);
  if (live.some((a) => needsAttention(a.state))) return 'var(--orange)';
  if (live.some((a) => isBusy(a.state))) return 'var(--blue)';
  return 'var(--green)';
}

function machineSub(m: MachineView): string {
  const snap = m.entry.snapshot;
  if (!m.online) {
    const seen = m.entry.lastSeen || snap.at;
    return seen ? `Offline · last seen ${relativeTime(seen)}` : m.peer?.lastError ? 'Unreachable' : 'Never seen';
  }
  const live = snap.agents.filter((a) => a.alive);
  let s = !snap.hasClaude ? 'No Claude Code' : live.length ? countByState(live) : 'No agents';
  const limit = usageWarning(snap.usage);
  if (limit) s += ` · ${limit.rejected ? 'limit hit' : `${limit.row.percent}% of ${limit.row.label.toLowerCase()}`}`;
  return s;
}

function agentSub(a: Agent): string {
  const bits: string[] = [STATE_LABEL[a.state]];
  if (a.managed && !a.managed.exited) bits.push('managed');
  const model = shortModel(a.model);
  if (model) bits.push(a.effort ? `${model} · ${a.effort}` : model);
  const subs = (a.subagents ?? []).filter(subagentActive).length;
  if (subs) bits.push(`${subs} subagent${subs === 1 ? '' : 's'}`);
  const tasks = (a.tasks ?? []).filter(taskActive).length;
  if (tasks) bits.push(`${tasks} task${tasks === 1 ? '' : 's'}`);
  if (a.lastActivityAt) bits.push(relativeTime(a.lastActivityAt));
  return bits.join(' · ');
}

/** What an agent is doing or waiting on, for the row's third line. */
function agentNote(a: Agent): string | undefined {
  if (!a.alive) return undefined;
  const q = a.pendingTools.find((t) => t.name === 'AskUserQuestion');
  if (a.state === 'question' && q?.summary) return q.summary;
  if (a.managed?.pending?.description) return a.managed.pending.description;
  return a.stateDetail || undefined;
}

function agentRow(ctx: Ctx, m: MachineView, a: Agent, opts: { where?: boolean; info?: boolean } = {}): Row {
  return {
    key: a.id,
    icon: STATE_ICON[a.state],
    spin: a.state === 'working',
    tint: STATE_TINT[a.state],
    title: agentLabel(a),
    sub: opts.where ? `${m.name} · ${basename(a.workspacePath)}` : agentSub(a),
    note: opts.where ? [STATE_LABEL[a.state], agentNote(a)].filter(Boolean).join(' — ') : agentNote(a),
    onTap: () => ctx.flows.openChat(m, a),
    onInfo: opts.info === false ? undefined : () => ctx.navigate(R.agent(a.id)),
  };
}

function usageBlock(u: Usage | undefined): HTMLElement | undefined {
  const rows = usageRows(u);
  if (!rows.length) return undefined;
  const el = h('div', 'usage');
  for (const r of rows) {
    const level = r.percent >= 100 ? 'hit' : r.percent >= WARN_PERCENT ? 'warn' : '';
    const item = h('div');
    const head = h('div', 'u-head');
    head.append(h('span', undefined, r.label), h('span', `u-pct ${level}`, `${r.percent}%`));
    const bar = h('div', 'u-bar');
    const fill = h('div', `u-fill ${level}`);
    fill.style.width = `${Math.min(100, r.percent)}%`;
    bar.append(fill);
    item.append(head, bar);
    if (r.resetsAt) item.append(h('div', 'u-reset', `Resets in ${resetsIn(r.resetsAt)}`));
    el.append(item);
  }
  return el;
}

function detail(key: string, title: string, value: string | undefined, copy = false): Row | undefined {
  if (!value) return undefined;
  return {
    key,
    title,
    value,
    onTap: copy
      ? () => {
          if (copyText(value)) toast(`${title} copied.`, 'ok');
        }
      : undefined,
    chevron: false,
  };
}

function compact<T>(xs: (T | undefined | false | 0 | '')[]): T[] {
  return xs.filter(Boolean) as T[];
}

// ---- home ---------------------------------------------------------------------------------------

export class HomeScreen extends Screen {
  private readonly header = h('div');
  private readonly large = h('div', 'large-title', 'Vineyard');
  private readonly sub = h('div', 'large-sub');
  private readonly status = new Block('card');

  constructor(route: string, ctx: Ctx) {
    super(route, ctx);
    this.header.append(this.large, this.sub);
    this.content.prepend(this.header);
    this.titleEl.classList.add('faded');
    this.setTitle('Vineyard');
  }

  title(): string {
    return 'Vineyard';
  }

  protected override onScroll(): void {
    super.onScroll();
    this.titleEl.classList.toggle('faded', this.content.scrollTop < 36);
  }

  render(): void {
    const { store, flows } = this.ctx;
    const p = settings();
    this.setActions([
      { icon: 'refresh', label: 'Refresh', onTap: () => (flows.refresh(), toast('Asked every machine for a fresh look.')) },
      {
        icon: 'ellipsis',
        label: 'More',
        onTap: () =>
          actionSheet(undefined, [
            { label: 'New agent…', run: () => this.pickMachine('New agent on…', (m) => flows.newAgent(m)) },
            { label: 'Resume a past session…', run: () => this.pickMachine('Past sessions on…', (m) => this.ctx.navigate(R.history(m.id))) },
            { label: 'Settings', run: () => this.ctx.navigate(R.settings) },
          ]),
      },
    ]);
    const s = store.summary();
    const bits = [`${s.machinesOnline}/${s.machinesTotal} online`];
    if (s.attention) bits.push(`${s.attention} need${s.attention === 1 ? 's' : ''} you`);
    if (s.busy) bits.push(`${s.busy} working`);
    if (s.idle) bits.push(`${s.idle} idle`);
    this.sub.textContent = store.loaded ? bits.join(' · ') : '';

    const sections: Section[] = [];
    const serverName = this.ctx.info()?.name ?? 'this machine';
    if (store.state !== 'connected') {
      const [title, text, kind] =
        store.state === 'offline'
          ? ['Cannot reach the web server', `vineyardd web on ${serverName} is not answering. It retries on its own.`, 'error']
          : store.state === 'disconnected'
            ? [`Cannot reach the daemon on ${serverName}`, store.error ?? 'Is vineyardd running there?', 'error']
            : ['Connecting…', `Attaching to the daemon on ${serverName}.`, ''];
      const retry = store.state === 'connecting' ? undefined : { label: 'Retry', onTap: () => this.ctx.api.reconnect() };
      sections.push({ key: 'status', block: this.status.set(`${title}|${text}`, (el) => fillCard(el, title, text, kind as '' | 'error', retry)) });
    }
    const waiting = store.attention();
    if (waiting.length) sections.push({ key: 'attention', header: 'Needs you', rows: waiting.map(({ agent, machine }) => agentRow(this.ctx, machine, agent, { where: true })) });

    const machines = machinesFor(store.allMachines(), p);
    if (store.loaded || machines.length) {
      sections.push({
        key: 'machines',
        header: 'Machines',
        footer: `Vineyard web ${this.ctx.info()?.version ?? ''} on ${serverName}`,
        rows: machines.map((m) => ({
          key: m.id,
          icon: machineIcon(m),
          tint: machineTint(m),
          title: m.local ? `${m.name} (this machine)` : m.name,
          sub: machineSub(m),
          onTap: () => this.ctx.navigate(R.machine(m.id)),
        })),
      });
    } else if (store.state === 'connected') sections.push({ key: 'loading', block: spinner() });
    renderSections(this.body, sections);
  }

  private pickMachine(title: string, then: (m: MachineView) => void): void {
    const online = machinesFor(this.ctx.store.allMachines(), settings()).filter((m) => m.online);
    if (!online.length) return toast('No machine is online.', 'error');
    if (online.length === 1) return then(online[0]!);
    actionSheet(title, online.map((m) => ({ label: m.name, detail: machineSub(m), run: () => then(m) })));
  }
}

// ---- machine ------------------------------------------------------------------------------------

export class MachineScreen extends Screen {
  private readonly hero = new Block('card');
  private readonly usage = new Block('usage-wrap');

  constructor(
    route: string,
    ctx: Ctx,
    private readonly id: string,
  ) {
    super(route, ctx);
  }

  title(): string {
    return this.ctx.store.machine(this.id)?.name ?? 'Machine';
  }

  render(): void {
    this.machineOr(this.id, (m) => this.renderMachine(m));
  }

  private renderMachine(m: MachineView): void {
    const { flows, navigate } = this.ctx;
    const snap = m.entry.snapshot;
    const p = settings();
    this.setTitle(m.name);
    this.setActions(m.online ? [{ icon: 'add', label: 'New agent', onTap: () => flows.newAgent(m) }] : []);
    const nameOf = (id: string) => this.ctx.store.machine(id)?.name ?? id;

    const status = m.online ? (m.local ? 'Online' : `Online · ${describeVia(m.entry.via, nameOf)}`) : machineSub(m);
    const heroEl = this.hero.set(`${m.name}|${status}|${machineTint(m)}|${m.local}`, (el) => {
      const row = h('div', 'hero');
      const ic = h('span', 'ic');
      ic.style.setProperty('--tint', machineTint(m));
      ic.append(icon(machineIcon(m)));
      const txt = h('div', 'hero-text');
      txt.append(h('div', 'hero-title', m.local ? `${m.name} (this machine)` : m.name), h('div', 'hero-sub', status));
      row.append(ic, txt);
      el.append(row);
    });
    const problem = m.peer?.lastError && (!m.online || m.entry.via?.startsWith('relay:')) ? m.peer.lastError : undefined;

    const sections: Section[] = [{ key: 'hero', block: heroEl }];
    if (!m.online && !snap.at) sections.push({ key: 'never', block: messageCard('Never reported', problem ?? 'This machine is in the fleet but has not answered yet.', 'warn') });

    const ws = workspacesFor(m, p);
    sections.push({
      key: 'workspaces',
      header: 'Workspaces',
      footer: !ws.length ? (m.online ? 'No workspaces with agents or transcripts.' : 'Workspaces show once the machine reports.') : undefined,
      rows: ws.map((w) => {
        const agents = p.showExited ? w.agents : w.agents.filter((a) => a.alive);
        const dom = dominantState(agents);
        return {
          key: w.id,
          icon: dom ? STATE_ICON[dom] : w.openInIde ? 'folder-active' : 'folder',
          spin: dom === 'working',
          tint: dom ? STATE_TINT[dom] : 'var(--gray)',
          title: basename(w.path),
          sub: dom ? countByState(agents) : w.lastActivityAt ? `Last active ${relativeTime(w.lastActivityAt)}` : tildify(w.path, snap.host.home),
          onTap: () => navigate(R.workspace(m.id, w.path)),
        };
      }),
    });

    const u = usageBlock(snap.usage);
    if (u) sections.push({ key: 'usage', header: 'Usage', footer: snap.usage?.at ? `As of ${relativeTime(snap.usage.at)}, from a session started by Vineyard.` : undefined, block: this.usage.set(JSON.stringify(snap.usage), (el) => el.append(u)) });

    const info = this.ctx.info();
    sections.push({
      key: 'actions',
      header: 'Actions',
      rows: compact<Row>([
        m.online && { key: 'new', title: 'New agent…', action: true, onTap: () => flows.newAgent(m) },
        m.online && { key: 'history', title: 'Resume a past session…', action: true, onTap: () => navigate(R.history(m.id)) },
        m.online && { key: 'login', title: 'Sign in to Claude…', action: true, onTap: () => flows.run(flows.login(m)) },
        !m.online && { key: 'wake', title: 'Wake machine', action: true, onTap: () => flows.run(flows.wake(m)) },
        m.local && info?.log && { key: 'log', title: 'Daemon log', onTap: () => navigate(R.log(m.id)) },
        m.local && info?.restart && { key: 'restart', title: 'Restart daemon', destructive: true, onTap: () => flows.run(flows.restartDaemon(m)) },
        !m.local && { key: 'remove', title: 'Remove from fleet', destructive: true, onTap: () => flows.run(flows.remove(m)) },
      ]),
    });

    sections.push({
      key: 'details',
      header: 'Details',
      footer: problem,
      rows: compact<Row>([
        detail('addr', 'Address', snap.listen || m.peer?.addr, true),
        detail('sys', 'System', snap.host.os ? `${snap.host.os}/${snap.host.arch ?? ''}` : undefined),
        detail('ver', 'vineyardd', snap.daemonVersion),
        detail('host', 'Hostname', snap.host.hostname),
        detail('seen', 'Last seen', m.entry.lastSeen ? relativeTime(m.entry.lastSeen) : undefined),
        detail('snap', 'Snapshot', snap.at ? relativeTime(snap.at) : undefined),
        detail('uplink', 'Uplink', snap.uplink ? `to ${nameOf(snap.uplink)}` : undefined),
        detail('web', 'Web app', snap.webApp?.error ? `could not start` : snap.webApp?.urls?.[0], true),
        detail('id', 'Machine id', m.id, true),
      ]),
    });
    renderSections(this.body, sections);
  }
}

// ---- workspace ----------------------------------------------------------------------------------

export class WorkspaceScreen extends Screen {
  constructor(
    route: string,
    ctx: Ctx,
    private readonly machineId: string,
    private readonly path: string,
  ) {
    super(route, ctx);
  }

  title(): string {
    return basename(this.path);
  }

  render(): void {
    this.machineOr(this.machineId, (m) => this.renderWorkspace(m));
  }

  private renderWorkspace(m: MachineView): void {
    const { flows, navigate } = this.ctx;
    const p = settings();
    this.setTitle(basename(this.path));
    this.setActions(m.online ? [{ icon: 'add', label: 'New agent here', onTap: () => flows.newAgent(m, this.path) }] : []);
    const w = this.ctx.store.workspaceOf(m, this.path);
    const home = m.entry.snapshot.host.home;
    const sections: Section[] = [];
    const agents = w ? agentsFor(w, p) : [];
    sections.push({
      key: 'agents',
      header: 'Agents',
      footer: !agents.length ? (w?.historyCount ? `No live agents. ${w.historyCount} past session${w.historyCount === 1 ? '' : 's'} on disk.` : 'No agents here.') : undefined,
      rows: agents.map((a) => agentRow(this.ctx, m, a)),
    });
    sections.push({
      key: 'actions',
      rows: compact<Row>([
        m.online && { key: 'new', title: 'New agent here', action: true, onTap: () => flows.newAgent(m, this.path) },
        m.online && { key: 'history', title: 'Resume a past session…', action: true, onTap: () => navigate(R.history(m.id, this.path)) },
      ]),
    });
    sections.push({
      key: 'details',
      header: 'Workspace',
      footer: w?.openInIde ? `Open in VS Code on ${m.name}.` : undefined,
      rows: compact<Row>([detail('path', 'Path', tildify(this.path, home), true), detail('machine', 'Machine', m.name), w && detail('history', 'Transcripts', String(w.historyCount)), w?.lastActivityAt && detail('last', 'Last activity', relativeTime(w.lastActivityAt))]),
    });
    renderSections(this.body, sections);
  }
}

// ---- agent details ------------------------------------------------------------------------------

export class AgentScreen extends Screen {
  private readonly hero = new Block('card');
  private readonly prompt = new Block('card');
  private stopping = new Set<string>();

  constructor(
    route: string,
    ctx: Ctx,
    private readonly agentId: string,
  ) {
    super(route, ctx);
  }

  title(): string {
    const a = this.ctx.store.findAgent(this.agentId)?.agent;
    return a ? agentLabel(a) : 'Agent';
  }

  render(): void {
    const found = this.ctx.store.findAgent(this.agentId);
    if (!found) {
      this.setTitle('Agent');
      renderSections(this.body, [{ key: 'gone', block: this.ctx.store.loaded ? messageCard('Session not found', 'It is no longer in the fleet.') : spinner() }]);
      return;
    }
    const { agent: a, machine: m, parent } = found;
    const { flows, navigate } = this.ctx;
    const p = settings();
    const sub = a.kind === 'subagent';
    const managedLive = !!a.managed && !a.managed.exited;
    this.setTitle(agentLabel(a));
    this.setActions([{ icon: 'comment-discussion', label: 'Open chat', onTap: () => flows.openChat(m, a) }]);

    const state = a.alive ? a.state : 'exited';
    const heroEl = this.hero.set(`${agentLabel(a)}|${state}|${a.stateDetail}`, (el) => {
      const row = h('div', 'hero');
      const ic = h('span', 'ic');
      ic.style.setProperty('--tint', STATE_TINT[state]);
      ic.append(icon(STATE_ICON[state], state === 'working' ? 'codicon-modifier-spin' : ''));
      const txt = h('div', 'hero-text');
      txt.append(h('div', 'hero-title', agentLabel(a)), h('div', 'hero-sub', [STATE_LABEL[state], a.stateDetail].filter(Boolean).join(' — ')));
      row.append(ic, txt);
      el.append(row);
    });
    const sections: Section[] = [{ key: 'hero', block: heroEl }];

    sections.push({
      key: 'actions',
      rows: compact<Row>([
        { key: 'chat', title: 'Open chat', action: true, onTap: () => flows.openChat(m, a) },
        sub && parent && { key: 'parent', title: `Parent session: ${agentLabel(parent)}`, onTap: () => navigate(R.agent(parent.id)) },
        !sub && flows.canTakeOver(m, a) && { key: 'takeover', title: 'Take over session', action: true, onTap: () => flows.run(flows.takeOver(m, a)) },
        !sub && !managedLive && m.online && { key: 'resume', title: 'Resume under Vineyard control…', action: true, onTap: () => flows.run(flows.resumeManaged(m, a)) },
        !sub && m.online && { key: 'rename', title: 'Rename…', action: true, onTap: () => flows.run(flows.rename(m, a)) },
        !sub && m.online && { key: 'history', title: 'Past sessions in this workspace', onTap: () => navigate(R.history(m.id, a.workspacePath)) },
        !sub && a.alive && m.online && { key: 'stop', title: managedLive ? 'Stop session' : 'Terminate session', destructive: true, onTap: () => flows.run(flows.stop(m, a)) },
      ]),
    });

    if (a.lastPrompt && !sub) {
      sections.push({
        key: 'prompt',
        header: 'Last prompt',
        block: this.prompt.set(a.lastPrompt, (el) => {
          el.append(h('div', 'quote', a.lastPrompt!.slice(0, 600)));
        }),
      });
    }
    if (a.pendingTools.length) {
      sections.push({ key: 'pending', header: 'Waiting on', rows: a.pendingTools.map((t) => ({ key: t.id, icon: 'tools', tint: 'var(--blue)', title: t.name, sub: t.summary })) });
    }

    // The parent session: its subagent tree and background commands, as the tree shows under it.
    const session = sub ? parent : a;
    if (session && !sub) {
      const subs = subagentRows(session, p);
      if (subs.length) {
        sections.push({
          key: 'subagents',
          header: 'Subagents',
          rows: subs.map(({ sub: s, depth }) => {
            const running = managedLive && subagentActive(s);
            const id = subagentId(session, s);
            return {
              key: s.agentId,
              indent: depth,
              icon: STATE_ICON[s.state],
              spin: s.state === 'working',
              tint: STATE_TINT[s.state],
              title: subagentLabel(s),
              sub: compact<string>([STATE_LABEL[s.state], s.type, shortModel(s.model), s.background && 'background', s.lastActivityAt && relativeTime(s.lastActivityAt)]).join(' · '),
              onTap: () => navigate(R.chat(id)),
              button: running ? { label: this.stopping.has(s.agentId) ? 'Stopping…' : 'Stop', destructive: true, busy: this.stopping.has(s.agentId), onTap: () => this.stopTask(m, a, s.agentId) } : undefined,
              onInfo: running ? undefined : () => navigate(R.agent(id)),
            };
          }),
        });
      }
      const tasks = tasksFor(session, p);
      if (tasks.length) {
        sections.push({
          key: 'tasks',
          header: 'Background tasks',
          rows: tasks.map((t) => {
            const running = taskActive(t);
            const elapsed = taskElapsed(t);
            return {
              key: t.toolUseId,
              icon: running ? 'terminal' : t.state === 'completed' ? 'pass' : 'error',
              tint: running ? 'var(--blue)' : t.state === 'completed' ? 'var(--green)' : 'var(--red)',
              title: taskLabel(t),
              sub: compact<string>([t.kind || 'shell', taskStateLabel(t).toLowerCase(), elapsed && clock(elapsed)]).join(' · '),
              button: running && managedLive && t.taskId ? { label: this.stopping.has(t.taskId) ? 'Stopping…' : 'Stop', destructive: true, busy: this.stopping.has(t.taskId), onTap: () => this.stopTask(m, a, t.taskId!) } : undefined,
            };
          }),
        });
      }
    }

    const model = a.model || a.managed?.model;
    sections.push({
      key: 'details',
      header: 'Details',
      rows: compact<Row>([
        detail('machine', 'Machine', m.name),
        detail('ws', 'Workspace', tildify(a.workspacePath, m.entry.snapshot.host.home), true),
        detail('model', 'Model', model ? shortModel(model) : undefined),
        detail('effort', 'Effort', a.effort),
        detail('mode', 'Mode', a.permissionMode),
        detail('ctx', 'Context', a.contextTokens ? `${tokens(a.contextTokens)} tokens` : undefined),
        detail('branch', 'Branch', a.gitBranch),
        detail('managed', 'Managed', a.managed ? `${a.managed.turns} turn${a.managed.turns === 1 ? '' : 's'}${a.managed.costUsd ? ` · $${a.managed.costUsd.toFixed(2)}` : ''}${a.managed.exited ? ' · ended' : ''}` : undefined),
        detail('session', 'Session', a.sessionId, true),
        detail('pid', 'PID', a.pid ? String(a.pid) : undefined),
        detail('client', 'Client', [a.entrypoint, a.version].filter(Boolean).join(' ')),
        detail('up', 'Uptime', a.startedAt && a.alive ? duration(Date.now() - a.startedAt) : undefined),
        detail('last', 'Last activity', a.lastActivityAt ? relativeTime(a.lastActivityAt) : undefined),
      ]),
    });
    renderSections(this.body, sections);
  }

  private stopTask(m: MachineView, a: Agent, taskId: string): void {
    this.stopping.add(taskId);
    this.render();
    this.ctx.api
      .request('stoptask', m.id, { sessionId: a.sessionId, taskId }, 25_000)
      .catch((err: Error) => toast(`Could not stop ${taskId}: ${err.message}`, 'error'))
      .finally(() => {
        this.stopping.delete(taskId);
        this.render();
      });
  }
}

// ---- past sessions ------------------------------------------------------------------------------

export class HistoryScreen extends Screen {
  private sessions: SessionSummary[] | undefined;
  private error: string | undefined;
  private query = '';
  private readonly search = h('input', 'search');
  private readonly searchWrap = h('div');

  constructor(
    route: string,
    ctx: Ctx,
    private readonly machineId: string,
    private readonly cwd: string | undefined,
  ) {
    super(route, ctx);
    this.search.type = 'search';
    this.search.placeholder = 'Search titles, prompts, branches';
    this.search.oninput = () => {
      this.query = this.search.value.trim().toLowerCase();
      this.render();
    };
    this.searchWrap.append(this.search);
    this.load();
  }

  title(): string {
    return 'Past sessions';
  }

  private load(): void {
    this.sessions = undefined;
    this.error = undefined;
    this.ctx.api
      .request<{ sessions: SessionSummary[] }>('sessions', this.machineId, { cwd: this.cwd, limit: 80 }, 30_000)
      .then((r) => (this.sessions = r.sessions ?? []))
      .catch((err: Error) => (this.error = err.message))
      .finally(() => this.render());
  }

  render(): void {
    this.machineOr(this.machineId, (m) => this.renderList(m));
  }

  private renderList(m: MachineView): void {
    this.setTitle(this.cwd ? `Past · ${basename(this.cwd)}` : `Past · ${m.name}`);
    this.setActions([{ icon: 'refresh', label: 'Reload', onTap: () => this.load() }]);
    const sections: Section[] = [{ key: 'search', block: this.searchWrap }];
    if (this.error) sections.push({ key: 'err', block: messageCard('Could not list sessions', this.error, 'error', { label: 'Retry', onTap: () => this.load() }) });
    else if (!this.sessions) sections.push({ key: 'loading', block: spinner() });
    else {
      const live = new Set(m.entry.snapshot.agents.filter((a) => a.alive).map((a) => a.sessionId));
      const q = this.query;
      const list = this.sessions.filter((s) => !q || [s.title, s.firstPrompt, s.lastPrompt, s.gitBranch, s.model, s.cwd, s.sessionId].some((x) => x?.toLowerCase().includes(q)));
      const home = m.entry.snapshot.host.home;
      sections.push({
        key: 'list',
        footer: !list.length ? (q ? 'Nothing matches.' : `No past sessions${this.cwd ? ` in ${basename(this.cwd)}` : ''} on ${m.name}.`) : 'Newest first. Tap one to resume it under Vineyard’s control.',
        rows: list.map((s) => ({
          key: s.sessionId,
          icon: live.has(s.sessionId) ? 'circle-large-filled' : 'history',
          tint: live.has(s.sessionId) ? 'var(--green)' : 'var(--gray)',
          title: s.title || s.firstPrompt || s.sessionId.slice(0, 8),
          sub: compact<string>([relativeTime(s.mtime), s.model && shortModel(s.model), s.gitBranch, s.turns && `${s.turns} turn${s.turns === 1 ? '' : 's'}`]).join(' · '),
          note: `${this.cwd ? '' : tildify(s.cwd, home) + ' · '}${s.lastPrompt && s.lastPrompt !== s.firstPrompt ? s.lastPrompt : s.sessionId}`,
          onTap: () => this.ctx.flows.run(this.ctx.flows.resumePast(m, s)),
        })),
      });
    }
    renderSections(this.body, sections);
  }
}

// ---- daemon log ---------------------------------------------------------------------------------

export class LogScreen extends Screen {
  private readonly pre = h('pre', 'log-text');
  private loaded = false;

  constructor(route: string, ctx: Ctx) {
    super(route, ctx);
    this.load();
  }

  title(): string {
    return 'Daemon log';
  }

  private load(): void {
    this.ctx.api
      .log(400)
      .then((t) => {
        this.pre.textContent = t;
        this.loaded = true;
        this.render();
        requestAnimationFrame(() => (this.content.scrollTop = this.content.scrollHeight));
      })
      .catch((err: Error) => {
        this.pre.textContent = err.message;
        this.loaded = true;
        this.render();
      });
  }

  render(): void {
    this.setTitle('Daemon log');
    this.setActions([{ icon: 'refresh', label: 'Reload', onTap: () => this.load() }]);
    renderSections(this.body, [{ key: 'log', footer: '~/.vineyard/vineyardd.log, last 400 lines', block: this.loaded ? this.pre : spinner() }]);
  }
}

// ---- settings -----------------------------------------------------------------------------------

type Choice<K extends keyof Settings> = { value: Settings[K]; label: string; detail?: string };

export class SettingsScreen extends Screen {
  private devices: PairedDevice[] | undefined;
  private devicesError: string | undefined;
  private devicesAt = 0;

  constructor(route: string, ctx: Ctx) {
    super(route, ctx);
    this.loadDevices();
  }

  title(): string {
    return 'Settings';
  }

  private loadDevices(): void {
    this.devicesAt = Date.now();
    this.ctx.api
      .devices()
      .then((d) => ((this.devices = d), (this.devicesError = undefined)))
      .catch((err: Error) => (this.devicesError = err.message))
      .finally(() => this.render());
  }

  private async pairAnother(): Promise<void> {
    const p = await this.ctx.api.pairNew();
    const shown = `${p.code.slice(0, 4)}-${p.code.slice(4)}`;
    const body = h('div');
    body.append(h('div', 'pair-code-show', shown), copyBlock(p.links[0] ?? ''));
    await dialog({
      title: 'Pair another device',
      message: `On the new device open ${p.links[0]?.split('#')[0] ?? 'this address'} and enter the code, or send it the link below. Single use, valid ${Math.round(p.expiresInSeconds / 60)} minutes.`,
      body,
      buttons: [{ label: 'Done', value: 'cancel' }],
    });
  }

  private async revoke(d: PairedDevice | 'all'): Promise<void> {
    const all = d === 'all';
    const me = !all && d.current;
    const ok = await confirm(
      all ? 'Sign out every device?' : me ? 'Sign out this device?' : `Remove ${d.name}?`,
      all ? 'Every paired browser, this one included, has to pair again with a new code.' : me ? 'You will need a new pairing code to use the app here again.' : 'It will need a new pairing code to use the app again.',
      all || me ? 'Sign out' : 'Remove',
      true,
    );
    if (!ok) return;
    await this.ctx.api.revoke(all ? { all: true } : { id: d.id });
    if (all || me) location.reload();
    else this.loadDevices();
  }

  private pick<K extends keyof Settings>(key: K, title: string, choices: Choice<K>[]): Row {
    const cur = settings()[key];
    return {
      key,
      title,
      value: choices.find((c) => c.value === cur)?.label ?? String(cur),
      chevron: true,
      onTap: () => actionSheet(title, choices.map((c) => ({ label: (c.value === cur ? '✓ ' : '') + c.label, detail: c.detail, run: () => updateSettings({ [key]: c.value } as Partial<Settings>) }))),
    };
  }

  private toggle(key: 'showHistorical' | 'showExited' | 'showFinishedSubagents' | 'notifyAttention', title: string): Row {
    return { key, title, toggle: { on: settings()[key], onChange: (on) => updateSettings({ [key]: on }) } };
  }

  render(): void {
    this.setTitle('Settings');
    const info = this.ctx.info();
    const { flows } = this.ctx;
    if (Date.now() - this.devicesAt > 15_000) this.loadDevices(); // another device may have paired since
    renderSections(this.body, [
      {
        key: 'devices',
        header: 'Paired devices',
        footer: privateConnection() ? undefined : 'This connection is plain HTTP: anyone on the network can read what the app shows and copy a device’s pairing. Securing the route (Tailscale, a VPN, a TLS proxy) is up to you.',
        rows: compact<Row>([
          { key: 'pair', title: 'Pair another device…', action: true, onTap: () => flows.run(this.pairAnother()) },
          ...(this.devices ?? []).map((d) => ({
            key: `dev-${d.id}`,
            icon: /iphone|android phone/i.test(d.name) ? 'device-mobile' : 'browser',
            tint: d.current ? 'var(--blue)' : 'var(--gray)',
            title: d.current ? `${d.name} (this device)` : d.name,
            sub: `Paired ${relativeTime(d.pairedAt)}${d.lastSeen && !d.current ? ` · seen ${relativeTime(d.lastSeen)}` : ''}`,
            button: { label: d.current ? 'Sign out' : 'Remove', destructive: true, onTap: () => flows.run(this.revoke(d)) },
          })),
          this.devicesError && { key: 'deverr', title: 'Could not list devices', sub: this.devicesError },
          (this.devices?.length ?? 0) > 1 && { key: 'all', title: 'Sign out every device', destructive: true, onTap: () => flows.run(this.revoke('all')) },
        ]),
      },
      {
        key: 'view',
        header: 'Lists',
        rows: [
          this.toggle('showHistorical', 'Workspaces without agents'),
          this.toggle('showExited', 'Exited agents'),
          this.toggle('showFinishedSubagents', 'Finished subagents and tasks'),
          this.pick('hideUnseenDays', 'Hide machines unseen for', [
            { value: 7, label: '7 days' },
            { value: 30, label: '30 days' },
            { value: 90, label: '90 days' },
            { value: 0, label: 'Never hide' },
          ]),
        ],
      },
      {
        key: 'sort',
        header: 'Order',
        rows: [
          this.pick('sortMachines', 'Machines', [
            { value: 'status', label: 'Online first' },
            { value: 'name', label: 'By name' },
            { value: 'recent', label: 'Recently reported' },
          ]),
          this.pick('sortWorkspaces', 'Workspaces', [
            { value: 'name', label: 'By name' },
            { value: 'recent', label: 'Recent activity' },
            { value: 'attention', label: 'Needs you first' },
          ]),
          this.pick('sortAgents', 'Agents', [
            { value: 'recent', label: 'Newest first' },
            { value: 'name', label: 'By title' },
            { value: 'attention', label: 'Needs you first' },
          ]),
        ],
      },
      {
        key: 'chat',
        header: 'Chat',
        footer: 'Observed sessions are ones Vineyard did not start (the Claude Code pane, a terminal). Taking one over ends its process there and resumes it under Vineyard, so questions and permission prompts can be answered here.',
        rows: [
          this.pick('observed', 'Observed sessions', [
            { value: 'observe', label: 'Watch', detail: 'Messages go in between tool calls' },
            { value: 'takeOver', label: 'Take over', detail: 'Asks first when it is mid-turn' },
          ]),
          this.pick('defaultPermissionMode', 'New agents’ mode', [
            { value: '', label: 'Claude Code’s setting' },
            { value: 'default', label: 'Ask before edits' },
            { value: 'acceptEdits', label: 'Accept edits' },
            { value: 'plan', label: 'Plan mode' },
            { value: 'auto', label: 'Auto' },
            { value: 'bypassPermissions', label: 'Bypass permissions' },
          ]),
          this.pick('transcriptLines', 'Transcript lines loaded', [
            { value: 200, label: '200' },
            { value: 400, label: '400' },
            { value: 1000, label: '1000' },
          ]),
          this.toggle('notifyAttention', 'Banner when an agent needs you'),
        ],
      },
      {
        key: 'about',
        header: 'About',
        footer: 'Any paired device can read your transcripts and drive your agents on every machine.',
        rows: compact<Row>([
          detail('ver', 'Vineyard web', info?.version),
          detail('server', 'Served by', info?.name),
          detail('link', 'Daemon link', this.ctx.store.state),
          { key: 'reset', title: 'Reset settings', destructive: true, onTap: () => updateSettings(DEFAULTS) },
        ]),
      },
    ]);
  }
}

// ---- chat ---------------------------------------------------------------------------------------

export class ChatScreen extends Screen {
  private readonly frame = h('iframe');
  private host: ChatHost | undefined;

  constructor(
    route: string,
    ctx: Ctx,
    private readonly agentId: string,
  ) {
    super(route, ctx);
    this.el.classList.add('chat');
    this.nav.hidden = true;
    this.content.hidden = true;
    this.frame.title = 'Chat';
    this.el.append(this.frame);
  }

  title(): string {
    const a = this.ctx.store.findAgent(this.agentId)?.agent;
    return a ? agentLabel(a) : 'Chat';
  }

  override mount(): void {
    if (this.host) return;
    this.host = new ChatHost(this.frame, this.agentId, {
      api: this.ctx.api,
      store: this.ctx.store,
      info: () => this.ctx.info(),
      navigate: (r) => this.ctx.navigate(r),
      back: () => this.ctx.back(),
      subagentRoute: (id) => R.chat(id),
      settingsRoute: () => R.settings,
      historyRoute: (mid, cwd) => R.history(mid, cwd),
      takeOver: (m, a) => this.ctx.flows.takeOver(m, a),
      login: (m) => this.ctx.flows.login(m),
    });
    this.frame.src = `chat.html#${encodeURIComponent(this.agentId)}`;
  }

  /** The frame's document, for the page to pass its safe-area insets in. */
  get frameDoc(): Document | undefined {
    try {
      return this.frame.contentDocument ?? undefined;
    } catch {
      return undefined;
    }
  }

  get frameEl(): HTMLIFrameElement {
    return this.frame;
  }

  render(): void {
    /* the chat host follows the store itself */
  }

  override destroy(): void {
    this.host?.dispose();
    this.frame.src = 'about:blank';
  }
}

export function screenFor(route: string, ctx: Ctx): Screen {
  const r: Route = parseRoute(route);
  switch (r.kind) {
    case 'home':
      return new HomeScreen(route, ctx);
    case 'machine':
      return new MachineScreen(route, ctx, r.machine);
    case 'workspace':
      return new WorkspaceScreen(route, ctx, r.machine, r.path);
    case 'history':
      return new HistoryScreen(route, ctx, r.machine, r.cwd);
    case 'log':
      return new LogScreen(route, ctx);
    case 'chat':
      return new ChatScreen(route, ctx, r.agent);
    case 'agent':
      return new AgentScreen(route, ctx, r.agent);
    case 'settings':
      return new SettingsScreen(route, ctx);
  }
}

/** A screen's title without building it, for the back button of a screen opened cold. */
export function titleForRoute(route: string, store: FleetStore): string {
  const r = parseRoute(route);
  switch (r.kind) {
    case 'home':
      return 'Vineyard';
    case 'machine':
      return store.machine(r.machine)?.name ?? 'Machine';
    case 'workspace':
      return basename(r.path);
    case 'history':
      return 'Past sessions';
    case 'log':
      return 'Log';
    case 'settings':
      return 'Settings';
    case 'chat':
    case 'agent': {
      const a = store.findAgent(r.agent)?.agent;
      return a ? agentLabel(a) : 'Agent';
    }
  }
}

