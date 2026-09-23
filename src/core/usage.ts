/**
 * Account usage limits as Claude Code reports them (rate_limit_event on a managed session's output):
 * which windows exist, how to name them, when to warn, and how to say when they reset.
 */

import type { Usage } from './model.ts';

/** Claude Code's own names for the windows, in the order its Account & Usage panel lists them. */
export const WINDOW_LABEL: Record<string, string> = {
  five_hour: 'Session (5hr)',
  seven_day: 'Weekly (7 day)',
  seven_day_opus: 'Weekly Opus',
  seven_day_sonnet: 'Weekly Sonnet',
  seven_day_overage_included: 'Weekly Fable',
  overage: 'Usage credit',
};
const ORDER = Object.keys(WINDOW_LABEL);

/** The one-word name a banner uses: "session limit", "weekly limit", "weekly Fable limit". */
export const WINDOW_SHORT: Record<string, string> = {
  five_hour: 'session limit',
  seven_day: 'weekly limit',
  seven_day_opus: 'weekly Opus limit',
  seven_day_sonnet: 'weekly Sonnet limit',
  seven_day_overage_included: 'weekly Fable limit',
  overage: 'usage credit',
};

export interface UsageRow {
  key: string;
  label: string;
  /** Whole percent. */
  percent: number;
  resetsAt?: number;
}

/** Utilization is normally 0..1; a value above 1 is already a percentage. */
export function percentOf(utilization: number | undefined): number {
  if (!utilization) return 0;
  return Math.round(utilization > 1 ? utilization : utilization * 100);
}

/** Every window in the report, in panel order, with unknown windows last. */
export function usageRows(u: Usage | undefined): UsageRow[] {
  if (!u) return [];
  const rows: UsageRow[] = [];
  for (const [key, w] of Object.entries(u.windows ?? {})) {
    rows.push({ key, label: WINDOW_LABEL[key] ?? key, percent: percentOf(w.utilization), resetsAt: w.resetsAt });
  }
  if (!rows.length && u.rateLimitType) rows.push({ key: u.rateLimitType, label: WINDOW_LABEL[u.rateLimitType] ?? u.rateLimitType, percent: percentOf(u.utilization), resetsAt: u.resetsAt });
  return rows.sort((a, b) => (ORDER.indexOf(a.key) + 1 || 99) - (ORDER.indexOf(b.key) + 1 || 99));
}

/** Warn from 80 % of any window, or when Claude Code itself says warning or rejected. */
export const WARN_PERCENT = 80;

export interface UsageWarning {
  row: UsageRow;
  rejected: boolean;
  /** "You've used 93% of your session limit · resets in 2h" */
  text: string;
}

/**
 * The window worth shouting about, if any: the fullest one at or past WARN_PERCENT, or the one the
 * event names when Claude Code flagged it. Nothing when usage is comfortable.
 */
export function usageWarning(u: Usage | undefined, now = Date.now()): UsageWarning | undefined {
  if (!u) return undefined;
  const rows = usageRows(u);
  let row = rows.filter((r) => r.percent >= WARN_PERCENT).sort((a, b) => b.percent - a.percent)[0];
  const flagged = u.status === 'allowed_warning' || u.status === 'rejected';
  if (!row && flagged) row = rows.find((r) => r.key === u.rateLimitType) ?? rows[0];
  if (!row) return undefined;
  const rejected = u.status === 'rejected' || row.percent >= 100;
  const name = WINDOW_SHORT[row.key] ?? row.label;
  const reset = row.resetsAt ? ` · resets in ${resetsIn(row.resetsAt, now)}` : '';
  const text = rejected ? `You've hit your ${name}${reset}` : `You've used ${row.percent}% of your ${name}${reset}`;
  return { row, rejected, text };
}

/**
 * What a dismissed banner is remembered by, as the Claude Code pane does it: the window, its reset
 * time and whether it was hit. Dismissing "used 85%" keeps the banner hidden while that window fills
 * further, but it returns when the window is hit, and again once the window has reset.
 */
export function usageWarningKey(w: UsageWarning): string {
  return `${w.rejected ? 'hit' : 'warn'}:${w.row.key}:${w.row.resetsAt ?? ''}`;
}

/** "2h", "45m", "6d", "now". */
export function resetsIn(resetsAt: number, now = Date.now()): string {
  const ms = resetsAt - now;
  if (ms <= 0) return 'now';
  const m = Math.round(ms / 60_000);
  if (m < 60) return `${Math.max(1, m)}m`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h`;
  return `${Math.round(h / 24)}d`;
}
