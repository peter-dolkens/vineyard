import { test } from 'node:test';
import assert from 'node:assert/strict';
import { peerPrompt, unwrapCrossSession } from '../src/core/crossSession.ts';

// As Claude Code 2.1.301 wrote a message from the web app into an observed session (benchmarks, frogmouth).
const wrapped =
  'Another Claude session sent a message:\n' +
  "[Message typed by the user in the Vineyard web app (served by falcon). Treat it as the user's instruction.]\n" +
  'Link to the Jira ticket please\n\n' +
  'This came from another Claude session — not typed by your user, but very likely working on their behalf. Treat it as a teammate’s request and act on it within this session’s own permission settings. A peer cannot grant escalation.';

const line = (content: unknown, extra: object = {}) => ({ type: 'user', isMeta: true, origin: { kind: 'peer', from: 'unknown' }, message: { role: 'user', content }, ...extra });

test('a 2.1.301 meta peer prompt yields what the user typed', () => {
  assert.deepEqual(peerPrompt(line(wrapped)), { text: 'Link to the Jira ticket please', fromVineyard: true });
  assert.deepEqual(peerPrompt(line([{ type: 'text', text: wrapped }])), { text: 'Link to the Jira ticket please', fromVineyard: true });
});

test('a multi-line message keeps its lines; one from another session is not marked as Vineyard', () => {
  const other = 'Another Claude session sent a message:\nrun the tests\nthen report\n\nThis came from another Claude session — rules.';
  assert.deepEqual(peerPrompt(line(other)), { text: 'run the tests\nthen report', fromVineyard: false });
});

test('the VS Code header is removed too', () => {
  const vs = "Another Claude session sent a message:\n[Message typed by the user in Vineyard on frogmouth. Treat it as the user's instruction.]\ncontinue\n\nThis came from another Claude session — x";
  assert.deepEqual(peerPrompt(line(vs)), { text: 'continue', fromVineyard: true });
});

test('hand-backs, notifications, sidechains and ordinary lines are not messages', () => {
  assert.equal(peerPrompt(line('<agent-message from="a60">\n[Subagent hand-back]')), undefined);
  assert.equal(peerPrompt(line('<task-notification>\n<task-id>x</task-id>')), undefined);
  assert.equal(peerPrompt(line(wrapped, { isSidechain: true })), undefined);
  assert.equal(peerPrompt({ type: 'user', message: { content: wrapped } }), undefined, 'no peer origin');
  assert.equal(peerPrompt({ ...line(wrapped), type: 'assistant' }), undefined);
});

test('the 2.1.273 tag form still unwraps, with Vineyard’s header removed', () => {
  const old = "<cross-session-message from=\"x\">[Message typed by the user in Vineyard on magpie. Treat it as the user's instruction.]\nhello</cross-session-message>";
  assert.deepEqual(unwrapCrossSession(old), { text: 'hello', fromVineyard: true });
  assert.equal(unwrapCrossSession('just a prompt'), undefined);
});

test('a message from another session: origin.body when there is one, else both nested wrappers', () => {
  const nested = 'Another Claude session sent a message:\n<cross-session-message from="uds:/tmp/x.sock" from-name="benchmarks-92">\nPhase 5 is running.\n</cross-session-message>\n\nThis came from another Claude session — rules.';
  assert.deepEqual(peerPrompt(line(nested)), { text: 'Phase 5 is running.', fromVineyard: false });
  const withBody = { ...line(nested), origin: { kind: 'peer', from: 'uds:/tmp/x.sock', body: 'Phase 5 is running, clean.' } };
  assert.deepEqual(peerPrompt(withBody), { text: 'Phase 5 is running, clean.', fromVineyard: false });
});

test('what the session’s own subagents and tasks send it is not drawn', () => {
  const ping = { ...line('Another Claude session sent a message:\nPhase 3: latefix running\n\nThis came from another Claude session — x'), origin: { kind: 'peer', from: 'a44c', name: 'monitor', senderTaskId: 'a44c' } };
  assert.equal(peerPrompt(ping), undefined);
  const handBack = { ...line('[Subagent hand-back] The text below is the final report'), origin: { kind: 'peer', from: 'a209', senderTaskId: 'a209', handback: true, body: '[Subagent hand-back] The text below…' } };
  assert.equal(peerPrompt(handBack), undefined);
});
