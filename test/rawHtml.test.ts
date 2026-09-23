import { test } from 'node:test';
import assert from 'node:assert/strict';
import { escapeUnknownTags, RAW_HTML_TAGS } from '../src/core/rawHtml.ts';

test('escapes tags the chat does not render, keeps the ones it does', () => {
  assert.equal(escapeUnknownTags('<attached-file name="a.txt">\nalpha\n</attached-file>'), '&lt;attached-file name="a.txt">\nalpha\n&lt;/attached-file>');
  assert.equal(escapeUnknownTags('<script>alert(1)</script>'), '&lt;script>alert(1)&lt;/script>');
  assert.equal(escapeUnknownTags('<form action=x><input name=y><iframe src=z></iframe>'), '&lt;form action=x>&lt;input name=y>&lt;iframe src=z>&lt;/iframe>');
  assert.equal(escapeUnknownTags('<b>bold</b> and <details><summary>s</summary>x</details>'), '<b>bold</b> and <details><summary>s</summary>x</details>');
  assert.equal(escapeUnknownTags('<IMG src="https://x/y.png"><Div>ok</Div>'), '<IMG src="https://x/y.png"><Div>ok</Div>');
  assert.equal(escapeUnknownTags('<br/><hr />'), '<br/><hr />');
  assert.equal(escapeUnknownTags('Array<string> and Map<K, V>'), 'Array&lt;string> and Map<K, V>');
});

test('leaves autolinks, comparisons and comments alone', () => {
  assert.equal(escapeUnknownTags('see <https://example.com/a?b=1> or <me@example.com>'), 'see <https://example.com/a?b=1> or <me@example.com>');
  assert.equal(escapeUnknownTags('a < b and 1 <3 and x<-y'), 'a < b and 1 <3 and x<-y');
  assert.equal(escapeUnknownTags('<!-- note --> <? x ?>'), '<!-- note --> <? x ?>');
  assert.equal(escapeUnknownTags('plain text'), 'plain text');
});

test('skips fenced code blocks and inline code spans', () => {
  const fenced = 'before <foo>\n```html\n<foo attr="1">\n</foo>\n```\nafter <foo>';
  assert.equal(escapeUnknownTags(fenced), 'before &lt;foo>\n```html\n<foo attr="1">\n</foo>\n```\nafter &lt;foo>');
  const tilde = '~~~\n<bar>\n~~~\n<bar>';
  assert.equal(escapeUnknownTags(tilde), '~~~\n<bar>\n~~~\n&lt;bar>');
  const longerClose = '````\n```\n<x>\n````\n<x>';
  assert.equal(escapeUnknownTags(longerClose), '````\n```\n<x>\n````\n&lt;x>');
  assert.equal(escapeUnknownTags('use `<foo>` not <foo>'), 'use `<foo>` not &lt;foo>');
  assert.equal(escapeUnknownTags('``a ` <foo> b`` <foo>'), '``a ` <foo> b`` &lt;foo>');
  assert.equal(escapeUnknownTags('unclosed ` <foo>'), 'unclosed ` <foo>');
});

test('cross-session and attached-file wrappers render visibly', () => {
  const prompt = '<cross-session-message from="x">\n<attached-file name="notes.md">\n# hi\n</attached-file>\n\nplease read\n</cross-session-message>';
  const out = escapeUnknownTags(prompt);
  assert.ok(!out.includes('<cross-session-message'));
  assert.ok(out.includes('&lt;attached-file name="notes.md">'));
  assert.ok(out.includes('# hi'));
});

test('allowlist has no tags that run code, take input or embed documents', () => {
  for (const t of ['script', 'style', 'form', 'input', 'button', 'iframe', 'object', 'embed', 'svg', 'math', 'link', 'meta', 'base', 'template', 'textarea', 'select']) {
    assert.ok(!RAW_HTML_TAGS.has(t), t);
  }
});
