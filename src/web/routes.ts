/**
 * The web app's screens as hash routes, so every screen has a URL (reload, back, a link from a toast)
 * and each knows the screen above it for a deep link opened cold.
 *
 *   /                          machines, and agents waiting for you
 *   /m/<machine>               a machine: its workspaces and actions
 *   /m/<machine>/w/<path>      a workspace: its agents
 *   /m/<machine>/history[?cwd] past sessions to resume
 *   /m/<machine>/log           the daemon log (this machine only)
 *   /a/<agent>                 an agent's chat
 *   /a/<agent>/info            an agent's details, subagents and background tasks
 *   /settings
 */

export type Route =
  | { kind: 'home' }
  | { kind: 'machine'; machine: string }
  | { kind: 'workspace'; machine: string; path: string }
  | { kind: 'history'; machine: string; cwd?: string }
  | { kind: 'log'; machine: string }
  | { kind: 'chat'; agent: string }
  | { kind: 'agent'; agent: string }
  | { kind: 'settings' };

const enc = encodeURIComponent;

export const R = {
  home: '/',
  machine: (id: string) => `/m/${enc(id)}`,
  workspace: (id: string, path: string) => `/m/${enc(id)}/w/${enc(path)}`,
  history: (id: string, cwd?: string) => `/m/${enc(id)}/history${cwd ? `?cwd=${enc(cwd)}` : ''}`,
  log: (id: string) => `/m/${enc(id)}/log`,
  chat: (agentId: string) => `/a/${enc(agentId)}`,
  agent: (agentId: string) => `/a/${enc(agentId)}/info`,
  settings: '/settings',
};

function dec(s: string | undefined): string {
  try {
    return decodeURIComponent(s ?? '');
  } catch {
    return s ?? '';
  }
}

export function parseRoute(route: string): Route {
  const [path = '', query = ''] = route.split('?', 2);
  const parts = path.split('/').filter(Boolean);
  const q = new URLSearchParams(query);
  if (parts[0] === 'm' && parts[1]) {
    const machine = dec(parts[1]);
    if (parts[2] === 'w' && parts[3]) return { kind: 'workspace', machine, path: dec(parts[3]) };
    if (parts[2] === 'history') return { kind: 'history', machine, cwd: q.get('cwd') || undefined };
    if (parts[2] === 'log') return { kind: 'log', machine };
    return { kind: 'machine', machine };
  }
  if (parts[0] === 'a' && parts[1]) {
    const agent = dec(parts[1]);
    return parts[2] === 'info' ? { kind: 'agent', agent } : { kind: 'chat', agent };
  }
  if (parts[0] === 'settings') return { kind: 'settings' };
  return { kind: 'home' };
}

/** Machine id and session id of an agent id ("<machine>::<session>[/<subagent>]"). */
export function splitAgentId(id: string): { machine: string; session: string; subagent?: string } {
  const i = id.indexOf('::');
  const machine = i >= 0 ? id.slice(0, i) : '';
  const [session = '', subagent] = (i >= 0 ? id.slice(i + 2) : id).split('/');
  return { machine, session, subagent };
}

/**
 * The screen above this one, for a screen opened cold (reload, a link): an agent's workspace needs the
 * fleet to know it, so callers pass it when they have it.
 */
export function parentRoute(r: Route, workspaceOf?: (agentId: string) => string | undefined): string {
  switch (r.kind) {
    case 'home':
      return R.home;
    case 'machine':
    case 'settings':
      return R.home;
    case 'workspace':
    case 'log':
      return R.machine(r.machine);
    case 'history':
      return r.cwd ? R.workspace(r.machine, r.cwd) : R.machine(r.machine);
    case 'chat':
    case 'agent': {
      const { machine, subagent } = splitAgentId(r.agent);
      if (subagent && r.kind === 'chat') return R.chat(r.agent.split('/')[0]!);
      const ws = workspaceOf?.(r.agent);
      if (r.kind === 'agent' && !subagent) return ws ? R.workspace(machine, ws) : R.machine(machine);
      if (r.kind === 'agent') return R.agent(r.agent.split('/')[0]!);
      return ws ? R.workspace(machine, ws) : machine ? R.machine(machine) : R.home;
    }
  }
}
