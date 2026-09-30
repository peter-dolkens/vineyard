/**
 * The Claude account picker (experimental, behind vineyard.experimental.claudeAccounts): the labels a
 * machine's daemon reports for the accounts it keeps signed in, and how to show them. Tokens never
 * leave that machine; the `accounts` op only carries what is here.
 */

import type { Usage } from './model.ts';
import { planLabel } from './format.ts';
import { usageRows } from './usage.ts';

export interface ClaudeAccount {
  /** accountUuid:organizationUuid */
  key: string;
  email?: string;
  name?: string;
  organization?: string;
  /** Claude Code's subscriptionType: pro, max, team, enterprise. */
  plan?: string;
  active?: boolean;
  savedAt?: number;
  /** When its sign-in lapses unless Claude refreshes it before then (epoch ms). */
  refreshExpiresAt?: number;
  /** The last limit report seen while it was signed in. */
  usage?: Usage;
}

export interface AccountsReply {
  accounts: ClaudeAccount[];
}

/** The kinds of account "Add account" offers. Only subscriptions for now. */
export const ACCOUNT_TYPES = [{ type: 'subscription', label: 'Claude subscription', detail: 'claude.ai account (Pro / Max / Team)' }] as const;

export function accountTitle(a: ClaudeAccount): string {
  return a.email || a.name || a.key.slice(0, 8);
}

/** "Claude Max" from "max". */
export function accountPlan(a: ClaudeAccount): string {
  const p = a.plan?.trim();
  return p ? planLabel(p.charAt(0).toUpperCase() + p.slice(1)) : '';
}

const DAY = 86_400_000;

/** Whether the account can still be switched to without signing in again. */
export function accountExpired(a: ClaudeAccount, now = Date.now()): boolean {
  return !!a.refreshExpiresAt && a.refreshExpiresAt < now;
}

/** "Sign-in expired" / "sign-in lapses in 3 days", only once it is close enough to matter. */
export function accountExpiry(a: ClaudeAccount, now = Date.now()): string {
  if (!a.refreshExpiresAt || a.active) return '';
  const left = a.refreshExpiresAt - now;
  if (left <= 0) return 'sign-in expired';
  if (left > 7 * DAY) return '';
  const days = Math.floor(left / DAY);
  return days < 1 ? 'sign-in lapses today' : `sign-in lapses in ${days} day${days === 1 ? '' : 's'}`;
}

/** "5hr 40% · 7d 12%" from the last report seen for it. */
export function accountUsage(a: ClaudeAccount): string {
  const short: Record<string, string> = { five_hour: '5hr', seven_day: '7d' };
  return usageRows(a.usage)
    .filter((r) => short[r.key])
    .map((r) => `${short[r.key]} ${r.percent}%`)
    .join(' · ');
}

/** The picker's second line: organization, plan, last usage, expiry. */
export function accountDetail(a: ClaudeAccount, now = Date.now()): string {
  return [a.organization, accountPlan(a), accountUsage(a), accountExpiry(a, now)].filter(Boolean).join(' · ');
}
