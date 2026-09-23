import { test } from 'node:test';
import assert from 'node:assert/strict';
import { CACHE_TTL_MS, buildActions, cacheState, contextFor, contextGauge, contextWindow, effortLabel, filterActions, groupActions, modeInfo, type ActionContext } from '../src/core/composer.ts';

test('context window follows the [1m] suffix', () => {
  assert.equal(contextWindow('claude-opus-5-5[1m]'), 1_000_000);
  assert.equal(contextWindow('claude-fable-5-1'), 200_000);
  assert.equal(contextWindow(undefined), 200_000);
});

test('context gauge clamps and grades', () => {
  assert.deepEqual(contextGauge(50_000, 200_000), { fraction: 0.25, percent: 25, level: 'ok' });
  assert.equal(contextGauge(150_000, 200_000).level, 'warn');
  assert.equal(contextGauge(190_000, 200_000).level, 'hot');
  assert.equal(contextGauge(5_000_000, 200_000).percent, 100);
  assert.equal(contextGauge(undefined, 200_000).percent, 0);
  assert.equal(contextGauge(300_000, 1_000_000).percent, 30);
  assert.equal(contextGauge(100_000, 0).percent, 50, 'no window: 200k');
  assert.equal(contextGauge(100_000, undefined).percent, 50);
});

test('the ring takes the measured window when Claude Code gave one, else infers it from the model id', () => {
  assert.deepEqual(contextFor({ contextTokens: 48_213, contextWindow: 200_000, managed: { exited: false } }, 'claude-opus-5-5[1m]'), { tokens: 48_213, window: 200_000, measured: true, compacting: false });
  assert.deepEqual(contextFor({ contextTokens: 48_213 }, 'claude-opus-5-5[1m]'), { tokens: 48_213, window: 1_000_000, measured: false, compacting: false });
  assert.deepEqual(contextFor(undefined, undefined), { tokens: 0, window: 200_000, measured: false, compacting: false });
  assert.equal(contextFor({ contextTokens: 1, managed: { exited: false, compacting: true } }, 'x').compacting, true);
  assert.equal(contextFor({ contextTokens: 1, managed: { exited: true, compacting: true } }, 'x').compacting, false, 'an exited session is not compacting');
});

test('cache state: none before any call, warm within the TTL, cold after', () => {
  const now = 1_000_000_000;
  assert.deepEqual(cacheState({ cacheRead: 0, cacheCreate: 0, input: 0, calls: 0 }, now, now), { state: 'none', hitPercent: 0 });
  assert.deepEqual(cacheState({ cacheRead: 90, cacheCreate: 5, input: 5, calls: 3 }, now - 1000, now), { state: 'warm', hitPercent: 90 });
  assert.deepEqual(cacheState({ cacheRead: 90, cacheCreate: 5, input: 5, calls: 3 }, now - CACHE_TTL_MS - 1, now), { state: 'cold', hitPercent: 90 });
  assert.equal(cacheState({ cacheRead: 0, cacheCreate: 0, input: 0, calls: 1 }, now, now).hitPercent, 0);
});

test('mode and effort labels', () => {
  assert.equal(modeInfo('auto').label, 'Auto');
  assert.equal(modeInfo('bypassPermissions').icon, 'unlock');
  assert.equal(modeInfo(undefined).label, 'Manual');
  assert.equal(modeInfo('weird').label, 'weird');
  assert.equal(effortLabel('xhigh'), 'Extra high');
  assert.equal(effortLabel(''), 'Default');
});

const base: ActionContext = {
  online: true,
  alive: true,
  managedLive: true,
  subagent: false,
  busy: true,
  machineName: 'falcon',
  modelLabel: 'Fable 5.1',
  effort: 'high',
  modeLabel: 'Auto',
  commands: [
    { name: 'compact', description: 'Clear history but keep a summary' },
    { name: 'code-review', description: 'Review the current diff', argumentHint: '[level]' },
  ],
  account: 'p@example.com',
  extVersion: '0.4.0',
  claudeVersion: '2.1.278',
};

test('a live managed session gets every action, slash commands included', () => {
  const acts = buildActions(base);
  const ids = acts.map((a) => a.id);
  assert.ok(ids.includes('compact') && ids.includes('slash:compact') && ids.includes('slash:code-review'));
  assert.ok(acts.every((a) => !a.disabled), `unexpected disabled: ${acts.filter((a) => a.disabled).map((a) => a.id)}`);
  assert.equal(acts.find((a) => a.id === 'model')!.value, 'Fable 5.1');
  assert.equal(acts.find((a) => a.id === 'effort')!.kind, 'effort');
  assert.equal(acts.find((a) => a.id === 'slash:code-review')!.value, '[level]');
  assert.match(acts.find((a) => a.id === 'login')!.detail!, /p@example.com/);
  assert.equal(acts.find((a) => a.id === 'report')!.value, 'Vineyard 0.4.0 · Claude Code 2.1.278');
  assert.equal(acts.find((a) => a.id === 'stop')!.label, 'Stop session');
});

test('an observed session keeps the settings rows but marks them off, and has no slash commands', () => {
  const acts = buildActions({ ...base, managedLive: false, commands: undefined });
  const off = acts.filter((a) => a.disabled).map((a) => a.id);
  assert.deepEqual(off, ['compact', 'clear', 'model', 'effort', 'mode', 'interrupt']);
  assert.ok(!acts.some((a) => a.id.startsWith('slash:')));
  assert.equal(acts.find((a) => a.id === 'stop')!.label, 'Terminate session');
  assert.match(acts.find((a) => a.id === 'model')!.detail!, /Only sessions started by Vineyard/);
});

test('offline and subagent views are mostly read-only', () => {
  const offline = buildActions({ ...base, online: false, managedLive: false });
  assert.ok(offline.find((a) => a.id === 'attach')!.disabled);
  assert.match(offline.find((a) => a.id === 'attach')!.detail!, /falcon is offline/);
  assert.ok(offline.find((a) => a.id === 'login')!.disabled);
  const sub = buildActions({ ...base, subagent: true, managedLive: false });
  for (const id of ['attach', 'rename', 'stop', 'resumeTerminal']) assert.ok(sub.find((a) => a.id === id)!.disabled, id);
  assert.ok(!sub.find((a) => a.id === 'rawTranscript')!.disabled);
});

test('interrupt only offered mid-turn', () => {
  assert.ok(buildActions({ ...base, busy: false }).find((a) => a.id === 'interrupt')!.disabled);
  assert.ok(!buildActions(base).find((a) => a.id === 'interrupt')!.disabled);
});

test('filter matches words across label, detail and description, ignoring a leading slash', () => {
  const acts = buildActions(base);
  assert.deepEqual(filterActions(acts, '/comp').map((a) => a.id), ['compact', 'slash:compact']);
  assert.deepEqual(filterActions(acts, 'review diff').map((a) => a.id), ['slash:code-review']);
  assert.deepEqual(filterActions(acts, 'terminal falcon').map((a) => a.id), ['openTerminal']);
  assert.equal(filterActions(acts, '   ').length, acts.length);
  assert.deepEqual(filterActions(acts, 'zzz'), []);
});

test('filter ranks label-prefix matches above word matches above the rest', () => {
  const acts = buildActions(base);
  // "code" is in "/code-review" (prefix), "Open workspace in VS Code" (word) and the report row's "Claude Code 2.1.278" (value).
  assert.deepEqual(filterActions(acts, '/code').map((a) => a.id), ['slash:code-review', 'openWorkspace', 'report']);
  // "Session info" starts with it; "Rename session…", "Stop session", "Copy session ID" have it at a word start, in display order.
  assert.deepEqual(filterActions(acts, 'session').map((a) => a.id).slice(0, 4), ['info', 'rename', 'stop', 'copySessionId']);
});

test('grouping keeps first-seen order', () => {
  const groups = groupActions(buildActions(base));
  assert.deepEqual(
    groups.map((g) => g.group),
    ['Context', 'Model', 'Session', 'Slash commands', 'Settings', 'Support'],
  );
  assert.equal(groups[3]!.actions.length, 2);
});
