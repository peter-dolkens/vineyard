import { test } from 'node:test';
import assert from 'node:assert/strict';
import { relativeTime, shortModel, tokens, basename, tildify } from '../src/core/format.ts';

test('shortModel', () => {
  assert.equal(shortModel('claude-fable-5-1'), 'Fable 5.1');
  assert.equal(shortModel('claude-opus-4-1-20250805'), 'Opus 4.1');
  assert.equal(shortModel('claude-fable-5-1[1m]'), 'Fable 5.1');
  assert.equal(shortModel(undefined), '');
});

test('relativeTime', () => {
  const now = 1_000_000_000;
  assert.equal(relativeTime(now - 2_000, now), 'just now');
  assert.equal(relativeTime(now - 30_000, now), '30s ago');
  assert.equal(relativeTime(now - 5 * 60_000, now), '5m ago');
  assert.equal(relativeTime(now - 3 * 3_600_000, now), '3h ago');
});

test('tokens / paths', () => {
  assert.equal(tokens(205_145), '205k');
  assert.equal(tokens(1_500_000), '1.5M');
  assert.equal(basename('/Users/peter/Projects/vineyard/'), 'vineyard');
  assert.equal(tildify('/Users/peter/Projects/x', '/Users/peter'), '~/Projects/x');
});
