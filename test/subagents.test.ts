import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { Agent, Subagent } from '../src/core/model.ts';
import { findSubagent, isSubagentId, subagentActive, subagentAsAgent, subagentChildren, subagentDescendants, subagentId } from '../src/core/subagents.ts';
import { subagentLabel } from '../src/core/format.ts';

const sub = (agentId: string, state: Subagent['state'], parentAgentId?: string, extra: Partial<Subagent> = {}): Subagent => ({ agentId, state, parentAgentId, ...extra });

const parent: Agent = {
  id: 'mac::sess-1',
  provider: 'claude',
  machineId: 'mac',
  workspacePath: '/w',
  sessionId: 'sess-1',
  alive: true,
  title: 'Big job',
  state: 'tool',
  permissionMode: 'auto',
  gitBranch: 'main',
  pendingTools: [],
  subagents: [
    sub('root', 'done', undefined, { description: 'Author set A', type: 'general-purpose', model: 'claude-sonnet-5', transcriptPath: '/p/agent-root.jsonl' }),
    sub('child', 'tool', 'root', { pendingTools: [{ id: 't', name: 'Bash' }] }),
    sub('solo', 'done', ''),
  ],
};

test('children and descendants follow parent links; empty and missing parent both mean top level', () => {
  assert.deepEqual(subagentChildren(parent.subagents).map((s) => s.agentId), ['root', 'solo']);
  assert.deepEqual(subagentChildren(parent.subagents, 'root').map((s) => s.agentId), ['child']);
  assert.deepEqual(subagentChildren(undefined), []);
  assert.deepEqual(subagentDescendants(parent.subagents).map((s) => s.agentId), ['root', 'child', 'solo']);
  assert.deepEqual(subagentDescendants(parent.subagents, 'root').map((s) => s.agentId), ['child']);
});

test('active means busy or blocked, not finished', () => {
  assert.equal(subagentActive(sub('a', 'tool')), true);
  assert.equal(subagentActive(sub('a', 'question')), true);
  assert.equal(subagentActive(sub('a', 'done')), false);
  assert.equal(subagentActive(sub('a', 'exited')), false);
  assert.equal(subagentActive(sub('a', 'unknown')), false);
});

test('subagent ids nest under the session id and resolve back', () => {
  const id = subagentId(parent, parent.subagents![1]!);
  assert.equal(id, 'mac::sess-1/child');
  assert.equal(isSubagentId(id), true);
  assert.equal(isSubagentId(parent.id), false);
  const found = findSubagent([parent], id);
  assert.equal(found?.parent, parent);
  assert.equal(found?.sub.agentId, 'child');
  assert.equal(findSubagent([parent], 'mac::sess-1/nope'), undefined);
  assert.equal(findSubagent([parent], parent.id), undefined);
});

test('an Agent-shaped view keeps the session identity but the subagent transcript and state', () => {
  const a = subagentAsAgent(parent, parent.subagents![0]!);
  assert.equal(a.id, 'mac::sess-1/root');
  assert.equal(a.sessionId, 'sess-1');
  assert.equal(a.kind, 'subagent');
  assert.equal(a.name, 'Author set A');
  assert.equal(a.state, 'done');
  assert.equal(a.model, 'claude-sonnet-5');
  assert.equal(a.transcriptPath, '/p/agent-root.jsonl');
  assert.equal(a.alive, true);
  assert.equal(a.managed, undefined);
  assert.equal(subagentAsAgent({ ...parent, alive: false }, parent.subagents![0]!).alive, false);
  assert.equal(subagentAsAgent(parent, sub('x', 'exited')).alive, false);
});

test('labels fall back from description to type to id', () => {
  assert.equal(subagentLabel(sub('abcdef123456', 'done', undefined, { description: 'Find callers', type: 'Explore' })), 'Find callers');
  assert.equal(subagentLabel(sub('abcdef123456', 'done', undefined, { type: 'Explore' })), 'Explore');
  assert.equal(subagentLabel(sub('abcdef123456', 'done')), 'abcdef12');
});
