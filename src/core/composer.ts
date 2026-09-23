/**
 * The DOM-free half of the chat composer's toolbar and "/" actions menu: what the context ring and
 * cache dot mean, the permission-mode and effort catalogues, and which actions apply to a session in
 * its current state. src/webview/composer.ts renders these; test/composer.test.ts checks them.
 */

import type { CommandInfo } from './model.ts';

// ---- context window ------------------------------------------------------------------------------

/** Tokens the model can hold: 1M for ids with the "[1m]" suffix, 200k for everything else. */
export function contextWindow(model: string | undefined): number {
  return /\[1m\]/i.test(model ?? '') ? 1_000_000 : 200_000;
}

export interface ContextGauge {
  /** 0..1 share of the window in use. */
  fraction: number;
  /** Whole percent, for the label. */
  percent: number;
  /** ok under 60 %, warn under 85 %, hot from there (Claude Code compacts on its own soon after). */
  level: 'ok' | 'warn' | 'hot';
}

export function contextGauge(tokens: number | undefined, model: string | undefined): ContextGauge {
  const window = contextWindow(model);
  const fraction = Math.min(1, Math.max(0, (tokens ?? 0) / window));
  const percent = Math.round(fraction * 100);
  return { fraction, percent, level: percent < 60 ? 'ok' : percent < 85 ? 'warn' : 'hot' };
}

// ---- prompt cache --------------------------------------------------------------------------------

/** Anthropic's default prompt-cache lifetime; the cached prefix is gone this long after the last call. */
export const CACHE_TTL_MS = 5 * 60 * 1000;

export interface CacheState {
  state: 'none' | 'warm' | 'cold';
  /** Share of the last call's input that was read from cache, whole percent. */
  hitPercent: number;
}

/**
 * How warm the session's prompt cache is: the last call's hit rate, and whether the cached prefix is
 * still likely to be there (another call within the TTL). No calls seen yet is 'none'.
 */
export function cacheState(last: { cacheRead: number; cacheCreate: number; input: number; calls: number }, lastActivityAt: number | undefined, now = Date.now()): CacheState {
  if (!last.calls) return { state: 'none', hitPercent: 0 };
  const total = last.cacheRead + last.cacheCreate + last.input;
  const hitPercent = total ? Math.round((last.cacheRead / total) * 100) : 0;
  const expired = !lastActivityAt || now - lastActivityAt > CACHE_TTL_MS;
  return { state: expired ? 'cold' : 'warm', hitPercent };
}

// ---- permission modes and effort -----------------------------------------------------------------

export interface ModeInfo {
  value: string;
  label: string;
  description: string;
  icon: string; // codicon name
}

/** In the order the Claude Code pane lists them, with its wording. */
export const MODES: ModeInfo[] = [
  { value: 'default', label: 'Manual', description: 'Claude asks for approval before each edit and command.', icon: 'hand' },
  { value: 'acceptEdits', label: 'Edit automatically', description: 'Claude edits files without asking; commands still ask.', icon: 'edit' },
  { value: 'plan', label: 'Plan', description: 'Claude explores the code and presents a plan before editing.', icon: 'checklist' },
  { value: 'auto', label: 'Auto', description: 'Claude approves actions that pass a safety check and pauses for anything risky.', icon: 'zap' },
  { value: 'bypassPermissions', label: 'Bypass permissions', description: 'Nothing is asked. Every tool runs.', icon: 'unlock' },
];

export function modeInfo(value: string | undefined): ModeInfo {
  return MODES.find((m) => m.value === value) ?? { value: value || 'default', label: value || 'Manual', description: '', icon: 'shield' };
}

export const EFFORT_LEVELS = ['low', 'medium', 'high', 'xhigh', 'max'] as const;
export const EFFORT_LABEL: Record<string, string> = { low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra high', max: 'Max' };

export function effortLabel(value: string | undefined): string {
  return value ? (EFFORT_LABEL[value] ?? value) : 'Default';
}

// ---- "/" actions ---------------------------------------------------------------------------------

export interface Action {
  id: string;
  label: string;
  group: string;
  /** Second line, or the tooltip. */
  detail?: string;
  /** Right-aligned current value ("Fable 5.1"). */
  value?: string;
  /** How the row renders: a plain row, or the effort slider. */
  kind?: 'item' | 'effort';
  /** Extra words the filter should match (a slash command's description, "@" aliases…). */
  keywords?: string;
  /** Shown but not runnable, with detail saying why. */
  disabled?: boolean;
}

export interface ActionContext {
  online: boolean;
  alive: boolean;
  /** A managed session that is still running: the only kind that takes settings and slash commands. */
  managedLive: boolean;
  /** An Agent-tool thread viewed on its own; read-only. */
  subagent: boolean;
  busy: boolean;
  machineName: string;
  modelLabel: string;
  effort: string | undefined;
  modeLabel: string;
  commands?: CommandInfo[];
  account?: string;
  extVersion?: string;
  claudeVersion?: string;
}

export const GROUP = { context: 'Context', model: 'Model', session: 'Session', commands: 'Slash commands', settings: 'Settings', support: 'Support' } as const;

/** Every action the menu can show for this session, in display order, with reasons where one is off. */
export function buildActions(c: ActionContext): Action[] {
  const canSend = c.online && c.alive && !c.subagent;
  const onlyManaged = 'Only sessions started by Vineyard take this from here.';
  const notManaged = canSend && !c.managedLive ? onlyManaged : undefined;
  const offline = !c.online ? `${c.machineName} is offline.` : undefined;
  const acts: Action[] = [
    { id: 'attach', group: GROUP.context, label: 'Attach file…', detail: canSend ? 'Images go as images to a Vineyard-started session; other files are inlined as text.' : offline ?? 'This session cannot take messages.', disabled: !canSend },
    { id: 'compact', group: GROUP.context, label: 'Compact conversation', detail: notManaged ?? 'Runs /compact: keeps a summary, frees the context window.', disabled: !c.managedLive },
    { id: 'clear', group: GROUP.context, label: 'Clear conversation', detail: notManaged ?? 'Runs /clear: drops the history entirely. Asks first.', disabled: !c.managedLive },
    { id: 'reload', group: GROUP.context, label: 'Reload transcript', detail: 'Re-read the transcript from the machine.' },
    { id: 'info', group: GROUP.context, label: 'Session info', detail: 'Model, tokens, cache, subagents, uptime.' },

    { id: 'model', group: GROUP.model, label: 'Switch model…', value: c.modelLabel || undefined, detail: notManaged, disabled: !c.managedLive },
    { id: 'effort', group: GROUP.model, label: 'Effort', value: effortLabel(c.effort), kind: 'effort', detail: notManaged, disabled: !c.managedLive },
    { id: 'mode', group: GROUP.model, label: 'Permission mode…', value: c.modeLabel, detail: notManaged, disabled: !c.managedLive },

    { id: 'rename', group: GROUP.session, label: 'Rename session…', disabled: !c.online || c.subagent },
    { id: 'interrupt', group: GROUP.session, label: 'Interrupt current turn', detail: notManaged ?? (c.busy ? 'Like pressing Escape in the session.' : 'The session is not mid-turn.'), disabled: !c.managedLive || !c.busy },
    { id: 'stop', group: GROUP.session, label: c.managedLive ? 'Stop session' : 'Terminate session', detail: c.managedLive ? 'Ends cleanly; resumable later.' : 'Kills the Claude Code process; the transcript stays.', disabled: !c.online || !c.alive || c.subagent },
    { id: 'copySessionId', group: GROUP.session, label: 'Copy session ID' },
    { id: 'openWorkspace', group: GROUP.session, label: 'Open workspace in VS Code' },
    { id: 'openTerminal', group: GROUP.session, label: `Open terminal on ${c.machineName}` },
    { id: 'resumeTerminal', group: GROUP.session, label: 'Resume in a terminal', detail: 'claude --resume in a terminal on that machine.', disabled: c.subagent },
    { id: 'rawTranscript', group: GROUP.session, label: 'Show raw transcript' },
  ];
  if (c.managedLive && c.commands?.length) {
    for (const cmd of c.commands) {
      acts.push({ id: `slash:${cmd.name}`, group: GROUP.commands, label: `/${cmd.name}`, detail: cmd.description || undefined, value: cmd.argumentHint || undefined, keywords: cmd.description });
    }
  }
  acts.push(
    { id: 'login', group: GROUP.settings, label: `Sign in to Claude on ${c.machineName}…`, detail: c.account ? `Signed in as ${c.account}` : 'Runs claude auth login there; the browser opens here.', disabled: !c.online },
    { id: 'settings', group: GROUP.settings, label: 'Vineyard settings' },
    { id: 'help', group: GROUP.support, label: 'View help docs' },
    { id: 'report', group: GROUP.support, label: 'Report a problem', value: [c.extVersion && `Vineyard ${c.extVersion}`, c.claudeVersion && `Claude Code ${c.claudeVersion}`].filter(Boolean).join(' · ') || undefined },
  );
  return acts;
}

/**
 * Case-insensitive word match over label, detail, keywords, id and value; "/" prefixes are ignored.
 * Rows whose label starts with the query come first (so "/code" puts /code-review above "Open
 * workspace in VS Code"), then rows where a word in the label starts with it, then the rest; ties
 * keep display order.
 */
export function filterActions(actions: Action[], query: string): Action[] {
  const q = query.trim().replace(/^\/+/, '').toLowerCase();
  if (!q) return actions;
  const words = q.split(/\s+/);
  const wordStart = new RegExp(`(^|[^a-z0-9])${q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`);
  const rank = (a: Action) => {
    const label = a.label.replace(/^\//, '').toLowerCase();
    return label.startsWith(q) ? 0 : wordStart.test(label) ? 1 : 2;
  };
  return actions
    .map((a, i) => ({ a, i, r: rank(a) }))
    .filter(({ a }) => {
      const hay = `${a.label} ${a.detail ?? ''} ${a.keywords ?? ''} ${a.id} ${a.value ?? ''}`.toLowerCase();
      return words.every((w) => hay.includes(w));
    })
    .sort((x, y) => x.r - y.r || x.i - y.i)
    .map(({ a }) => a);
}

/** Group actions in first-seen order for rendering with headings. */
export function groupActions(actions: Action[]): { group: string; actions: Action[] }[] {
  const out: { group: string; actions: Action[] }[] = [];
  for (const a of actions) {
    const g = out.find((x) => x.group === a.group);
    if (g) g.actions.push(a);
    else out.push({ group: a.group, actions: [a] });
  }
  return out;
}
