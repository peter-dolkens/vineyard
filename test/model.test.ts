import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { ManagedInfo } from '../src/core/model.ts';
import { notifiesHere } from '../src/core/model.ts';

const managed: ManagedInfo = { sessionId: 's1', pid: 1, cwd: '/w', startedAt: 0, ready: true, turns: 0, exited: false };

test('local sessions Vineyard did not start are left to their own UI', () => {
  assert.equal(notifiesHere({}, true), false);
  assert.equal(notifiesHere({ managed: undefined }, true), false);
});

test('managed local sessions and every remote session notify', () => {
  assert.equal(notifiesHere({ managed }, true), true);
  assert.equal(notifiesHere({}, false), true);
  assert.equal(notifiesHere({ managed }, false), true);
});
