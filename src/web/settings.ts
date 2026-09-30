/**
 * The web app's settings, kept in this browser's localStorage: the extension's view and chat settings
 * that make sense on a phone, plus remembered per-workspace session choices (SessionPrefStore, the
 * same class the extension keeps in VS Code's global state).
 */

import { SessionPrefStore } from '../extension/sessionPrefs.ts';
import type { ViewPrefs } from './store.ts';

export interface Settings extends ViewPrefs {
  /** vineyard.chat.observed: what opening the chat of a session Vineyard did not start does. */
  observed: 'observe' | 'takeOver';
  /** vineyard.spawn.defaultPermissionMode; '' passes no mode. */
  defaultPermissionMode: string;
  transcriptLines: number;
  /** Pop a banner in the app when an agent starts waiting for you. */
  notifyAttention: boolean;
  /** Experimental: the Claude account picker on each machine (vineyard.experimental.claudeAccounts). */
  claudeAccounts: boolean;
}

export const DEFAULTS: Settings = {
  showHistorical: true,
  showExited: false,
  showFinishedSubagents: true,
  hideUnseenDays: 30,
  sortMachines: 'status',
  sortWorkspaces: 'name',
  sortAgents: 'recent',
  observed: 'observe',
  defaultPermissionMode: '',
  transcriptLines: 400,
  notifyAttention: true,
  claudeAccounts: false,
};

const KEY = 'vineyard.web.settings';

function read(key: string): unknown {
  try {
    const raw = localStorage.getItem(key);
    return raw ? JSON.parse(raw) : undefined;
  } catch {
    return undefined; // private mode, storage blocked or a corrupt value: use defaults
  }
}

function write(key: string, value: unknown): void {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    /* storage full or blocked: the setting lasts until the page closes */
  }
}

let current: Settings = { ...DEFAULTS, ...((read(KEY) as Partial<Settings>) ?? {}) };
const listeners: (() => void)[] = [];

export function settings(): Settings {
  return current;
}

export function updateSettings(patch: Partial<Settings>): void {
  current = { ...current, ...patch };
  write(KEY, current);
  for (const fn of listeners) fn();
}

export function onSettingsChange(fn: () => void): void {
  listeners.push(fn);
}

/** localStorage as the Memento SessionPrefStore expects. */
const memento = {
  get<T>(key: string, fallback: T): T {
    return (read(key) as T | undefined) ?? fallback;
  },
  async update(key: string, value: unknown): Promise<void> {
    write(key, value);
  },
};

export const sessionPrefs = new SessionPrefStore(memento as unknown as ConstructorParameters<typeof SessionPrefStore>[0]);
