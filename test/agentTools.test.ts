import { test } from 'node:test';
import assert from 'node:assert/strict';
import { shouldSendTools, wantedTools } from '../src/core/agentTools.ts';

test('settings map to the daemon tool groups, sorted', () => {
  assert.deepEqual(wantedTools(() => false), []);
  const on = new Set(['driveAgents', 'listMachines', 'sendFiles']);
  assert.deepEqual(wantedTools((s) => on.has(s)), ['drive', 'files', 'machines']);
});

test('a changed allowed folder is sent too; "~" and empty mean the same', () => {
  assert.equal(shouldSendTools({ tools: ['newworkspaces'], at: 50 }, ['newworkspaces'], 100, '~'), false);
  assert.equal(shouldSendTools({ tools: ['newworkspaces'], at: 50, workspacesInside: '' }, ['newworkspaces'], 100, ' ~ '), false);
  assert.equal(shouldSendTools({ tools: ['newworkspaces'], at: 50 }, ['newworkspaces'], 100, '~/Projects'), true);
  assert.equal(shouldSendTools({ tools: ['newworkspaces'], at: 50, workspacesInside: '~/Projects' }, ['newworkspaces'], 100, '~/Projects'), false);
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
