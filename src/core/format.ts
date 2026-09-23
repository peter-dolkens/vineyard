import type { Agent, AgentState, Subagent } from './model.ts';

export function relativeTime(epochMs: number | undefined, now = Date.now()): string {
  if (!epochMs) return '';
  const s = Math.max(0, Math.round((now - epochMs) / 1000));
  if (s < 5) return 'just now';
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h ago`;
  const d = Math.round(h / 24);
  return `${d}d ago`;
}

export function duration(ms: number): string {
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

export function tokens(n: number | undefined): string {
  if (!n) return '';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1_000)}k`;
  return String(n);
}

/** "claude-fable-5-1" → "Fable 5.1"; "claude-opus-4-1-20250805" → "Opus 4.1". */
export function shortModel(model: string | undefined): string {
  if (!model) return '';
  const m = model.replace(/^claude-/, '').replace(/-\d{8}$/, '').replace(/\[.*\]$/, '');
  const parts = m.split('-');
  const family = parts[0] ?? m;
  const version = parts.slice(1).filter((p) => /^\d+$/.test(p)).join('.');
  const name = family.charAt(0).toUpperCase() + family.slice(1);
  return version ? `${name} ${version}` : name;
}

export const STATE_LABEL: Record<AgentState, string> = {
  question: 'Asking you a question',
  permission: 'Waiting for permission',
  working: 'Working',
  thinking: 'Thinking',
  tool: 'Running a tool',
  shell: 'In a shell',
  idle: 'Idle',
  done: 'Finished',
  exited: 'Exited',
  unknown: 'Unknown',
};

export function agentLabel(agent: Agent): string {
  return agent.title ?? agent.name ?? agent.sessionId.slice(0, 8);
}

export function subagentLabel(sub: Subagent): string {
  return sub.description || sub.type || sub.agentId.slice(0, 8);
}

export function basename(p: string): string {
  const parts = p.replace(/[\\/]+$/, '').split(/[\\/]/);
  return parts[parts.length - 1] || p;
}

/** Shorten "/Users/peter/Projects/x" to "~/Projects/x" when the home dir is known. */
export function tildify(p: string, home: string | undefined): string {
  if (home && p.startsWith(home)) return '~' + p.slice(home.length);
  return p;
}
