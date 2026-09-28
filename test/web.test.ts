import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { Agent, FleetEntry, Workspace } from '../src/core/model.ts';
import { R, parentRoute, parseRoute, splitAgentId } from '../src/web/routes.ts';
import { FleetStore, agentsFor, countByState, machinesFor, subagentRows, workspacesFor, type Transition, type ViewPrefs } from '../src/web/store.ts';

const prefs: ViewPrefs = { showHistorical: true, showExited: false, showFinishedSubagents: true, hideUnseenDays: 30, sortMachines: 'status', sortWorkspaces: 'name', sortAgents: 'recent' };

const agent = (id: string, state: Agent['state'], extra: Partial<Agent> = {}): Agent => ({
  id: `atelier::${id}`,
  provider: 'claude',
  machineId: 'atelier',
  workspacePath: '/w/shop',
  sessionId: id,
  alive: state !== 'exited',
  state,
  pendingTools: [],
  ...extra,
});

const ws = (path: string, agents: Agent[], extra: Partial<Workspace> = {}): Workspace => ({ id: `atelier:${path}`, machineId: 'atelier', path, agents, historyCount: 0, openInIde: false, ...extra });

const entry = (id: string, online: boolean, workspaces: Workspace[] = [], extra: Partial<FleetEntry> = {}): FleetEntry => ({
  snapshot: { machineId: id, name: id, host: {}, agents: workspaces.flatMap((w) => w.agents), workspaces, at: 1000, seq: 1, hasClaude: true },
  online,
  via: online ? 'direct' : 'cache',
  lastSeen: 1000,
  receivedAt: 1000,
  ...extra,
});

test('routes round-trip, including agent ids with :: and / and paths with slashes', () => {
  const id = 'atelier.local::5f2c/agent-7';
  assert.deepEqual(parseRoute(R.chat(id)), { kind: 'chat', agent: id });
  assert.deepEqual(parseRoute(R.agent(id)), { kind: 'agent', agent: id });
  assert.deepEqual(parseRoute(R.workspace('forge', '/Users/me/Projects/x y')), { kind: 'workspace', machine: 'forge', path: '/Users/me/Projects/x y' });
  assert.deepEqual(parseRoute(R.history('forge', '/w/a')), { kind: 'history', machine: 'forge', cwd: '/w/a' });
  assert.deepEqual(parseRoute(R.history('forge')), { kind: 'history', machine: 'forge', cwd: undefined });
  assert.deepEqual(parseRoute('/nonsense'), { kind: 'home' });
  assert.deepEqual(splitAgentId(id), { machine: 'atelier.local', session: '5f2c', subagent: 'agent-7' });
});

test('a screen opened cold knows the one above it', () => {
  const where = () => '/w/shop';
  assert.equal(parentRoute(parseRoute(R.chat('atelier::s1')), where), R.workspace('atelier', '/w/shop'));
  assert.equal(parentRoute(parseRoute(R.chat('atelier::s1')), () => undefined), R.machine('atelier'));
  assert.equal(parentRoute(parseRoute(R.chat('atelier::s1/sub')), where), R.chat('atelier::s1'));
  assert.equal(parentRoute(parseRoute(R.agent('atelier::s1/sub')), where), R.agent('atelier::s1'));
  assert.equal(parentRoute(parseRoute(R.history('atelier', '/w/shop'))), R.workspace('atelier', '/w/shop'));
  assert.equal(parentRoute(parseRoute(R.workspace('atelier', '/w'))), R.machine('atelier'));
  assert.equal(parentRoute(parseRoute(R.machine('atelier'))), R.home);
});

test('machines: this one first, then online, and long-unseen ones hidden', () => {
  const store = new FleetStore();
  const now = 100 * 86_400_000;
  store.apply({
    t: 'fleet',
    self: 'workshop',
    entries: [entry('orchard', false, [], { lastSeen: now - 40 * 86_400_000 }), entry('forge', false, [], { lastSeen: now - 86_400_000 }), entry('atelier', true), entry('workshop', true)],
    peers: [{ machineId: 'barn', addr: 'barn:7734', connected: false }],
  });
  const names = machinesFor(store.allMachines(), prefs, now).map((m) => m.name);
  assert.deepEqual(names, ['workshop', 'atelier', 'barn', 'forge']); // orchard unseen 40 days; barn never seen stays
  assert.deepEqual(machinesFor(store.allMachines(), { ...prefs, hideUnseenDays: 0 }, now).length, 5);
});

test('workspaces and agents follow the chosen order and the exited / historical toggles', () => {
  const store = new FleetStore();
  const shop = ws('/w/shop', [agent('a', 'idle', { startedAt: 1 }), agent('b', 'question', { startedAt: 2 }), agent('c', 'exited', { startedAt: 3 })]);
  const yard = ws('/w/yard', [agent('d', 'working', { workspacePath: '/w/yard' })], { lastActivityAt: 50 });
  const old = ws('/w/attic', [], { historyCount: 3 });
  store.apply({ t: 'fleet', self: 'atelier', entries: [entry('atelier', true, [shop, yard, old])], peers: [] });
  const m = store.machine('atelier')!;
  assert.deepEqual(workspacesFor(m, prefs).map((w) => w.path), ['/w/attic', '/w/shop', '/w/yard']);
  assert.deepEqual(workspacesFor(m, { ...prefs, showHistorical: false }).map((w) => w.path), ['/w/shop', '/w/yard']);
  assert.deepEqual(workspacesFor(m, { ...prefs, sortWorkspaces: 'attention' }).map((w) => w.path), ['/w/shop', '/w/yard', '/w/attic']);
  assert.deepEqual(agentsFor(shop, prefs).map((a) => a.sessionId), ['b', 'a']);
  assert.deepEqual(agentsFor(shop, { ...prefs, showExited: true }).map((a) => a.sessionId), ['c', 'b', 'a']);
  assert.equal(countByState(shop.agents), '1 needs you · 1 idle');
  assert.deepEqual(store.attention().map((x) => x.agent.sessionId), ['b']);
});

test('subagent rows nest by parent and hide finished ones only when asked, keeping active descendants', () => {
  const a = agent('s', 'tool', {
    subagents: [
      { agentId: 'p', state: 'done' },
      { agentId: 'q', state: 'tool', parentAgentId: 'p' },
      { agentId: 'r', state: 'done' },
    ],
  });
  assert.deepEqual(subagentRows(a, prefs).map((x) => `${x.sub.agentId}${x.depth}`), ['p0', 'q1', 'r0']);
  assert.deepEqual(subagentRows(a, { ...prefs, showFinishedSubagents: false }).map((x) => x.sub.agentId), ['p', 'q']);
});

test('state changes are reported as transitions, but not on a first sighting', () => {
  const store = new FleetStore();
  const seen: Transition[] = [];
  store.onTransition((t) => seen.push(t));
  store.apply({ t: 'fleet', self: 'atelier', entries: [entry('atelier', true, [ws('/w/shop', [agent('a', 'working')])])], peers: [] });
  assert.equal(seen.length, 0);
  store.apply({ t: 'update', entry: entry('atelier', true, [ws('/w/shop', [agent('a', 'question')])]) });
  assert.equal(seen.length, 1);
  assert.equal(seen[0]!.previous?.state, 'working');
  assert.equal(seen[0]!.current.state, 'question');
  const found = store.findAgent('atelier::a');
  assert.equal(found?.machine.local, true);
});
