import { test } from 'node:test';
import assert from 'node:assert/strict';
import { shouldSend, wantedListen } from '../src/core/webApp.ts';

const forge = { id: 'forge.local', name: 'forge' };

test('the setting maps to a listen address per machine', () => {
  assert.equal(wantedListen({ on: false, port: 7735, machines: [] }, forge), '');
  assert.equal(wantedListen({ on: true, port: 7735, machines: [] }, forge), ':7735');
  assert.equal(wantedListen({ on: true, port: 8080, machines: ['Forge'] }, forge), ':8080');
  assert.equal(wantedListen({ on: true, port: 7735, machines: ['orchard'] }, forge), '');
  assert.equal(wantedListen({ on: true, port: 7735, machines: ['forge.local'] }, forge), ':7735');
});

test('only a newer, different choice is sent; nothing while every machine agrees', () => {
  assert.equal(shouldSend({ listen: '', at: 0 }, ':7735', 0), false, 'never chosen here');
  assert.equal(shouldSend({ listen: '', at: 0 }, ':7735', 100), true);
  assert.equal(shouldSend({ listen: ':7735', at: 50 }, ':7735', 100), false, 'already as wanted');
  assert.equal(shouldSend({ listen: ':7735', at: 200 }, '', 100), false, 'a later choice from another window wins');
  assert.equal(shouldSend({ listen: ':7735', at: 100 }, '', 100), false, 'already told');
  assert.equal(shouldSend({ listen: ':7735', at: 50 }, '', 100), true);
  assert.equal(shouldSend(undefined, '', 100), false, 'old daemon, off: nothing to say');
  assert.equal(shouldSend(undefined, ':7735', 100), true, 'old daemon, on: one try');
});
