/**
 * The composer's toolbar, modelled on the Claude Code pane: attach (+), the "/" actions menu, a
 * context-window donut (exact figure in its tooltip; spins while Claude Code compacts), a prompt-cache
 * clock counting down to expiry, the subagent count, and the model and permission-mode pills whose popovers carry the effort
 * slider. main.ts owns the textarea and the transport; this module
 * owns everything under it and asks main.ts to act through BarDeps.
 */

import type { BackgroundTask, CommandInfo, ModelInfo, Subagent, Usage } from '../core/model.ts';
import { resetsIn, usageRows, usageWarning, usageWarningKey } from '../core/usage.ts';
import { effortOptions, modelOptions, selectedModel } from '../core/models.ts';
import { duration, shortModel, tokens as fmtTokens } from '../core/format.ts';
import { clock, taskActive, taskElapsed, taskLabel, taskStateLabel } from '../core/tasks.ts';
import { GROUP, MODES, buildActions, cacheClock, contextFor, contextGauge, effortLabel, filterActions, groupActions, modeInfo, type Action, type CacheClockInput } from '../core/composer.ts';

export interface BarAgent {
  sessionId: string;
  alive: boolean;
  name?: string;
  title?: string;
  kind?: string;
  state: string;
  model?: string;
  effort?: string;
  permissionMode?: string;
  contextTokens?: number;
  contextWindow?: number;
  lastActivityAt?: number;
  version?: string;
  subagents?: Subagent[];
  tasks?: BackgroundTask[];
  managed?: { exited: boolean; compacting?: boolean; model?: string; effort?: string; permissionMode?: string; models?: ModelInfo[]; commands?: CommandInfo[]; account?: string; accountOrg?: string; accountPlan?: string; usage?: Usage };
}
export interface BarMachine {
  id: string;
  name: string;
  online: boolean;
  /** The machine's newest account-limit report (from any session Vineyard started there). */
  usage?: Usage;
}
/**
 * Token counters from the loaded transcript window (main.ts's stats) plus the Agent-tool calls it
 * saw, used for the map when the daemon sends no subagent tree (older daemon, or a subagent's view).
 */
export interface BarStats {
  lastCacheRead: number;
  lastCacheCreate: number;
  lastInput: number;
  calls: number;
  /** What the prompt-cache clock counts from (core/composer.ts cacheClock). */
  cache: CacheClockInput;
  transcriptAgents: { name: string; desc: string; done: boolean; error: boolean }[];
}
export interface AttachmentChip {
  id: string;
  name: string;
  mediaType: string;
  size: number;
}

export interface BarDeps {
  configure(change: { model?: string; effort?: string; permissionMode?: string }): void;
  /** Run an action by id (see core/composer.ts buildActions); 'slash' carries the command text. */
  run(id: string, arg?: string): void;
  /** Put text into the composer, e.g. a slash command waiting for its argument. */
  insert(text: string): void;
  extVersion?: string;
}

export interface ComposerBar {
  render(agent: BarAgent | undefined, machine: BarMachine | undefined, stats: BarStats): void;
  setAttachments(items: AttachmentChip[]): void;
  /** Open the actions menu (the "/" button), optionally pre-filtered. */
  openActions(query?: string): void;
  /** The composer's text changed: open, filter or close the typed "/" menu accordingly. */
  onInput(value: string): void;
  /** A key pressed in the composer while a menu is open; true when the menu consumed it. */
  onKey(e: KeyboardEvent): boolean;
  isOpen(): boolean;
  close(): void;
  setBusy(busy: boolean): void;
}

const ACTIVE = new Set(['working', 'thinking', 'tool', 'question', 'permission']);
/** Limit banners the user closed, for as long as this webview lives; see usageWarningKey. */
const dismissedLimits = new Set<string>();

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls?: string, text?: string): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}
function icon(name: string): HTMLElement {
  return el('i', `codicon codicon-${name}`);
}
const SVG = 'http://www.w3.org/2000/svg';
/** An open hand (Lucide's "hand", ISC): codicons have no hand glyph, and Manual mode wants one. */
const HAND_PATHS = ['M18 11V6a2 2 0 0 0-2-2a2 2 0 0 0-2 2', 'M14 10V4a2 2 0 0 0-2-2a2 2 0 0 0-2 2v2', 'M10 10.5V6a2 2 0 0 0-2-2a2 2 0 0 0-2 2v8', 'M18 8a2 2 0 1 1 4 0v6a8 8 0 0 1-8 8h-2c-2.8 0-4.5-.86-5.99-2.34l-3.6-3.6a2 2 0 0 1 2.83-2.82L7 15'];
/** A permission mode's icon: a codicon, or the inline hand for Manual. */
function modeIcon(name: string): Element {
  if (name !== 'hand') return icon(name);
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'mode-svg');
  svg.setAttribute('aria-hidden', 'true');
  for (const d of HAND_PATHS) {
    const path = document.createElementNS(SVG, 'path');
    path.setAttribute('d', d);
    svg.append(path);
  }
  return svg;
}
function pill(id: string, title: string): HTMLButtonElement {
  const b = el('button', 'pill');
  b.id = id;
  b.title = title;
  b.type = 'button';
  return b;
}

/** A donut that fills clockwise; there is no label, the pill's tooltip carries the figure. */
function ring(fraction: number, level: string): SVGSVGElement {
  const r = 5.5;
  const c = 2 * Math.PI * r;
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('class', `ring ring-${level}`);
  svg.setAttribute('aria-hidden', 'true');
  const track = document.createElementNS(SVG, 'circle');
  track.setAttribute('class', 'ring-track');
  const fill = document.createElementNS(SVG, 'circle');
  fill.setAttribute('class', 'ring-fill');
  for (const k of [track, fill]) {
    k.setAttribute('cx', '8');
    k.setAttribute('cy', '8');
    k.setAttribute('r', String(r));
  }
  fill.setAttribute('stroke-dasharray', `${(fraction * c).toFixed(2)} ${c.toFixed(2)}`);
  svg.append(track, fill);
  return svg;
}

export function createComposerBar(host: HTMLElement, popHost: HTMLElement, deps: BarDeps): ComposerBar {
  // ---- toolbar ----------------------------------------------------------------------------------
  const bar = el('div', 'bar');
  const btnAttach = pill('btnAttach', 'Attach file…');
  btnAttach.append(icon('add'));
  const btnActions = pill('btnActions', 'Actions (or type / in the message box)');
  btnActions.append(el('span', 'slash-glyph', '/'));
  const ctxPill = pill('ctxPill', '');
  const cachePill = pill('cachePill', '');
  const agentsPill = pill('agentsPill', '');
  const modelPill = pill('modelPill', 'Model and reasoning effort');
  const modePill = pill('modePill', 'Permission mode');
  bar.append(btnAttach, btnActions, ctxPill, cachePill, agentsPill, modelPill, modePill);
  const chips = el('div', 'attachments');
  chips.hidden = true;
  host.prepend(bar);
  host.parentElement!.insertBefore(chips, host);
  // The limit banner sits above the message box, like the Claude Code pane's.
  const limitBar = el('div', 'limit');
  limitBar.hidden = true;
  popHost.insertBefore(limitBar, host.parentElement);

  // ---- popover ----------------------------------------------------------------------------------
  const pop = el('div', 'pop');
  pop.hidden = true;
  popHost.appendChild(pop);
  let open: 'actions' | 'typed' | 'model' | 'mode' | 'agents' | 'usage' | undefined;
  let rows: HTMLElement[] = [];
  let hi = -1;
  let filterBox: HTMLInputElement | undefined;
  // Escape on the typed "/" menu keeps it shut until the text stops looking like a slash command.
  let typedDismissed = false;

  let agent: BarAgent | undefined;
  let machine: BarMachine | undefined;
  let stats: BarStats = { lastCacheRead: 0, lastCacheCreate: 0, lastInput: 0, calls: 0, cache: {}, transcriptAgents: [] };
  /** Repaints the cache clock each second while it counts down, so the minutes tick and expiry shows itself. */
  let cacheTimer: number | undefined;

  const managedLive = () => !!agent?.managed && !agent.managed.exited && !!machine?.online && agent.alive;
  const canSend = () => !!machine?.online && !!agent?.alive && agent.kind !== 'subagent';
  const currentModel = () => agent?.managed?.model || agent?.model || '';
  const currentEffort = () => agent?.managed?.effort || agent?.effort || '';
  const currentMode = () => agent?.managed?.permissionMode || agent?.permissionMode || 'default';
  const models = () => agent?.managed?.models;
  const selectedRow = () => selectedModel(models(), currentModel());
  const modelLabel = () => {
    const row = models()?.find((m) => (m.value === 'default' ? '' : m.value) === selectedRow());
    const id = row?.resolvedModel || currentModel();
    return shortModel(id) || row?.displayName || (currentModel() ? currentModel() : 'Default model');
  };

  function close() {
    pop.hidden = true;
    pop.innerHTML = '';
    open = undefined;
    rows = [];
    hi = -1;
    filterBox = undefined;
    for (const b of [btnActions, modelPill, modePill, agentsPill]) b.classList.remove('open');
  }
  function show(kind: typeof open, anchor: HTMLElement) {
    close();
    open = kind;
    pop.hidden = false;
    anchor.classList.add('open');
  }
  document.addEventListener('mousedown', (e) => {
    if (open && !pop.contains(e.target as Node) && !bar.contains(e.target as Node)) close();
  });
  function dismiss() {
    if (open === 'typed') typedDismissed = true;
    close();
  }
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && open) {
      dismiss();
      e.stopPropagation();
    }
  });

  function highlight(i: number) {
    if (!rows.length) return;
    hi = (i + rows.length) % rows.length;
    rows.forEach((r, k) => r.classList.toggle('hi', k === hi));
    rows[hi]!.scrollIntoView({ block: 'nearest' });
  }
  function nav(e: KeyboardEvent): boolean {
    if (!open) return false;
    switch (e.key) {
      case 'ArrowDown':
        highlight(hi + 1);
        break;
      case 'ArrowUp':
        highlight(hi - 1);
        break;
      case 'Home':
        highlight(0);
        break;
      case 'End':
        highlight(rows.length - 1);
        break;
      case 'Enter': {
        if (hi < 0 || !rows[hi]) return false; // nothing to run: let the composer send the text
        const row = rows[hi]!;
        if (open === 'typed') deps.insert(''); // the "/query" was for the menu, not the agent
        row.click();
        break;
      }
      case 'Escape':
        dismiss();
        break;
      default:
        return false;
    }
    e.preventDefault();
    return true;
  }

  // ---- effort slider -----------------------------------------------------------------------------
  function effortSlider(): HTMLElement {
    const wrap = el('div', 'eslider');
    const levels = effortOptions(models(), selectedRow())
      .map(([v]) => v)
      .filter(Boolean);
    const cur = currentEffort();
    const live = managedLive();
    if (!levels.length) {
      wrap.append(el('span', 'dim', 'This model takes no effort setting'));
      return wrap;
    }
    wrap.classList.toggle('readonly', !live);
    wrap.setAttribute('role', 'radiogroup');
    wrap.setAttribute('aria-label', 'Reasoning effort');
    // The track fills up to the current stop, like a slider; the stop itself is the knob.
    const at = levels.indexOf(cur);
    levels.forEach((v, i) => {
      const cls = ['estop'];
      if (i === 0) cls.push('first');
      if (i === levels.length - 1) cls.push('top');
      if (v === cur) cls.push('active');
      if (at >= 0 && i < at) cls.push('filled');
      if (i === at - 1) cls.push('last-filled');
      const stop = el('span', cls.join(' '));
      stop.title = effortLabel(v) + (live ? '' : ' (read-only)');
      stop.dataset.v = v;
      stop.setAttribute('role', 'radio');
      stop.setAttribute('aria-checked', String(v === cur));
      stop.setAttribute('aria-label', effortLabel(v));
      if (live) {
        stop.onclick = (e) => {
          e.stopPropagation();
          if (v !== cur) deps.configure({ effort: v });
          close();
        };
      }
      wrap.appendChild(stop);
    });
    wrap.appendChild(el('span', 'ecur', effortLabel(cur)));
    return wrap;
  }
  function effortRow(): HTMLElement {
    const row = el('div', 'pop-row effort-row');
    row.append(el('span', 'pop-label', 'Effort'), effortSlider());
    return row;
  }
  function readOnlyNote(): HTMLElement | undefined {
    if (managedLive()) return undefined;
    const why = !machine?.online ? `${machine?.name ?? 'The machine'} is offline.` : !agent?.alive ? 'This session has exited.' : 'Only sessions started by Vineyard can be changed from here.';
    return el('div', 'pop-note', why);
  }

  // ---- model picker ------------------------------------------------------------------------------
  function openModel() {
    show('model', modelPill);
    pop.append(el('div', 'pop-title', 'Select a model'));
    const list = el('div', 'pop-list');
    const sel = selectedRow();
    const live = managedLive();
    let opts = modelOptions(models());
    if (!models()?.length && currentModel()) opts = [[currentModel(), shortModel(currentModel()) || currentModel(), currentModel()]];
    else if (sel && !opts.some(([v]) => v === sel)) opts = [...opts, [sel, shortModel(sel) || sel, sel]];
    for (const [value, label, desc] of opts) {
      const row = el('div', 'pop-item' + (value === sel ? ' selected' : '') + (live ? '' : ' readonly'));
      const text = el('div', 'pop-text');
      text.append(el('div', 'pop-label', label));
      const m = models()?.find((x) => (x.value === 'default' ? '' : x.value) === value);
      const sub = desc || (m?.resolvedModel ? shortModel(m.resolvedModel) : '');
      if (sub) text.append(el('div', 'pop-desc', sub));
      row.append(text);
      if (value === sel) row.append(icon('check'));
      if (live) {
        row.onclick = () => {
          if (value !== sel) deps.configure({ model: value });
          close();
        };
      }
      list.append(row);
      rows.push(row);
    }
    pop.append(list, el('div', 'pop-sep'), effortRow());
    const note = readOnlyNote();
    if (note) pop.append(note);
    highlight(Math.max(0, opts.findIndex(([v]) => v === sel)));
  }

  // ---- mode picker -------------------------------------------------------------------------------
  function openMode() {
    show('mode', modePill);
    pop.append(el('div', 'pop-title', 'Modes'));
    const list = el('div', 'pop-list');
    const cur = currentMode();
    const live = managedLive();
    for (const m of MODES) {
      const row = el('div', 'pop-item mode' + (m.value === cur ? ' selected' : '') + (live ? '' : ' readonly'));
      row.append(modeIcon(m.icon));
      const text = el('div', 'pop-text');
      text.append(el('div', 'pop-label', m.label), el('div', 'pop-desc', m.description));
      row.append(text);
      if (m.value === cur) row.append(icon('check'));
      if (live) {
        row.onclick = () => {
          if (m.value !== cur) deps.configure({ permissionMode: m.value });
          close();
        };
      }
      list.append(row);
      rows.push(row);
    }
    pop.append(list, el('div', 'pop-sep'), effortRow());
    const note = readOnlyNote();
    if (note) pop.append(note);
    highlight(Math.max(0, MODES.findIndex((m) => m.value === cur)));
  }

  // ---- agent map ---------------------------------------------------------------------------------
  const dotClass = (state: string) => (ACTIVE.has(state) ? 'live' : state === 'done' || state === 'idle' ? 'ok' : 'off');
  function subagentRow(sub: Subagent, now: number): HTMLElement {
    const row = el('div', 'amap-row' + (agent?.kind === 'subagent' ? '' : ' clickable'));
    row.append(el('span', `amap-dot ${dotClass(sub.state)}`));
    const text = el('div', 'amap-text');
    text.append(el('div', 'amap-name', sub.description || sub.type || sub.agentId.slice(0, 8)));
    const elapsed = sub.startedAt ? Math.max(0, (ACTIVE.has(sub.state) ? now : sub.lastActivityAt || now) - sub.startedAt) : 0;
    const meta = [elapsed ? clock(elapsed) : '', sub.contextTokens ? `${fmtTokens(sub.contextTokens)} tokens` : '', sub.type && sub.description ? sub.type : '', sub.background ? 'background' : ''].filter(Boolean);
    text.append(el('div', 'amap-meta', meta.join(' · ')));
    row.append(text);
    row.title = `${sub.state}${sub.stateDetail ? ` — ${sub.stateDetail}` : ''}${sub.model ? `\n${shortModel(sub.model)}` : ''}\nClick to open its transcript`;
    row.onclick = () => {
      close();
      deps.run('openSubagent', sub.agentId);
    };
    return row;
  }
  function subagentTree(host: HTMLElement, subs: Subagent[], parentId: string | undefined, now: number) {
    const kids = subs.filter((s) => (s.parentAgentId || undefined) === parentId);
    if (!kids.length) return;
    const box = el('div', 'amap-children');
    for (const k of kids) {
      box.append(subagentRow(k, now));
      subagentTree(box, subs, k.agentId, now);
    }
    host.append(box);
  }
  function openAgents() {
    show('agents', agentsPill);
    const now = Date.now();
    const subs = agent?.subagents;
    const tasks = agent?.tasks ?? [];
    const total = subs ? subs.length : stats.transcriptAgents.length;
    pop.append(el('div', 'pop-title', 'Agent map'));
    pop.append(el('div', 'pop-hint', `${total} agent${total === 1 ? '' : 's'}${subs?.length ? ' · click an agent for its transcript' : ''}`));
    const map = el('div', 'amap');
    const root = el('div', 'amap-row root');
    root.append(el('span', `amap-dot ${dotClass(agent?.alive ? agent.state : 'exited')}`));
    const rootText = el('div', 'amap-text');
    rootText.append(el('div', 'amap-name', agent?.title || agent?.name || agent?.sessionId.slice(0, 8) || 'session'));
    rootText.append(el('div', 'amap-meta', [modelLabel(), agent?.contextTokens ? `${fmtTokens(agent.contextTokens)} tokens in context` : ''].filter(Boolean).join(' · ')));
    root.append(rootText);
    map.append(root);
    if (subs) subagentTree(map, subs, undefined, now);
    else if (stats.transcriptAgents.length) {
      const box = el('div', 'amap-children');
      for (const s of stats.transcriptAgents.slice(-20)) {
        const row = el('div', 'amap-row');
        row.append(el('span', `amap-dot ${s.done ? (s.error ? 'err' : 'ok') : 'live'}`));
        const text = el('div', 'amap-text');
        text.append(el('div', 'amap-name', s.desc || s.name), el('div', 'amap-meta', [s.name, s.done ? (s.error ? 'failed' : 'done') : 'running'].join(' · ')));
        row.append(text);
        box.append(row);
      }
      map.append(box);
    }
    pop.append(map);
    if (tasks.length) {
      const running = tasks.filter(taskActive).length;
      pop.append(el('div', 'pop-sep'));
      pop.append(el('div', 'pop-hint', `${running || tasks.length} background task${(running || tasks.length) === 1 ? '' : 's'}${running ? '' : ' finished'}`));
      const list = el('div', 'amap tasks');
      for (const t of tasks) {
        const row = el('div', 'amap-row');
        row.append(el('span', `amap-dot ${taskActive(t) ? 'live' : t.state === 'completed' ? 'ok' : 'err'}`));
        const text = el('div', 'amap-text');
        text.append(el('div', 'amap-name', taskLabel(t)));
        const elapsed = taskElapsed(t, now);
        text.append(el('div', 'amap-meta', [t.kind || 'shell', taskActive(t) ? '' : taskStateLabel(t).toLowerCase(), elapsed ? clock(elapsed) : ''].filter(Boolean).join(' · ')));
        row.append(text);
        if (t.taskId) row.title = `Task ${t.taskId}`;
        list.append(row);
      }
      pop.append(list);
    }
  }

  // ---- account & usage ---------------------------------------------------------------------------
  /** The session's own report when it has one, else the machine's; whichever is newer. */
  const currentUsage = (): Usage | undefined => {
    const a = agent?.managed?.usage;
    const m = machine?.usage;
    if (a && m) return a.at >= m.at ? a : m;
    return a ?? m;
  };
  function openUsage() {
    show('usage', agentsPill);
    agentsPill.classList.remove('open');
    pop.append(el('div', 'pop-title', 'Account & usage'));
    const acct = agent?.managed;
    const grid = el('div', 'ugrid');
    const row = (k: string, v: string | undefined) => {
      if (!v) return;
      grid.append(el('div', 'k', k), el('div', 'v', v));
    };
    pop.append(el('div', 'pop-group', 'Account'));
    if (acct?.account || acct?.accountOrg || acct?.accountPlan) {
      row('Email', acct.account);
      row('Organization', acct.accountOrg);
      row('Plan', acct.accountPlan ? `Claude ${acct.accountPlan}` : undefined);
      pop.append(grid);
    } else {
      pop.append(el('div', 'pop-note', 'Who the session is signed in as is known for sessions started by Vineyard.'));
    }
    pop.append(el('div', 'pop-group', 'Usage'));
    const u = currentUsage();
    const rows = usageRows(u);
    if (!rows.length) {
      pop.append(el('div', 'pop-note', `Limits appear once a session started by Vineyard on ${machine?.name ?? 'this machine'} has made a request; Claude Code reports them with each response.`));
      return;
    }
    const list = el('div', 'ulist');
    for (const r of rows) {
      const item = el('div', 'uitem');
      const head = el('div', 'uhead');
      head.append(el('span', undefined, r.label), el('span', 'upct' + (r.percent >= 100 ? ' hit' : r.percent >= 80 ? ' warn' : ''), `${r.percent}%`));
      const bar = el('div', 'ubar');
      const fill = el('div', 'ufill' + (r.percent >= 100 ? ' hit' : r.percent >= 80 ? ' warn' : ''));
      fill.style.width = `${Math.min(100, r.percent)}%`;
      bar.append(fill);
      item.append(head, bar);
      if (r.resetsAt) item.append(el('div', 'ureset', `Resets in ${resetsIn(r.resetsAt)}`));
      list.append(item);
    }
    pop.append(list);
    const note = [`As of ${resetsIn(Date.now() + (Date.now() - (u?.at ?? Date.now())))} ago`.replace('As of now ago', 'Just now'), u === agent?.managed?.usage ? 'from this session' : `from a session on ${machine?.name ?? 'this machine'}`];
    if (u?.isUsingOverage) note.push('using extra usage');
    pop.append(el('div', 'pop-note', note.join(' · ')));
  }
  function usageSummary(): string | undefined {
    const rows = usageRows(currentUsage());
    if (!rows.length) return undefined;
    const top = [...rows].sort((a, b) => b.percent - a.percent)[0]!;
    return `${top.percent}% of ${top.label.toLowerCase()}`;
  }

  // ---- actions menu ------------------------------------------------------------------------------
  function actions(): Action[] {
    return buildActions({
      online: !!machine?.online,
      alive: !!agent?.alive,
      managedLive: managedLive(),
      subagent: agent?.kind === 'subagent',
      busy: !!agent && ACTIVE.has(agent.state) && agent.state !== 'question' && agent.state !== 'permission',
      machineName: machine?.name ?? 'the machine',
      modelLabel: modelLabel(),
      effort: currentEffort(),
      modeLabel: modeInfo(currentMode()).label,
      commands: agent?.managed?.commands,
      account: agent?.managed?.account,
      usageSummary: usageSummary(),
      extVersion: deps.extVersion,
      claudeVersion: agent?.version,
    });
  }
  function runAction(a: Action) {
    if (a.disabled) return;
    if (a.id === 'model') return openModel();
    if (a.id === 'mode') return openMode();
    if (a.id === 'usage') return openUsage();
    if (a.id.startsWith('slash:')) {
      const name = a.id.slice('slash:'.length);
      close();
      if (a.value) deps.insert(`/${name} `); // wants an argument: hand it to the user to finish
      else deps.run('slash', `/${name}`);
      return;
    }
    close();
    deps.run(a.id);
  }
  function renderActionList(list: HTMLElement, query: string) {
    list.innerHTML = '';
    rows = [];
    hi = -1;
    const matched = filterActions(actions(), query);
    if (!matched.length) {
      list.append(el('div', 'pop-empty', 'No matching actions'));
      return;
    }
    for (const g of groupActions(matched)) {
      list.append(el('div', 'pop-group', g.group));
      for (const a of g.actions) {
        const row = el('div', 'pop-item' + (a.disabled ? ' disabled' : '') + (a.kind === 'effort' ? ' effort-row' : ''));
        const text = el('div', 'pop-text');
        text.append(el('div', 'pop-label', a.label));
        if (a.detail && (a.disabled || a.group === GROUP.commands)) text.append(el('div', 'pop-desc', a.detail));
        else if (a.detail) row.title = a.detail;
        row.append(text);
        if (a.kind === 'effort') row.append(effortSlider());
        else if (a.value) row.append(el('span', 'pop-value', a.value));
        if (!a.disabled && a.kind !== 'effort') {
          row.onclick = () => runAction(a);
          rows.push(row);
        }
        list.append(row);
      }
    }
    highlight(0);
  }
  function openActions(query = '', typed = false) {
    show(typed ? 'typed' : 'actions', btnActions);
    const list = el('div', 'pop-list actions');
    if (!typed) {
      filterBox = el('input', 'pop-filter');
      filterBox.placeholder = 'Filter actions…';
      filterBox.value = query;
      filterBox.oninput = () => renderActionList(list, filterBox!.value);
      filterBox.onkeydown = (e) => void nav(e);
      pop.append(filterBox);
    } else {
      pop.append(el('div', 'pop-title', 'Actions'), el('div', 'pop-hint', 'Keep typing to filter · ↑↓ to choose · Enter to run · Esc to close'));
    }
    pop.append(list);
    renderActionList(list, query);
    const foot = el('div', 'pop-foot');
    foot.append(el('span', undefined, [deps.extVersion && `Vineyard ${deps.extVersion}`, agent?.version && `Claude Code ${agent.version}`].filter(Boolean).join(' · ')));
    if (agent?.managed?.account) foot.append(el('span', 'dim', agent.managed.account));
    pop.append(foot);
    if (filterBox) filterBox.focus();
  }

  btnAttach.onclick = () => deps.run('attach');
  btnActions.onclick = () => (open === 'actions' ? close() : openActions());
  modelPill.onclick = () => (open === 'model' ? close() : openModel());
  modePill.onclick = () => (open === 'mode' ? close() : openMode());
  ctxPill.onclick = () => {
    if (managedLive() && !contextFor(agent, undefined).compacting) deps.run('compact');
  };
  agentsPill.onclick = () => (open === 'agents' ? close() : openAgents());

  // ---- render --------------------------------------------------------------------------------------
  /**
   * The prompt-cache clock: a clock with the minutes left while the cache is warm ("12m"), a bare
   * clock in the error colour once it expired or right after a compaction. Hidden until a call has
   * touched the cache. Ticks once a second while warm so the minutes count down and expiry shows.
   */
  function paintCache() {
    const cc = cacheClock(stats.cache);
    cachePill.hidden = cc.state === 'unknown';
    if (cc.state === 'warm') {
      if (!cacheTimer) cacheTimer = window.setInterval(paintCache, 1000);
    } else if (cacheTimer) {
      clearInterval(cacheTimer);
      cacheTimer = undefined;
    }
    if (cachePill.hidden) return;
    cachePill.innerHTML = '';
    cachePill.append(icon('clock'));
    if (cc.label) cachePill.append(el('span', 'cache-left', cc.label));
    cachePill.className = `pill cache cache-${cc.state}`;
    const lifetime = cc.ttlMs >= 60 * 60_000 ? '1 hour' : `${Math.round(cc.ttlMs / 60_000)} minutes`;
    const at = (t: number | undefined) => (t === undefined ? '' : new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }));
    const lines: string[] = [];
    if (cc.state === 'warm') lines.push(`Prompt cache warm, about ${Math.ceil(cc.remainingMs / 60_000)} min left.`, `Lifetime ${lifetime}; the countdown restarted at ${at(cc.anchorAt)} with the last response.`);
    else if (cc.state === 'expired') lines.push(`Prompt cache likely expired (idle ${duration(Date.now() - (cc.anchorAt ?? Date.now()))}).`, `Lifetime ${lifetime}; the next response re-caches the conversation.`);
    else lines.push('Prompt cache does not cover the compacted conversation yet.', 'The next response re-caches it.');
    const u = stats.cache.usage;
    if (cc.hitRate !== undefined && u) {
      const read = u.cache_read_input_tokens ?? 0;
      const total = read + (u.cache_creation_input_tokens ?? 0) + (u.input_tokens ?? 0);
      lines.push(`Last call: ${cc.hitRate}% of its input read from cache (${fmtTokens(read) || '0'} of ${fmtTokens(total)} tokens).`);
    }
    cachePill.title = lines.join('\n');
    cachePill.setAttribute('aria-label', lines[0]!);
  }

  function render(a: BarAgent | undefined, m: BarMachine | undefined, s: BarStats) {
    agent = a;
    machine = m;
    stats = s;
    bar.classList.remove('busy');

    // Limit banner: the fullest window past the warning line, or the one Claude Code flagged. Closing
    // it hides that window at that severity until it is hit or resets, like the Claude Code pane.
    const warn = usageWarning(currentUsage());
    const warnKey = warn ? usageWarningKey(warn) : '';
    limitBar.hidden = !warn || dismissedLimits.has(warnKey);
    if (warn && !limitBar.hidden) {
      limitBar.className = 'limit' + (warn.rejected ? ' hit' : '');
      limitBar.innerHTML = '';
      limitBar.append(icon(warn.rejected ? 'error' : 'warning'), el('span', 'limit-text', warn.text));
      const view = el('a', 'limit-link', 'View usage');
      view.href = '#';
      view.onclick = (e) => {
        e.preventDefault();
        openUsage();
      };
      const close = el('button', 'chip-x limit-x');
      close.type = 'button';
      close.title = 'Dismiss warning';
      close.append(icon('close'));
      close.onclick = () => {
        dismissedLimits.add(warnKey);
        limitBar.hidden = true;
      };
      limitBar.append(view, close);
    }

    const send = canSend();
    btnAttach.disabled = !send;
    btnAttach.title = send ? 'Attach file…' : 'This session cannot take messages';

    // Context donut: share of the window in use, the exact figure in the tooltip; click compacts a
    // managed session. While Claude Code compacts, the arc spins and the click is off.
    // When the harness has not measured the window, it follows the wire id ("[1m]" suffix), which the
    // picker row knows better than the transcript.
    const wireModel = models()?.find((x) => (x.value === 'default' ? '' : x.value) === selectedRow())?.resolvedModel || currentModel();
    const ctx = contextFor(agent, wireModel);
    ctxPill.hidden = !ctx.tokens && !ctx.compacting;
    if (!ctxPill.hidden) {
      const g = contextGauge(ctx.tokens, ctx.window);
      const compactable = managedLive() && !ctx.compacting;
      ctxPill.innerHTML = '';
      ctxPill.append(ctx.compacting ? ring(0.25, 'compacting') : ring(g.fraction, g.level));
      ctxPill.className = `pill ctx ctx-${g.level}` + (ctx.compacting ? ' compacting' : '') + (compactable ? ' action' : '');
      const figure = `${fmtTokens(ctx.tokens)} of ${fmtTokens(ctx.window)} tokens (${g.percent}%)`;
      ctxPill.title = ctx.compacting ? `Compacting the conversation…\n${figure} before compaction` : `Context window: ${figure}` + (ctx.measured ? '' : '\nWindow size inferred from the model id') + (compactable ? '\nClick to compact the conversation' : '');
      ctxPill.setAttribute('aria-label', ctxPill.title.split('\n')[0]!);
    }

    // Prompt-cache clock, like the Claude Code pane's: minutes until the cache expires while it is
    // warm, a bare red clock once it has expired or right after a compaction, until the next response.
    paintCache();

    // Subagents and background tasks: the daemon's lists when it sends them, else what the loaded
    // transcript shows for agents (tasks come only from the daemon).
    const subs = agent?.subagents;
    const total = subs ? subs.length : stats.transcriptAgents.length;
    const running = subs ? subs.filter((x) => ACTIVE.has(x.state)).length : stats.transcriptAgents.filter((x) => !x.done).length;
    const tasks = agent?.tasks ?? [];
    const runningTasks = tasks.filter(taskActive).length;
    agentsPill.hidden = !total && !tasks.length;
    if (total || tasks.length) {
      const words: string[] = [];
      if (total) words.push(`${total} agent${total === 1 ? '' : 's'}`);
      if (runningTasks) words.push(`${runningTasks} task${runningTasks === 1 ? '' : 's'}`);
      else if (!total) words.push(`${tasks.length} task${tasks.length === 1 ? '' : 's'} finished`);
      agentsPill.innerHTML = '';
      agentsPill.append(el('span', 'dot' + (running || runningTasks ? ' live' : '')), el('span', undefined, words.join(' · ')));
      agentsPill.title = `${total} subagent${total === 1 ? '' : 's'} spawned${running ? `, ${running} running` : ''}${tasks.length ? `\n${tasks.length} background task${tasks.length === 1 ? '' : 's'}${runningTasks ? `, ${runningTasks} running` : ''}` : ''}\nClick for the agent map`;
    }

    // Model + effort, and mode.
    modelPill.innerHTML = '';
    modelPill.append(el('span', undefined, modelLabel()));
    const eff = currentEffort();
    if (eff) modelPill.append(el('span', 'dim', effortLabel(eff)));
    const mode = modeInfo(currentMode());
    modePill.innerHTML = '';
    modePill.append(icon(mode.icon), el('span', undefined, mode.label));
    modePill.title = mode.description || 'Permission mode';
    for (const p of [modelPill, modePill]) p.classList.toggle('readonly', !managedLive());

    // A popover showing stale values is worse than none.
    if (open === 'model') openModel();
    else if (open === 'mode') openMode();
    else if (open === 'agents') openAgents();
    else if (open === 'usage') openUsage();
  }

  function setAttachments(items: AttachmentChip[]) {
    chips.innerHTML = '';
    chips.hidden = !items.length;
    for (const it of items) {
      const chip = el('span', 'chip');
      chip.append(icon(it.mediaType.startsWith('image/') ? 'file-media' : 'file'), el('span', 'chip-name', it.name), el('span', 'dim', fmtSize(it.size)));
      const x = el('button', 'chip-x');
      x.type = 'button';
      x.title = 'Remove';
      x.append(icon('close'));
      x.onclick = () => deps.run('removeAttachment', it.id);
      chip.append(x);
      chips.append(chip);
    }
  }

  return {
    render,
    setAttachments,
    openActions: (q) => openActions(q ?? ''),
    onInput(value) {
      // A lone "/word" is a command being typed; a space or newline after it means prose.
      const slashToken = /^\/\S*$/.test(value);
      if (!value.startsWith('/') || value === '/') typedDismissed = false;
      if (open === 'typed') {
        if (!slashToken) return close();
        const list = pop.querySelector<HTMLElement>('.pop-list');
        if (list) renderActionList(list, value.slice(1));
      } else if (slashToken && !typedDismissed) {
        openActions(value.slice(1), true); // takes over from any other popover
      }
    },
    onKey: (e) => (open ? nav(e) : false),
    isOpen: () => !!open,
    close,
    setBusy: (b) => bar.classList.toggle('busy', b),
  };
}

function fmtSize(n: number): string {
  if (n >= 1_048_576) return `${(n / 1_048_576).toFixed(1)} MB`;
  if (n >= 1024) return `${Math.round(n / 1024)} KB`;
  return `${n} B`;
}
