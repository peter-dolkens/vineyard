/**
 * Chat webview: renders a Claude Code transcript as a conversation with a railway margin, streams new
 * entries, and hosts the composer plus permission / question cards. Talks to chatPanel.ts via postMessage.
 */

import { marked } from 'marked';

declare function acquireVsCodeApi(): { postMessage(m: unknown): void };
const vscode = acquireVsCodeApi();

type Entry = Record<string, any>;
interface Pending {
  requestId: string;
  toolName: string;
  displayName?: string;
  input?: any;
  suggestions?: any[];
  requiresUserInteraction?: boolean;
  description?: string;
}
interface Agent {
  id: string;
  sessionId: string;
  alive: boolean;
  name?: string;
  title?: string;
  state: string;
  stateDetail?: string;
  model?: string;
  effort?: string;
  permissionMode?: string;
  contextTokens?: number;
  workspacePath: string;
  lastActivityAt?: number;
  startedAt?: number;
  gitBranch?: string;
  version?: string;
  pendingTools: { id: string; name: string; summary?: string }[];
  managed?: { exited: boolean; pending?: Pending; turns: number; costUsd?: number; lastError?: string; permissionMode?: string; model?: string; effort?: string };
}
interface MachineInfo {
  id: string;
  name: string;
  online: boolean;
}

marked.setOptions({ gfm: true, breaks: false });

const STATE_LABEL: Record<string, string> = {
  question: 'Asking you a question',
  permission: 'Waiting for permission',
  working: 'Working',
  thinking: 'Thinking',
  tool: 'Running a tool',
  shell: 'In a shell',
  idle: 'Idle',
  exited: 'Exited',
  unknown: 'Unknown',
};
const STATE_ICON: Record<string, string> = {
  question: 'question',
  permission: 'shield',
  working: 'loading codicon-modifier-spin',
  thinking: 'lightbulb',
  tool: 'tools',
  shell: 'terminal',
  idle: 'circle-large-filled',
  exited: 'circle-slash',
  unknown: 'circle-outline',
};
const BUSY = new Set(['working', 'thinking', 'tool']);

// ---- state ----------------------------------------------------------------------------------------

let agent: Agent | undefined;
let machine: MachineInfo | undefined;
const seen = new Set<string>();
const toolRows = new Map<string, HTMLElement>(); // tool_use_id → details row
const toolNames = new Map<string, string>();
let currentTurn: HTMLElement | undefined;
let pinned = true;
let sending = false;
let tickerTimer: number | undefined;

// Stats derived from the transcript (for the info strip).
const stats = {
  input: 0,
  cacheRead: 0,
  cacheCreate: 0,
  output: 0,
  lastCacheRead: 0,
  lastCacheCreate: 0,
  lastInput: 0,
  calls: 0,
  toolCalls: 0,
  turns: 0,
  subagents: new Map<string, { name: string; desc: string; done: boolean; error: boolean; at: string }>(),
};

// ---- skeleton -------------------------------------------------------------------------------------

const app = document.getElementById('app')!;
app.innerHTML = `
<header class="hdr">
  <div class="hdr-main">
    <span class="state-dot" id="stateIcon"></span>
    <div class="hdr-text">
      <div class="title" id="title"></div>
      <div class="sub" id="sub"></div>
    </div>
  </div>
  <div class="hdr-actions">
    <button class="icon" id="btnInfo" title="Session info"><i class="codicon codicon-info"></i></button>
    <button class="icon" id="btnTerminal" title="Open terminal here"><i class="codicon codicon-terminal"></i></button>
    <button class="icon" id="btnWorkspace" title="Open workspace in VS Code"><i class="codicon codicon-folder-opened"></i></button>
    <button class="icon" id="btnReload" title="Reload transcript"><i class="codicon codicon-refresh"></i></button>
    <button class="icon danger" id="btnStop" title="Stop this session" hidden><i class="codicon codicon-debug-stop"></i></button>
  </div>
</header>
<section class="info" id="info" hidden></section>
<div class="banner" id="banner" hidden></div>
<main id="log" class="log"><div id="turns"></div><div id="ticker" class="ticker" hidden></div></main>
<button id="jump" class="jump" hidden><i class="codicon codicon-arrow-down"></i> New messages</button>
<section id="cards" class="cards"></section>
<footer class="composer">
  <div class="composer-box">
    <textarea id="input" rows="1" placeholder="Message this agent…"></textarea>
    <div class="composer-actions">
      <div class="controls" id="controls" hidden>
        <label class="ctl" title="Model for the rest of this session"><i class="codicon codicon-hubot"></i><select id="selModel"></select></label>
        <label class="ctl" title="Reasoning effort"><i class="codicon codicon-dashboard"></i><select id="selEffort"></select></label>
        <label class="ctl" title="Permission mode"><i class="codicon codicon-shield"></i><select id="selMode"></select></label>
      </div>
      <span class="hint" id="hint"></span>
      <button class="icon" id="btnInterrupt" title="Interrupt current turn" hidden><i class="codicon codicon-debug-pause"></i></button>
      <button class="send" id="btnSend" title="Send (Enter)"><i class="codicon codicon-send"></i></button>
    </div>
  </div>
</footer>`;

const logEl = document.getElementById('log')!;
const turnsEl = document.getElementById('turns')!;
const tickerEl = document.getElementById('ticker')!;
const infoEl = document.getElementById('info')!;
const cardsEl = document.getElementById('cards')!;
const input = document.getElementById('input') as HTMLTextAreaElement;
const btnSend = document.getElementById('btnSend') as HTMLButtonElement;
const btnInterrupt = document.getElementById('btnInterrupt') as HTMLButtonElement;
const btnStop = document.getElementById('btnStop') as HTMLButtonElement;
const jump = document.getElementById('jump') as HTMLButtonElement;
const banner = document.getElementById('banner')!;
const controlsEl = document.getElementById('controls')!;
const selModel = document.getElementById('selModel') as HTMLSelectElement;
const selEffort = document.getElementById('selEffort') as HTMLSelectElement;
const selMode = document.getElementById('selMode') as HTMLSelectElement;

// Live session controls (managed sessions only), mirroring the pickers in the Claude Code pane.
const MODELS: [string, string][] = [
  ['', 'Default model'],
  ['claude-fable-5-1', 'Fable 5.1'],
  ['claude-opus-5', 'Opus 5'],
  ['claude-sonnet-5', 'Sonnet 5'],
  ['claude-haiku-4-5-20251001', 'Haiku 4.5'],
];
const EFFORTS: [string, string][] = [
  ['', 'Default effort'],
  ['low', 'Low effort'],
  ['medium', 'Medium effort'],
  ['high', 'High effort'],
  ['xhigh', 'Extra-high effort'],
  ['max', 'Max effort'],
];
const MODES: [string, string][] = [
  ['default', 'Ask before acting'],
  ['acceptEdits', 'Accept edits'],
  ['plan', 'Plan mode'],
  ['auto', 'Auto mode'],
  ['bypassPermissions', 'Bypass permissions'],
];
function fillSelect(sel: HTMLSelectElement, options: [string, string][], current: string, labelFor: (v: string) => string) {
  const opts = current && !options.some(([v]) => v === current) ? [...options, [current, labelFor(current)] as [string, string]] : options;
  if (sel.dataset.sig !== JSON.stringify(opts)) {
    sel.innerHTML = '';
    for (const [v, label] of opts) {
      const o = document.createElement('option');
      o.value = v;
      o.textContent = label;
      sel.appendChild(o);
    }
    sel.dataset.sig = JSON.stringify(opts);
  }
  sel.value = current;
}
function renderControls() {
  const live = !!agent?.managed && !agent.managed.exited && !!machine?.online;
  controlsEl.hidden = !live;
  if (!live || !agent) return;
  fillSelect(selModel, MODELS, agent.managed?.model || agent.model || '', (v) => shortModel(v) || v);
  fillSelect(selEffort, EFFORTS, agent.managed?.effort || agent.effort || '', (v) => v);
  fillSelect(selMode, MODES, agent.managed?.permissionMode || agent.permissionMode || 'default', (v) => v);
}
function configure(change: { model?: string; effort?: string; permissionMode?: string }) {
  controlsEl.classList.add('busy');
  vscode.postMessage({ type: 'configure', ...change });
}
selModel.onchange = () => configure({ model: selModel.value });
selEffort.onchange = () => configure({ effort: selEffort.value });
selMode.onchange = () => configure({ permissionMode: selMode.value });

document.getElementById('btnTerminal')!.onclick = () => vscode.postMessage({ type: 'openTerminal' });
document.getElementById('btnWorkspace')!.onclick = () => vscode.postMessage({ type: 'openWorkspace' });
document.getElementById('btnInfo')!.onclick = () => {
  infoEl.hidden = !infoEl.hidden;
  renderInfo();
};
document.getElementById('btnReload')!.onclick = () => {
  resetLog();
  vscode.postMessage({ type: 'reload' });
};
btnStop.onclick = () => vscode.postMessage({ type: 'stop' });
btnInterrupt.onclick = () => vscode.postMessage({ type: 'interrupt' });
btnSend.onclick = send;
jump.onclick = () => {
  pinned = true;
  scrollToBottom();
};
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    send();
  }
});
input.addEventListener('input', autoGrow);
logEl.addEventListener('scroll', () => {
  pinned = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
  if (pinned) jump.hidden = true;
});

function resetLog() {
  seen.clear();
  toolRows.clear();
  toolNames.clear();
  turnsEl.innerHTML = '';
  currentTurn = undefined;
  Object.assign(stats, { input: 0, cacheRead: 0, cacheCreate: 0, output: 0, lastCacheRead: 0, lastCacheCreate: 0, lastInput: 0, calls: 0, toolCalls: 0, turns: 0 });
  stats.subagents.clear();
}

function autoGrow() {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 200) + 'px';
}

function send() {
  const text = input.value.trim();
  if (!text || sending) return;
  vscode.postMessage({ type: 'send', text });
  appendLocalEcho(text);
  input.value = '';
  autoGrow();
}

function scrollToBottom() {
  logEl.scrollTop = logEl.scrollHeight;
  jump.hidden = true;
}

function afterAppend() {
  if (pinned) scrollToBottom();
  else jump.hidden = false;
}

// ---- helpers --------------------------------------------------------------------------------------

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls?: string, text?: string): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function md(text: string): HTMLElement {
  const d = el('div', 'md');
  d.innerHTML = marked.parse(text) as string;
  for (const a of d.querySelectorAll('a')) a.setAttribute('target', '_blank');
  return d;
}

function fmtTime(ts: unknown): string {
  if (typeof ts !== 'string') return '';
  const d = new Date(ts);
  return isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + 'M';
  if (n >= 1_000) return Math.round(n / 1_000) + 'k';
  return String(n);
}

function fmtDuration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  return m < 60 ? `${m}m ${s % 60}s` : `${Math.floor(m / 60)}h ${m % 60}m`;
}

function shortModel(m?: string): string {
  if (!m) return '';
  const s = m.replace(/^claude-/, '').replace(/-\d{8}$/, '').replace(/\[.*\]$/, '');
  const parts = s.split('-');
  const fam = parts[0] ?? s;
  const ver = parts.slice(1).filter((p) => /^\d+$/.test(p)).join('.');
  return fam.charAt(0).toUpperCase() + fam.slice(1) + (ver ? ' ' + ver : '');
}

function summarizeInput(name: string, input: any): string {
  if (!input || typeof input !== 'object') return '';
  const first = (...k: string[]) => {
    for (const x of k) if (typeof input[x] === 'string' && input[x].trim()) return String(input[x]).trim();
    return '';
  };
  let s = '';
  switch (name) {
    case 'Bash':
      s = first('description', 'command');
      break;
    case 'Read':
    case 'Write':
    case 'Edit':
    case 'NotebookEdit':
      s = first('file_path', 'notebook_path');
      break;
    case 'Grep':
    case 'Glob':
      s = first('pattern');
      break;
    case 'Agent':
    case 'Task':
      s = first('description', 'prompt');
      break;
    case 'AskUserQuestion':
      s = input.questions?.[0]?.question ?? '';
      break;
    default:
      s = first('description', 'command', 'file_path', 'path', 'pattern', 'query', 'url', 'prompt');
  }
  s = s.replace(/\s+/g, ' ');
  return s.length > 140 ? s.slice(0, 137) + '…' : s;
}

const TOOL_ICON: Record<string, string> = {
  Bash: 'terminal',
  Read: 'go-to-file',
  Write: 'new-file',
  Edit: 'edit',
  NotebookEdit: 'notebook',
  Grep: 'search',
  Glob: 'search',
  Agent: 'hubot',
  Task: 'hubot',
  WebFetch: 'globe',
  WebSearch: 'globe',
  AskUserQuestion: 'question',
  ExitPlanMode: 'checklist',
  SendMessage: 'mail',
  Skill: 'extensions',
};
function iconFor(tool: string): HTMLElement {
  return el('i', `codicon codicon-${TOOL_ICON[tool] ?? 'symbol-method'}`);
}

/** Every event sits on the railway: a row with a coloured marker in the left margin. */
function rail(kind: string, node: HTMLElement, time?: string): HTMLElement {
  const row = el('div', `ev ev-${kind}`);
  const marker = el('span', 'marker');
  if (time) marker.title = time;
  row.appendChild(marker);
  row.appendChild(node);
  return row;
}

function turnFor(): HTMLElement {
  if (!currentTurn) {
    currentTurn = el('section', 'turn');
    turnsEl.appendChild(currentTurn);
  }
  return currentTurn;
}

// ---- transcript rendering -------------------------------------------------------------------------

function appendLocalEcho(text: string) {
  const bubble = el('div', 'bubble', text);
  const row = rail('user', bubble);
  row.classList.add('echo');
  row.dataset.echo = '1';
  turnFor().appendChild(row);
  afterAppend();
}

function clearEchoes() {
  for (const e of turnsEl.querySelectorAll('[data-echo]')) e.remove();
}

function startTurn(promptText: string, time: string, cross: boolean) {
  clearEchoes();
  const turn = el('section', 'turn');
  const sticky = el('div', 'turn-prompt');
  const bubble = el('div', 'bubble' + (cross ? ' cross' : ''));
  bubble.appendChild(md(promptText));
  const marker = el('span', 'marker');
  sticky.appendChild(marker);
  sticky.appendChild(bubble);
  sticky.appendChild(el('div', 'meta', (cross ? 'via message · ' : '') + time));
  sticky.onclick = () => sticky.classList.toggle('expanded');
  turn.appendChild(sticky);
  turnsEl.appendChild(turn);
  currentTurn = turn;
  stats.turns++;
}

function renderEntry(e: Entry) {
  const uuid = typeof e.uuid === 'string' ? e.uuid : undefined;
  if (uuid) {
    if (seen.has(uuid)) return;
    seen.add(uuid);
  }
  const type = e.type;
  const time = fmtTime(e.timestamp);
  const side = e.isSidechain === true;

  if (type === 'system') {
    const sub = e.subtype ?? 'system';
    if (sub === 'compact_boundary') {
      turnFor().appendChild(rail('system', el('div', 'divider', `Conversation compacted · ${time}`), time));
    } else if (sub === 'api_error' || e.level === 'error') {
      const msg = typeof e.content === 'string' ? e.content : e.error?.formatted ?? e.error?.message ?? 'error';
      turnFor().appendChild(rail('error', el('div', 'sys error', msg), time));
    }
    return;
  }
  if (type !== 'user' && type !== 'assistant') return;

  const msg = e.message ?? {};
  if (type === 'assistant' && msg.usage) {
    const u = msg.usage;
    stats.calls++;
    stats.lastInput = u.input_tokens ?? 0;
    stats.lastCacheRead = u.cache_read_input_tokens ?? 0;
    stats.lastCacheCreate = u.cache_creation_input_tokens ?? 0;
    stats.input += stats.lastInput;
    stats.cacheRead += stats.lastCacheRead;
    stats.cacheCreate += stats.lastCacheCreate;
    stats.output += u.output_tokens ?? 0;
  }
  const blocks: any[] = Array.isArray(msg.content) ? msg.content : typeof msg.content === 'string' ? [{ type: 'text', text: msg.content }] : [];

  for (const b of blocks) {
    switch (b.type) {
      case 'text': {
        const text = String(b.text ?? '');
        if (!text.trim()) break;
        if (type === 'user') {
          if (e.isMeta === true || side) break;
          const t = text.trim();
          const cross = /^<cross-session-message\b/.test(t);
          const clean = cross ? t.replace(/<\/?cross-session-message[^>]*>/g, '').trim() : t;
          startTurn(clean, time, cross);
        } else {
          const body = el('div', 'assistant');
          body.appendChild(md(text));
          const row = rail(side ? 'side' : 'assistant', body, time);
          if (side) row.appendChild(el('span', 'side-tag', 'subagent'));
          turnFor().appendChild(row);
        }
        break;
      }
      case 'thinking': {
        const t = String(b.thinking ?? '').trim();
        if (!t) break;
        const det = el('details', 'thinking');
        det.appendChild(el('summary', undefined, 'Thinking'));
        det.appendChild(md(t));
        turnFor().appendChild(rail(side ? 'side' : 'thinking', det, time));
        break;
      }
      case 'tool_use': {
        const name = String(b.name ?? 'tool');
        if (b.id) toolNames.set(b.id, name);
        stats.toolCalls++;
        if ((name === 'Agent' || name === 'Task') && b.id) {
          stats.subagents.set(b.id, { name: String(b.input?.subagent_type ?? b.input?.name ?? 'agent'), desc: String(b.input?.description ?? '').slice(0, 80), done: false, error: false, at: time });
        }
        const det = el('details', `tool tool-${name}`);
        const sum = el('summary');
        sum.appendChild(iconFor(name));
        sum.appendChild(el('span', 'tool-name', name));
        const s = summarizeInput(name, b.input);
        if (s) sum.appendChild(el('span', 'tool-summary', s));
        sum.appendChild(el('span', 'tool-status running', ''));
        det.appendChild(sum);
        const body = el('div', 'tool-body');
        if (name === 'Bash' && typeof b.input?.command === 'string') {
          body.appendChild(ioBlock('IN', b.input.command, 'in'));
        } else {
          const pre = el('pre', 'input');
          pre.textContent = JSON.stringify(b.input ?? {}, null, 2);
          body.appendChild(pre);
        }
        det.appendChild(body);
        if (b.id) toolRows.set(b.id, det);
        turnFor().appendChild(rail(side ? 'side' : 'tool', det, time));
        break;
      }
      case 'tool_result': {
        const id = String(b.tool_use_id ?? '');
        const row = toolRows.get(id);
        const raw = typeof b.content === 'string' ? b.content : Array.isArray(b.content) ? b.content.map((c: any) => c.text ?? '').join('\n') : '';
        const text = raw.trim();
        const sub = stats.subagents.get(id);
        if (sub) {
          sub.done = true;
          sub.error = !!b.is_error;
        }
        if (row) {
          if (b.is_error) row.parentElement?.classList.add('has-error');
          const status = row.querySelector('.tool-status');
          if (status) {
            status.className = 'tool-status ' + (b.is_error ? 'error' : 'done');
            status.innerHTML = b.is_error ? '<i class="codicon codicon-error"></i>' : '<i class="codicon codicon-check"></i>';
          }
          if (text) {
            const clipped = text.length > 6000 ? text.slice(0, 6000) + `\n… (${text.length - 6000} more chars)` : text;
            const isBash = row.classList.contains('tool-Bash');
            row.querySelector('.tool-body')!.appendChild(isBash ? ioBlock('OUT', clipped, b.is_error ? 'out error' : 'out') : outputBlock(clipped, !!b.is_error));
          }
        } else if (text && !side) {
          const det = el('details', 'tool');
          det.appendChild(el('summary', undefined, `${toolNames.get(id) ?? 'tool'} result`));
          det.appendChild(outputBlock(text.slice(0, 6000), !!b.is_error));
          turnFor().appendChild(rail('tool', det, time));
        }
        break;
      }
      default:
        break;
    }
  }
}

function ioBlock(label: string, text: string, cls: string): HTMLElement {
  const wrap = el('div', `io ${cls}`);
  wrap.appendChild(el('span', 'io-label', label));
  const pre = el('pre');
  pre.textContent = text;
  wrap.appendChild(pre);
  return wrap;
}

function outputBlock(text: string, isError: boolean): HTMLElement {
  const pre = el('pre', 'output' + (isError ? ' error' : ''));
  pre.textContent = text;
  return pre;
}

// ---- header / ticker / info -----------------------------------------------------------------------

function renderHeader() {
  if (!agent || !machine) return;
  const title = agent.title || agent.name || agent.sessionId.slice(0, 8);
  document.getElementById('title')!.textContent = title;
  const st = agent.alive ? agent.state : 'exited';
  const icon = document.getElementById('stateIcon')!;
  icon.className = `state-dot state-${st}`;
  icon.innerHTML = `<i class="codicon codicon-${STATE_ICON[st] ?? 'circle-outline'}"></i>`;
  const bits = [STATE_LABEL[st] ?? st];
  if (agent.stateDetail && st !== 'idle') bits.push(agent.stateDetail);
  const model = shortModel(agent.model || agent.managed?.model);
  const meta: string[] = [];
  if (model) meta.push(model + (agent.effort ? ` · ${agent.effort}` : ''));
  const mode = agent.permissionMode || agent.managed?.permissionMode;
  if (mode) meta.push(mode);
  if (agent.contextTokens) meta.push(`${fmtTokens(agent.contextTokens)} ctx`);
  meta.push(`${machine.name} · ${agent.workspacePath.split(/[\\/]/).filter(Boolean).pop() ?? ''}`);
  if (agent.managed) meta.push(agent.managed.exited ? 'managed · ended' : `managed${agent.managed.costUsd ? ` · $${agent.managed.costUsd.toFixed(2)}` : ''}`);
  document.getElementById('sub')!.textContent = `${bits.join(' — ')}   ·   ${meta.join(' · ')}`;

  const managedLive = !!agent.managed && !agent.managed.exited;
  btnStop.hidden = !(agent.alive && machine.online);
  btnStop.title = managedLive ? 'Stop this session' : 'Terminate this session';
  btnInterrupt.hidden = !managedLive;
  const canSend = machine.online && agent.alive;
  btnSend.disabled = !canSend || sending;
  input.disabled = !canSend;
  input.placeholder = !machine.online ? `${machine.name} is offline` : !agent.alive ? 'This session has exited' : managedLive ? 'Message this agent…  (Enter to send, Shift+Enter for newline)' : 'Message this agent…  (delivered as a cross-session message)';
  document.getElementById('hint')!.textContent = managedLive ? '' : agent.alive ? 'observed session' : '';

  controlsEl.classList.remove('busy');
  renderControls();
  renderTicker();
  renderCards();
  if (!infoEl.hidden) renderInfo();
}

function renderTicker() {
  if (tickerTimer) {
    clearInterval(tickerTimer);
    tickerTimer = undefined;
  }
  if (!agent || !agent.alive || !BUSY.has(agent.state)) {
    tickerEl.hidden = true;
    return;
  }
  const since = agent.lastActivityAt ?? Date.now();
  const label = agent.state === 'thinking' ? 'Thinking' : agent.state === 'tool' ? (agent.stateDetail ?? 'Running a tool') : 'Working';
  const paint = () => {
    tickerEl.innerHTML = `<span class="pulse"></span><span class="ticker-text">${escapeHtml(label)}</span><span class="ticker-time">${fmtDuration(Date.now() - since)}</span>`;
  };
  paint();
  tickerEl.hidden = false;
  tickerTimer = window.setInterval(paint, 1000);
  if (pinned) scrollToBottom();
}

function renderInfo() {
  if (!agent || !machine) return;
  const rows: [string, string][] = [];
  const model = agent.model || agent.managed?.model;
  if (model) rows.push(['Model', `${shortModel(model)}  (${model})`]);
  if (agent.effort) rows.push(['Effort', agent.effort]);
  const mode = agent.permissionMode || agent.managed?.permissionMode;
  if (mode) rows.push(['Permission mode', mode]);
  if (agent.contextTokens) rows.push(['Context', `${fmtTokens(agent.contextTokens)} tokens`]);
  if (stats.calls) {
    const lastTotal = stats.lastInput + stats.lastCacheRead + stats.lastCacheCreate;
    const hit = lastTotal ? Math.round((stats.lastCacheRead / lastTotal) * 100) : 0;
    rows.push(['Prompt cache (last call)', `${fmtTokens(stats.lastCacheRead)} read · ${fmtTokens(stats.lastCacheCreate)} written · ${fmtTokens(stats.lastInput)} uncached · ${hit}% hit`]);
    rows.push(['Tokens (loaded window)', `${fmtTokens(stats.cacheRead)} cache read · ${fmtTokens(stats.cacheCreate)} cache write · ${fmtTokens(stats.input)} input · ${fmtTokens(stats.output)} output over ${stats.calls} calls`]);
  }
  rows.push(['Activity (loaded window)', `${stats.turns} turn${stats.turns === 1 ? '' : 's'} · ${stats.toolCalls} tool call${stats.toolCalls === 1 ? '' : 's'}`]);
  if (agent.managed) {
    rows.push(['Managed by Vineyard', `${agent.managed.turns} completed turn${agent.managed.turns === 1 ? '' : 's'}${agent.managed.costUsd ? ` · $${agent.managed.costUsd.toFixed(3)}` : ''}${agent.managed.exited ? ' · ended' : ''}`]);
    if (agent.managed.lastError) rows.push(['Last error', agent.managed.lastError]);
  }
  if (agent.gitBranch) rows.push(['Branch', agent.gitBranch]);
  rows.push(['Session', `${agent.name ? agent.name + ' · ' : ''}${agent.sessionId}`]);
  if (agent.version) rows.push(['Claude Code', agent.version]);
  if (agent.startedAt) rows.push(['Uptime', fmtDuration(Date.now() - agent.startedAt)]);
  rows.push(['Machine', `${machine.name} · ${agent.workspacePath}`]);

  infoEl.innerHTML = '';
  const grid = el('div', 'info-grid');
  for (const [k, v] of rows) {
    grid.appendChild(el('div', 'k', k));
    grid.appendChild(el('div', 'v', v));
  }
  infoEl.appendChild(grid);

  const subs = [...stats.subagents.values()];
  const running = subs.filter((s) => !s.done).length;
  const head = el('div', 'info-sub-title', `Agents: ${subs.length} spawned${running ? ` · ${running} running` : ''}`);
  infoEl.appendChild(head);
  if (subs.length) {
    const map = el('div', 'agent-map');
    for (const s of subs.slice(-12)) {
      const chip = el('div', `agent-chip ${s.done ? (s.error ? 'error' : 'done') : 'running'}`);
      chip.appendChild(el('span', 'chip-dot'));
      chip.appendChild(el('span', 'chip-name', s.name));
      chip.appendChild(el('span', 'chip-desc', s.desc));
      chip.title = `${s.at} · ${s.done ? (s.error ? 'failed' : 'done') : 'running'}`;
      map.appendChild(chip);
    }
    infoEl.appendChild(map);
  }
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!);
}

// ---- cards ---------------------------------------------------------------------------------------

function renderCards() {
  cardsEl.innerHTML = '';
  if (!agent || !machine) return;
  const pending = agent.managed && !agent.managed.exited ? agent.managed.pending : undefined;
  if (pending) {
    cardsEl.appendChild(pending.toolName === 'AskUserQuestion' ? questionCard(pending) : permissionCard(pending));
    return;
  }
  const q = agent.pendingTools.find((t) => t.name === 'AskUserQuestion');
  if (q && agent.alive && agent.state === 'question') {
    cardsEl.appendChild(observedQuestionCard(q.summary ?? 'The agent asked a question'));
  } else if (agent.alive && agent.state === 'permission' && !agent.managed) {
    const c = el('div', 'card notice');
    c.appendChild(el('div', 'card-title', 'Waiting for permission in its own UI'));
    c.appendChild(el('div', 'card-text', `${agent.stateDetail ?? ''} — approve it in the Claude Code pane or terminal on ${machine.name}. Messages you send here are read once it continues.`));
    cardsEl.appendChild(c);
  }
}

function questionCard(p: Pending): HTMLElement {
  const card = el('div', 'card question');
  const qs: any[] = Array.isArray(p.input?.questions) ? p.input.questions : [];
  const answers: Record<string, string> = {};
  card.appendChild(el('div', 'card-title', qs.length > 1 ? `Claude has ${qs.length} questions` : 'Claude has a question'));
  for (const q of qs) {
    const wrap = el('div', 'q');
    if (q.header) wrap.appendChild(el('div', 'q-header', String(q.header)));
    wrap.appendChild(el('div', 'q-text', String(q.question ?? '')));
    const opts = el('div', 'options');
    for (const o of q.options ?? []) {
      const b = el('button', 'option');
      b.appendChild(el('span', 'opt-label', String(o.label ?? '')));
      if (o.description) b.appendChild(el('span', 'opt-desc', String(o.description)));
      b.onclick = () => {
        if (q.multiSelect) {
          b.classList.toggle('selected');
          answers[q.question] = [...opts.querySelectorAll('.option.selected .opt-label')].map((x) => x.textContent ?? '').join(', ');
        } else {
          for (const x of opts.querySelectorAll('.option')) x.classList.remove('selected');
          b.classList.add('selected');
          answers[q.question] = String(o.label ?? '');
          if (qs.length === 1) submit();
        }
      };
      opts.appendChild(b);
    }
    const other = el('input', 'other') as HTMLInputElement;
    other.placeholder = 'Other…';
    other.onkeydown = (ev) => {
      if (ev.key === 'Enter' && other.value.trim()) {
        answers[q.question] = other.value.trim();
        if (qs.length === 1) submit();
      }
    };
    other.oninput = () => {
      if (other.value.trim()) answers[q.question] = other.value.trim();
    };
    opts.appendChild(other);
    wrap.appendChild(opts);
    card.appendChild(wrap);
  }
  if (qs.length > 1) {
    const b = el('button', 'primary', 'Answer');
    b.onclick = submit;
    card.appendChild(b);
  }
  function submit() {
    if (!Object.keys(answers).length) return;
    card.classList.add('busy');
    vscode.postMessage({ type: 'respond', requestId: p.requestId, response: { behavior: 'allow', updatedInput: { ...(p.input ?? {}), answers } } });
  }
  return card;
}

function observedQuestionCard(summary: string): HTMLElement {
  const card = el('div', 'card question observed');
  card.appendChild(el('div', 'card-title', 'Claude asked a question in its own UI'));
  card.appendChild(el('div', 'card-text', summary));
  card.appendChild(el('div', 'card-hint', `Claude Code only accepts the answer in the pane or terminal where the session runs (${machine?.name}). Open the workspace there, or send a message below; it is read once the question is dismissed.`));
  const b = el('button', 'primary', `Open on ${machine?.name}`);
  b.onclick = () => vscode.postMessage({ type: 'openWorkspace' });
  card.appendChild(b);
  return card;
}

function permissionCard(p: Pending): HTMLElement {
  const card = el('div', 'card permission');
  const title = el('div', 'card-title');
  title.appendChild(iconFor(p.toolName));
  title.appendChild(el('span', undefined, ` ${p.displayName || p.toolName} wants to run`));
  card.appendChild(title);
  if (p.description) card.appendChild(el('div', 'card-text', p.description));
  if (p.toolName === 'Bash' && typeof p.input?.command === 'string') card.appendChild(ioBlock('IN', p.input.command, 'in'));
  else {
    const pre = el('pre', 'input');
    pre.textContent = JSON.stringify(p.input ?? {}, null, 2);
    card.appendChild(pre);
  }
  const row = el('div', 'btn-row');
  const allow = el('button', 'primary', 'Allow');
  allow.onclick = () => respond({ behavior: 'allow', updatedInput: p.input ?? {} });
  row.appendChild(allow);
  const sugg = Array.isArray(p.suggestions) ? p.suggestions : [];
  if (sugg.length) {
    const always = el('button', undefined, 'Allow and remember');
    always.title = 'Allow and add the suggested permission rule for this session';
    always.onclick = () => respond({ behavior: 'allow', updatedInput: p.input ?? {}, updatedPermissions: sugg });
    row.appendChild(always);
  }
  const deny = el('button', 'danger', 'Deny');
  deny.onclick = () => {
    const reason = (card.querySelector('.deny-reason') as HTMLInputElement | null)?.value.trim();
    respond({ behavior: 'deny', message: reason || 'The user denied this action in Vineyard.' });
  };
  row.appendChild(deny);
  card.appendChild(row);
  const reason = el('input', 'deny-reason') as HTMLInputElement;
  reason.placeholder = 'Optional: tell Claude what to do instead';
  card.appendChild(reason);
  function respond(response: unknown) {
    card.classList.add('busy');
    vscode.postMessage({ type: 'respond', requestId: p.requestId, response });
  }
  return card;
}

// ---- messages from the extension -----------------------------------------------------------------

window.addEventListener('message', (ev) => {
  const m = ev.data;
  switch (m.type) {
    case 'init':
    case 'agent':
      agent = m.agent;
      machine = m.machine;
      renderHeader();
      break;
    case 'entries':
      if (m.reset) resetLog();
      for (const e of m.entries) renderEntry(e);
      if (!infoEl.hidden) renderInfo();
      afterAppend();
      break;
    case 'status':
      banner.textContent = m.text;
      banner.className = `banner ${m.kind}`;
      banner.hidden = false;
      controlsEl.classList.remove('busy');
      if (m.kind !== 'error') setTimeout(() => (banner.hidden = true), 4000);
      break;
    case 'sending':
      sending = m.busy;
      btnSend.disabled = sending;
      if (!sending) clearEchoes();
      break;
  }
});

vscode.postMessage({ type: 'ready' });
