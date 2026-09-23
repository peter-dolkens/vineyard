import { test } from 'node:test';
import assert from 'node:assert/strict';
import { acceptEditsSuggestions, planText } from '../src/core/plan.ts';

const setMode = { type: 'setMode', mode: 'acceptEdits', destination: 'session' };
const otherMode = { type: 'setMode', mode: 'bypassPermissions', destination: 'session' };
const rule = { type: 'addRules', rules: [{ toolName: 'Edit' }], behavior: 'allow', destination: 'session' };
const dirs = { type: 'addDirectories', directories: ['/w'], destination: 'session' };

test('the offered acceptEdits switch is sent back with any addRules, nothing else', () => {
  assert.deepEqual(acceptEditsSuggestions([rule, setMode, dirs]), [setMode, rule]);
  assert.deepEqual(acceptEditsSuggestions([setMode]), [setMode]);
});

test('no acceptEdits suggestion means the caller must configure the mode itself', () => {
  assert.equal(acceptEditsSuggestions(undefined), undefined);
  assert.equal(acceptEditsSuggestions([]), undefined);
  assert.equal(acceptEditsSuggestions([rule, otherMode]), undefined);
  assert.equal(acceptEditsSuggestions([null, 'x', 3]), undefined);
});

test('planText tolerates a missing or non-string plan', () => {
  assert.equal(planText({ plan: '# Steps\n1. a' }), '# Steps\n1. a');
  assert.equal(planText({}), '');
  assert.equal(planText(undefined), '');
  assert.equal(planText({ plan: 4 }), '');
});
