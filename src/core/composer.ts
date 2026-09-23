/**
 * The DOM-free half of the chat composer's toolbar and "/" actions menu: what the context ring and
 * cache clock mean, the permission-mode and effort catalogues, and which actions apply to a session in
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
  /** Whole percent, for the tooltip. */
  percent: number;
  /** ok under 60 %, warn under 85 %, hot from there (Claude Code compacts on its own soon after). */
  level: 'ok' | 'warn' | 'hot';
}

/** Share of a window of `window` tokens in use; a missing or zero window falls back to 200k. */
export function contextGauge(tokens: number | undefined, window: number | undefined): ContextGauge {
  const w = window && window > 0 ? window : contextWindow(undefined);
  const fraction = Math.min(1, Math.max(0, (tokens ?? 0) / w));
  const percent = Math.round(fraction * 100);
  return { fraction, percent, level: percent < 60 ? 'ok' : percent < 85 ? 'warn' : 'hot' };
}

/** The fields of an agent the ring reads. */
export interface ContextSource {
  contextTokens?: number;
  contextWindow?: number;
  managed?: { exited: boolean; compacting?: boolean };
}

export interface ContextView {
  /** Tokens in the window: the last API call's input as the transcript shows it, or Claude Code's own measurement when that is newer (the daemon picks). */
  tokens: number;
  window: number;
  /** True when Claude Code reported the window's size itself (get_context_usage); false when it is inferred from the model id. */
  measured: boolean;
  /** A compaction is running (managed sessions only): the count is about to drop. */
  compacting: boolean;
}

/**
 * What the ring shows for a session. Nothing here asks Claude Code anything: the daemon merges the
 * harness's measurement into contextTokens / contextWindow when it has one, so the transcript's
 * per-call usage carries the ring between the few check-ins (handshake, model switch, compaction).
 */
export function contextFor(a: ContextSource | undefined, wireModel: string | undefined): ContextView {
  const measured = !!a?.contextWindow && a.contextWindow > 0;
  return {
    tokens: a?.contextTokens ?? 0,
    window: measured ? a!.contextWindow! : contextWindow(wireModel),
    measured,
    compacting: !!a?.managed && !a.managed.exited && !!a.managed.compacting,
  };
}

// ---- prompt cache --------------------------------------------------------------------------------

/**
 * Anthropic's prompt-cache lifetimes. Every API call that reads or writes the cache restarts the
 * clock; usage.cache_creation's breakdown says which lifetime the call bought.
 */
export const CACHE_TTL_5M_MS = 5 * 60 * 1000;
export const CACHE_TTL_1H_MS = 60 * 60 * 1000;

/** The cache-relevant part of an assistant message's usage, as the transcript records it. */
export interface CacheUsage {
  input_tokens?: number;
  cache_read_input_tokens?: number;
  cache_creation_input_tokens?: number;
  cache_creation?: { ephemeral_5m_input_tokens?: number; ephemeral_1h_input_tokens?: number };
}

export interface CacheClockInput {
  /** Usage of the last main-thread assistant message; none when the loaded window holds no call. */
  usage?: CacheUsage;
  /** When the request behind that message went out (its user or tool-result line), epoch ms. */
  requestAt?: number;
  /** That assistant line's timestamp, epoch ms. */
  respondedAt?: number;
  /** The last compact_boundary (or compact summary) timestamp, epoch ms. */
  compactedAt?: number;
  /** The lifetime carried over from earlier calls, for a call whose breakdown created nothing. */
  ttlMs?: number;
}

export interface CacheClock {
  /**
   * warm: the countdown runs; expired: it ran out, the next call re-caches; compacted: a compaction
   * came after the last call, so the cache does not cover the conversation yet; unknown: no call that
   * touched the cache has been seen.
   */
  state: 'warm' | 'expired' | 'compacted' | 'unknown';
  ttlMs: number;
  /** When the countdown started: the request time (the pane's anchor), else the response time. */
  anchorAt?: number;
  expiresAt?: number;
  remainingMs: number;
  /** Share of the last call's input read from cache, whole percent; none when it saw no tokens. */
  hitRate?: number;
  /** Minutes left, rounded up, as the pane prints it ("12m"); empty unless warm. */
  label: string;
}

/**
 * Which lifetime a call bought: 1 h when the 1h breakdown carries tokens, 5 min when the 5m one does,
 * nothing when the call created no cache (the caller then keeps the previous lifetime).
 */
export function cacheTtlMs(usage: CacheUsage | undefined): number | undefined {
  const c = usage?.cache_creation;
  if (!c) return undefined;
  if ((c.ephemeral_1h_input_tokens ?? 0) > 0) return CACHE_TTL_1H_MS;
  if ((c.ephemeral_5m_input_tokens ?? 0) > 0) return CACHE_TTL_5M_MS;
  return undefined;
}

/**
 * The prompt-cache clock as the Claude Code pane computes it: a call whose usage read or wrote the
 * cache restarts a countdown of the cache's lifetime from the moment the request went out; when it
 * runs out the cache has likely expired; a compaction after the last call leaves the cache useless
 * until the next response. Pure, so the webview can repaint it every second.
 */
export function cacheClock(input: CacheClockInput, now = Date.now()): CacheClock {
  const { usage, compactedAt, requestAt, respondedAt } = input;
  const ttlMs = cacheTtlMs(usage) ?? input.ttlMs ?? CACHE_TTL_5M_MS;
  const read = usage?.cache_read_input_tokens ?? 0;
  const create = usage?.cache_creation_input_tokens ?? 0;
  const total = (usage?.input_tokens ?? 0) + read + create;
  const hitRate = total > 0 ? Math.round((read / total) * 100) : undefined;
  if (compactedAt !== undefined && (respondedAt === undefined || respondedAt <= compactedAt)) {
    return { state: 'compacted', ttlMs, remainingMs: 0, hitRate, label: '' };
  }
  const anchorAt = requestAt !== undefined && (respondedAt === undefined || requestAt <= respondedAt) ? requestAt : respondedAt;
  if (!usage || read + create <= 0 || anchorAt === undefined) return { state: 'unknown', ttlMs, remainingMs: 0, hitRate, label: '' };
  const expiresAt = anchorAt + ttlMs;
  const remainingMs = Math.min(ttlMs, expiresAt - now);
  if (remainingMs > 0) return { state: 'warm', ttlMs, anchorAt, expiresAt, remainingMs, hitRate, label: `${Math.ceil(remainingMs / 60_000)}m` };
  return { state: 'expired', ttlMs, anchorAt, expiresAt, remainingMs: 0, hitRate, label: '' };
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
  /** "93% of session limit", for the Account & usage row's value. */
  usageSummary?: string;
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
    { id: 'usage', group: GROUP.settings, label: 'Account & usage…', value: c.usageSummary, detail: c.usageSummary ? undefined : 'Limits appear once a session started by Vineyard has made a request.' },
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
