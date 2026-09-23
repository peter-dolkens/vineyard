/**
 * Background tasks on the extension side: shell commands a session left running (see the daemon's
 * claude/tasks.go). Helpers shared by the tree and the chat's Agent map.
 */

import type { BackgroundTask } from './model.ts';

export function taskActive(t: BackgroundTask): boolean {
  return t.state === 'running';
}

export function taskLabel(t: BackgroundTask): string {
  return t.description || (t.taskId ? `Task ${t.taskId}` : 'Background command');
}

/** "running", "completed", "failed"… as a word for a row. */
export function taskStateLabel(t: BackgroundTask): string {
  return t.state === 'running' ? 'Running' : t.state.charAt(0).toUpperCase() + t.state.slice(1);
}

/** How long it has run (still running) or ran (finished), in ms; 0 when the start is unknown. */
export function taskElapsed(t: BackgroundTask, now = Date.now()): number {
  if (!t.startedAt) return 0;
  return Math.max(0, (t.state === 'running' || !t.endedAt ? now : t.endedAt) - t.startedAt);
}

/** "45s", "10m 45s", "2h 05m": the Claude Code agent-map style, finer than format.ts's duration(). */
export function clock(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}
