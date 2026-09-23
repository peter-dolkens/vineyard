import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { BackgroundTask } from '../src/core/model.ts';
import { clock, taskActive, taskElapsed, taskLabel, taskStateLabel } from '../src/core/tasks.ts';

const running: BackgroundTask = { toolUseId: 'tu1', taskId: 'abc', kind: 'shell', state: 'running', description: 'Watch CI', startedAt: 1_000_000 };
const failed: BackgroundTask = { toolUseId: 'tu2', kind: 'shell', state: 'failed', startedAt: 1_000_000, endedAt: 1_065_000 };

test('task labels and states', () => {
  assert.ok(taskActive(running) && !taskActive(failed));
  assert.equal(taskLabel(running), 'Watch CI');
  assert.equal(taskLabel({ ...failed, taskId: 'zz9' }), 'Task zz9');
  assert.equal(taskLabel(failed), 'Background command');
  assert.equal(taskStateLabel(running), 'Running');
  assert.equal(taskStateLabel(failed), 'Failed');
});

test('elapsed runs to now while running and stops at the end otherwise', () => {
  assert.equal(taskElapsed(running, 1_600_000), 600_000);
  assert.equal(taskElapsed(failed, 9_000_000), 65_000);
  assert.equal(taskElapsed({ ...running, startedAt: undefined }, 5), 0);
});

test('clock format', () => {
  assert.equal(clock(45_000), '45s');
  assert.equal(clock(645_000), '10m 45s');
  assert.equal(clock(2 * 3_600_000 + 5 * 60_000), '2h 05m');
});
