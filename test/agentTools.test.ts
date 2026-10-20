import { test } from 'node:test';
import assert from 'node:assert/strict';
import { shouldSendTools, wantedTools } from '../src/core/agentTools.ts';

test('settings map to the daemon tool groups, sorted', () => {
  assert.deepEqual(wantedTools(() => false), []);
  const on = new Set(['driveAgents', 'listMachines']);
  assert.deepEqual(wantedTools((s) => on.has(s)), ['drive', 'machines']);
});

test('only a newer, different choice is sent', () => {
  assert.equal(shouldSendTools({ tools: [], at: 0 }, ['drive'], 0), false, 'never chosen here');
  assert.equal(shouldSendTools({ tools: [], at: 0 }, ['drive'], 100), true);
  assert.equal(shouldSendTools({ tools: ['machines', 'drive'], at: 50 }, ['drive', 'machines'], 100), false, 'same set, any order');
  assert.equal(shouldSendTools({ tools: ['drive'], at: 200 }, [], 100), false, 'a later choice from another window wins');
  assert.equal(shouldSendTools({ tools: ['drive'], at: 50 }, [], 100), true);
  assert.equal(shouldSendTools(undefined, [], 100), false, 'old daemon, none wanted');
  assert.equal(shouldSendTools(undefined, ['drive'], 100), true, 'old daemon, some wanted: one try');
});
