/**
 * Remembers the model and reasoning effort last chosen for each workspace, so a new session there
 * starts the way the previous one was left rather than on Claude Code's defaults. Keyed by machine
 * and workspace path: the same repo checked out on two machines is two workspaces.
 */

import type * as vscode from 'vscode';

const KEY = 'vineyard.sessionPrefs';

export interface SessionPrefs {
  model?: string;
  effort?: string;
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
   * as having no memory, so the field is dropped rather than stored.
   */
  async remember(machineId: string, cwd: string, patch: SessionPrefs): Promise<void> {
    const all = { ...this.all() };
    const k = keyOf(machineId, cwd);
    const next: SessionPrefs = { ...all[k] };
    for (const field of ['model', 'effort'] as const) {
      if (patch[field] === undefined) continue;
      if (patch[field]) next[field] = patch[field];
      else delete next[field];
    }
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
