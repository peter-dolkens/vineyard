import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { ManagedInfo } from '../src/core/model.ts';
import { isLongUnseen, notifiesHere } from '../src/core/model.ts';

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

test('machines long offline leave the tree; this machine and never-seen ones stay', () => {
  const day = 86_400_000;
  const now = 100 * day;
  assert.equal(isLongUnseen({ local: false, online: false, lastSeen: now - 31 * day }, 30, now), true);
  assert.equal(isLongUnseen({ local: false, online: false, lastSeen: now - 29 * day }, 30, now), false);
  assert.equal(isLongUnseen({ local: false, online: true, lastSeen: now - 90 * day }, 30, now), false);
  assert.equal(isLongUnseen({ local: true, online: false, lastSeen: now - 90 * day }, 30, now), false);
  assert.equal(isLongUnseen({ local: false, online: false }, 30, now), false);
  assert.equal(isLongUnseen({ local: false, online: false, lastSeen: now - 90 * day }, 0, now), false);
});
