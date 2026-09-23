/**
 * The composer's toolbar, modelled on the Claude Code pane: attach (+), the "/" actions menu, a
 * context-window ring, a prompt-cache dot, the subagent count, and the model and permission-mode pills
 * whose popovers carry the effort slider. main.ts owns the textarea and the transport; this module
 * owns everything under it and asks main.ts to act through BarDeps.
 */

import type { BackgroundTask, CommandInfo, ModelInfo, Subagent } from '../core/model.ts';
import { effortOptions, modelOptions, selectedModel } from '../core/models.ts';
import { shortModel, tokens as fmtTokens } from '../core/format.ts';
import { clock, taskActive, taskElapsed, taskLabel, taskStateLabel } from '../core/tasks.ts';
import { GROUP, MODES, buildActions, cacheState, contextGauge, contextWindow, effortLabel, filterActions, groupActions, modeInfo, type Action } from '../core/composer.ts';

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
  lastActivityAt?: number;
  version?: string;
  subagents?: Subagent[];
  tasks?: BackgroundTask[];
  managed?: { exited: boolean; model?: string; effort?: string; permissionMode?: string; models?: ModelInfo[]; commands?: CommandInfo[]; account?: string };
}
export interface BarMachine {
  id: string;
  name: string;
  online: boolean;
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

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls?: string, text?: string): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}
function icon(name: string): HTMLElement {
  return el('i', `codicon codicon-${name}`);
}
function pill(id: string, title: string): HTMLButtonElement {
  const b = el('button', 'pill');
  b.id = id;
  b.title = title;
  b.type = 'button';
  return b;
}

/** A ring that fills clockwise; the label sits beside it. */
function ring(fraction: number, level: string): SVGSVGElement {
  const r = 5.5;
  const c = 2 * Math.PI * r;
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('class', `ring ring-${level}`);
  const track = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
  track.setAttribute('class', 'ring-track');
  const fill = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
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

  // ---- popover ----------------------------------------------------------------------------------
  const pop = el('div', 'pop');
  pop.hidden = true;
  popHost.appendChild(pop);
  let open: 'actions' | 'typed' | 'model' | 'mode' | 'agents' | undefined;
  let rows: HTMLElement[] = [];
  let hi = -1;
  let filterBox: HTMLInputElement | undefined;
  // Escape on the typed "/" menu keeps it shut until the text stops looking like a slash command.
  let typedDismissed = false;

  let agent: BarAgent | undefined;
  let machine: BarMachine | undefined;
  let stats: BarStats = { lastCacheRead: 0, lastCacheCreate: 0, lastInput: 0, calls: 0, transcriptAgents: [] };

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
    for (const v of levels) {
      const stop = el('span', 'estop' + (v === cur ? ' active' : '') + (v === levels[levels.length - 1] ? ' top' : ''));
      stop.title = effortLabel(v) + (live ? '' : ' (read-only)');
      stop.dataset.v = v;
      if (live) {
        stop.onclick = (e) => {
          e.stopPropagation();
          if (v !== cur) deps.configure({ effort: v });
          close();
        };
      }
      wrap.appendChild(stop);
    }
    return wrap;
  }
  function effortRow(): HTMLElement {
    const row = el('div', 'pop-row effort-row');
    const lbl = el('span', 'pop-label');
    lbl.append('Effort ', el('span', 'dim', `(${effortLabel(currentEffort())})`));
    row.append(lbl, effortSlider());
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
      row.append(icon(m.icon));
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
      extVersion: deps.extVersion,
      claudeVersion: agent?.version,
    });
  }
  function runAction(a: Action) {
    if (a.disabled) return;
    if (a.id === 'model') return openModel();
    if (a.id === 'mode') return openMode();
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
        const lbl = el('div', 'pop-label', a.label);
        if (a.kind === 'effort') lbl.append(' ', el('span', 'dim', `(${effortLabel(currentEffort())})`));
        text.append(lbl);
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
    if (managedLive()) deps.run('compact');
  };
  agentsPill.onclick = () => (open === 'agents' ? close() : openAgents());

  // ---- render --------------------------------------------------------------------------------------
  function render(a: BarAgent | undefined, m: BarMachine | undefined, s: BarStats) {
    agent = a;
    machine = m;
    stats = s;
    bar.classList.remove('busy');
    const send = canSend();
    btnAttach.disabled = !send;
    btnAttach.title = send ? 'Attach file…' : 'This session cannot take messages';

    // Context ring: share of the window in use; click compacts a managed session.
    const tokensUsed = agent?.contextTokens;
    ctxPill.hidden = !tokensUsed;
    if (tokensUsed) {
      // The window depends on the wire id ("[1m]" suffix), which the picker row knows better than the transcript.
      const wireModel = models()?.find((x) => (x.value === 'default' ? '' : x.value) === selectedRow())?.resolvedModel || currentModel();
      const g = contextGauge(tokensUsed, wireModel);
      ctxPill.innerHTML = '';
      ctxPill.append(ring(g.fraction, g.level), el('span', undefined, `${g.percent}%`));
      ctxPill.className = `pill ctx ctx-${g.level}` + (managedLive() ? ' action' : '');
      ctxPill.title = `${fmtTokens(tokensUsed)} of ${fmtTokens(contextWindow(wireModel))} tokens in context (${g.percent}%)` + (managedLive() ? '\nClick to compact the conversation' : '');
    }

    // Prompt cache: hit rate of the last call, and whether the prefix is likely still cached.
    const cs = cacheState({ cacheRead: stats.lastCacheRead, cacheCreate: stats.lastCacheCreate, input: stats.lastInput, calls: stats.calls }, agent?.lastActivityAt);
    cachePill.hidden = cs.state === 'none';
    if (cs.state !== 'none') {
      cachePill.innerHTML = '';
      cachePill.append(el('span', `dot cache-${cs.state}`), el('span', undefined, `${cs.hitPercent}%`));
      cachePill.className = 'pill cache';
      cachePill.title = `Prompt cache ${cs.state}: ${cs.hitPercent}% of the last call's input was read from cache (${fmtTokens(stats.lastCacheRead)} of ${fmtTokens(stats.lastCacheRead + stats.lastCacheCreate + stats.lastInput)})` + (cs.state === 'cold' ? '\nMore than 5 minutes since the last call, so the cached prefix has likely expired' : '');
    }

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
      if (!value.startsWith('/')) typedDismissed = false;
      if (open === 'typed') {
        if (!slashToken) return close();
        const list = pop.querySelector<HTMLElement>('.pop-list');
        if (list) renderActionList(list, value.slice(1));
      } else if (!open && slashToken && !typedDismissed) {
        openActions(value.slice(1), true);
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
