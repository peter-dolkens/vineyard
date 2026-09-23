/**
 * Remembers the model, reasoning effort and permission mode last chosen for each workspace, so a new
 * session there starts the way the previous one was left rather than on Claude Code's defaults. Keyed
 * by machine and workspace path: the same repo checked out on two machines is two workspaces.
 *
 * Each choice is stored with its basis: the Claude Code settings default in force when it was made,
 * as the daemon reported it. When that default later changes, the daemon drops the choice at spawn,
 * so a new global default is not shadowed forever by one old pick in one workspace.
 */

import type * as vscode from 'vscode';

const KEY = 'vineyard.sessionPrefs';

export const PREF_FIELDS = ['model', 'effort', 'permissionMode'] as const;
export type PrefField = (typeof PREF_FIELDS)[number];

/**
 * Modes that belong to one task rather than to the workspace: plan mode ends when a plan is approved
 * (Claude Code switches on its own, unseen here), and bypass is confirmed per session. Neither is
 * carried into the next session.
 */
const TRANSIENT_MODES = new Set(['plan', 'bypassPermissions']);

/** What a new session gets from the machine's Claude Code settings when no flag is passed ("" = built-in default). */
export type SettingsDefaults = Record<PrefField, string>;

export type SessionChoice = Partial<Record<PrefField, string>>;

export interface SessionPrefs extends SessionChoice {
  /** Per remembered field, the settings default when it was chosen. Missing for choices made before this was tracked. */
  basis?: Partial<Record<PrefField, string>>;
}

export class SessionPrefStore {
  private readonly state: vscode.Memento;

  // Assigned in the body rather than as a parameter property so Node's strip-only TS loader can run the tests.
  constructor(state: vscode.Memento) {
    this.state = state;
  }

  /** What a new session in this workspace should start with; empty when nothing was ever chosen. */
  get(machineId: string, cwd: string): SessionPrefs {
    return this.all()[keyOf(machineId, cwd)] ?? {};
  }

  /**
   * Record a choice made in a live session. An empty string means "the default", which is the same
   * as having no memory, so the field is dropped rather than stored. `defaults` is what the daemon
   * reported the settings defaults to be; without it (an older daemon) the choice has no basis and
   * is kept until changed.
   */
  async remember(machineId: string, cwd: string, patch: SessionChoice, defaults?: SettingsDefaults): Promise<void> {
    await this.edit(machineId, cwd, (next) => {
      for (const field of PREF_FIELDS) {
        const value = patch[field];
        if (value === undefined) continue;
        if (field === 'permissionMode' && TRANSIENT_MODES.has(value)) continue;
        if (value) {
          next[field] = value;
          if (defaults) next.basis = { ...next.basis, [field]: defaults[field] ?? '' };
          else if (next.basis) delete next.basis[field];
        } else {
          delete next[field];
          if (next.basis) delete next.basis[field];
        }
      }
    });
  }

  /**
   * After a spawn: forget the choices the daemon found stale, and give any remembered choice that has
   * no basis yet today's defaults, so it too gives way when they next change.
   */
  async reconcile(machineId: string, cwd: string, stale: readonly string[], defaults: SettingsDefaults): Promise<void> {
    await this.edit(machineId, cwd, (next) => {
      for (const field of PREF_FIELDS) {
        if (stale.includes(field)) {
          delete next[field];
          if (next.basis) delete next.basis[field];
        } else if (next[field] && next.basis?.[field] === undefined) {
          next.basis = { ...next.basis, [field]: defaults[field] ?? '' };
        }
      }
    });
  }

  private async edit(machineId: string, cwd: string, change: (next: SessionPrefs) => void): Promise<void> {
    const all = { ...this.all() };
    const k = keyOf(machineId, cwd);
    const next: SessionPrefs = { ...all[k], basis: all[k]?.basis && { ...all[k].basis } };
    change(next);
    if (next.basis && !Object.keys(next.basis).length) delete next.basis;
    if (!next.basis) delete next.basis;
    if (Object.keys(next).length) all[k] = next;
    else delete all[k];
    await this.state.update(KEY, all);
  }

  private all(): Record<string, SessionPrefs> {
    return this.state.get<Record<string, SessionPrefs>>(KEY, {});
  }
}

function keyOf(machineId: string, cwd: string): string {
  return `${machineId}::${cwd}`;
}
