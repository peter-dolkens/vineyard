import { test } from 'node:test';
import assert from 'node:assert/strict';
import { accountDetail, accountExpired, accountExpiry, accountPlan, accountTitle, type ClaudeAccount } from '../src/core/accounts.ts';

const DAY = 86_400_000;
const now = 1_800_000_000_000;

test('account labels', () => {
  const a: ClaudeAccount = {
    key: 'acct:org',
    email: 'sam@example.com',
    organization: 'Example Co',
    plan: 'team',
    refreshExpiresAt: now + 3 * DAY + 1,
    usage: { status: 'allowed', at: now, windows: { five_hour: { utilization: 0.4 }, seven_day: { utilization: 0.12 }, seven_day_opus: { utilization: 0.5 } } },
  };
  assert.equal(accountTitle(a), 'sam@example.com');
  assert.equal(accountTitle({ key: 'abcdef123456' }), 'abcdef12');
  assert.equal(accountPlan(a), 'Claude Team');
  assert.equal(accountDetail(a, now), 'Example Co · Claude Team · 5hr 40% · 7d 12% · sign-in lapses in 3 days');
});

test('account expiry', () => {
  assert.equal(accountExpiry({ key: 'k', refreshExpiresAt: now + 20 * DAY }, now), '');
  assert.equal(accountExpiry({ key: 'k', refreshExpiresAt: now + DAY + 5 }, now), 'sign-in lapses in 1 day');
  assert.equal(accountExpiry({ key: 'k', refreshExpiresAt: now + 60_000 }, now), 'sign-in lapses today');
  assert.equal(accountExpiry({ key: 'k', refreshExpiresAt: now - 1 }, now), 'sign-in expired');
  assert.equal(accountExpiry({ key: 'k', refreshExpiresAt: now - 1, active: true }, now), '');
  assert.equal(accountExpired({ key: 'k', refreshExpiresAt: now - 1 }, now), true);
  assert.equal(accountExpired({ key: 'k' }, now), false);
});
