import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { Usage } from '../src/core/model.ts';
import { percentOf, resetsIn, usageRows, usageWarning, usageWarningKey } from '../src/core/usage.ts';

const now = 1_700_000_000_000;
const usage: Usage = {
  status: 'allowed_warning',
  rateLimitType: 'five_hour',
  utilization: 0.93,
  resetsAt: now + 2 * 3_600_000,
  windows: {
    seven_day: { utilization: 0.19, resetsAt: now + 6 * 86_400_000 },
    five_hour: { utilization: 0.93, resetsAt: now + 2 * 3_600_000 },
    seven_day_overage_included: { utilization: 0.35, resetsAt: now + 6 * 86_400_000 },
  },
  at: now,
};

test('percent handles fractions and percentages', () => {
  assert.equal(percentOf(0.93), 93);
  assert.equal(percentOf(35), 35);
  assert.equal(percentOf(undefined), 0);
});

test('rows follow the panel order with labels', () => {
  assert.deepEqual(
    usageRows(usage).map((r) => [r.key, r.label, r.percent]),
    [
      ['five_hour', 'Session (5hr)', 93],
      ['seven_day', 'Weekly (7 day)', 19],
      ['seven_day_overage_included', 'Weekly Fable', 35],
    ],
  );
  assert.deepEqual(usageRows(undefined), []);
  // A header-only event has no windows block; its own window still makes a row.
  assert.deepEqual(usageRows({ status: 'allowed', rateLimitType: 'seven_day', utilization: 0.4, at: now }).map((r) => [r.key, r.percent]), [['seven_day', 40]]);
});

test('warning text mirrors the Claude Code banner', () => {
  const w = usageWarning(usage, now)!;
  assert.equal(w.row.key, 'five_hour');
  assert.equal(w.rejected, false);
  assert.equal(w.text, "You've used 93% of your session limit · resets in 2h");
  assert.equal(usageWarning({ ...usage, status: 'allowed', windows: { five_hour: { utilization: 0.5 }, seven_day: { utilization: 0.2 } } }, now), undefined, 'comfortable usage: no warning');
  const rejected = usageWarning({ ...usage, status: 'rejected', windows: { five_hour: { utilization: 1, resetsAt: now + 90_000 } } }, now)!;
  assert.equal(rejected.rejected, true);
  assert.equal(rejected.text, "You've hit your session limit · resets in 2m");
  // Claude Code flagged it even though no window reads 80 %: trust the flag.
  const flagged = usageWarning({ status: 'allowed_warning', rateLimitType: 'seven_day', utilization: 0.75, at: now }, now)!;
  assert.equal(flagged.row.key, 'seven_day');
  assert.equal(flagged.text, "You've used 75% of your weekly limit");
});

test('resets-in wording', () => {
  assert.equal(resetsIn(now + 30 * 60_000, now), '30m');
  assert.equal(resetsIn(now + 2 * 3_600_000, now), '2h');
  assert.equal(resetsIn(now + 6 * 86_400_000, now), '6d');
  assert.equal(resetsIn(now - 1, now), 'now');
});

test('dismissal key changes when the window is hit or resets, not as it fills', () => {
  const at85 = usageWarning({ ...usage, windows: { five_hour: { utilization: 0.85, resetsAt: now + 3_600_000 } } }, now)!;
  const at95 = usageWarning({ ...usage, windows: { five_hour: { utilization: 0.95, resetsAt: now + 3_600_000 } } }, now)!;
  const hit = usageWarning({ ...usage, status: 'rejected', windows: { five_hour: { utilization: 1, resetsAt: now + 3_600_000 } } }, now)!;
  const nextWindow = usageWarning({ ...usage, windows: { five_hour: { utilization: 0.85, resetsAt: now + 6 * 3_600_000 } } }, now)!;
  assert.equal(usageWarningKey(at85), usageWarningKey(at95));
  assert.notEqual(usageWarningKey(at85), usageWarningKey(hit));
  assert.notEqual(usageWarningKey(at85), usageWarningKey(nextWindow));
  assert.equal(usageWarningKey(at85), `warn:five_hour:${now + 3_600_000}`);
});
