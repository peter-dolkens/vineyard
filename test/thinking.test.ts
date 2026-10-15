import { test } from 'node:test';
import assert from 'node:assert/strict';
import { isNarration } from '../src/core/thinking.ts';

// Shapes taken from a real transcript: two consecutive thinking blocks in one response, the second
// of which Claude Code prints as ordinary output ("Now I'll write the parser based on this structure").
const reasoning = { apiBlockIndex: 0, thinkingDurationMs: 16050, thinkingDisplay: 'summarized' };
const narration = { apiBlockIndex: 1, thinkingDurationMs: 2, thinkingDisplay: 'summarized' };

test('the reasoning block of a response is not narration', () => {
  assert.equal(isNarration(reasoning), false);
  assert.equal(isNarration({ apiBlockIndex: 0, thinkingDurationMs: 2 }), false); // fast, but still the reasoning
});

test('a later block written in no measurable time is narration', () => {
  assert.equal(isNarration(narration), true);
});

test('a later block that took real thinking stays folded away', () => {
  assert.equal(isNarration({ apiBlockIndex: 1, thinkingDurationMs: 4500 }), false);
});

test('older transcripts carry no duration; the block index decides alone', () => {
  assert.equal(isNarration({ apiBlockIndex: 1 }), true);
  assert.equal(isNarration({ apiBlockIndex: 0 }), false);
  assert.equal(isNarration({}), false);
});
