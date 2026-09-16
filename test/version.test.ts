import { test } from 'node:test';
import assert from 'node:assert/strict';
import { compareVersions, isNewer, parseVersion } from '../src/core/version.ts';

test('parseVersion accepts release, dev and git-describe forms', () => {
  assert.deepEqual(parseVersion('0.2.0'), { major: 0, minor: 2, patch: 0, suffix: '' });
  assert.deepEqual(parseVersion('v0.3.1'), { major: 0, minor: 3, patch: 1, suffix: '' });
  assert.deepEqual(parseVersion('0.2.0-dev'), { major: 0, minor: 2, patch: 0, suffix: 'dev' });
  assert.deepEqual(parseVersion('v0.2.0-3-g1006fe4'), { major: 0, minor: 2, patch: 0, suffix: '3-g1006fe4' });
  assert.equal(parseVersion('dev'), undefined);
  assert.equal(parseVersion(''), undefined);
  assert.equal(parseVersion(undefined), undefined);
});

test('compareVersions orders by numeric core only', () => {
  assert.ok(compareVersions('0.3.0', '0.2.9') > 0);
  assert.ok(compareVersions('0.2.0', '0.10.0') < 0);
  assert.equal(compareVersions('0.2.0-dev', 'v0.2.0-3-gabc'), 0);
  assert.equal(compareVersions('dev', '0.2.0'), 0);
});

test('isNewer is strict and refuses unparseable input', () => {
  assert.equal(isNewer('0.3.0', '0.2.0'), true);
  assert.equal(isNewer('0.2.0', '0.3.0'), false);
  assert.equal(isNewer('0.3.0-dev', '0.3.0'), false);
  assert.equal(isNewer('0.3.0', '0.3.0-dev'), false);
  assert.equal(isNewer('0.3.0', 'dev'), false);
  assert.equal(isNewer('dev', '0.1.0'), false);
  assert.equal(isNewer('0.3.0', undefined), false);
});
