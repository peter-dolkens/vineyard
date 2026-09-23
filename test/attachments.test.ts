import { test } from 'node:test';
import assert from 'node:assert/strict';
import { MAX_IMAGE_BYTES, MAX_TEXT_BYTES, attachmentMediaType, attachmentProblem, isImageType, looksBinary } from '../src/core/attachments.ts';

test('media types by extension, plain text otherwise', () => {
  assert.equal(attachmentMediaType('Shot.PNG'), 'image/png');
  assert.equal(attachmentMediaType('a.jpeg'), 'image/jpeg');
  assert.equal(attachmentMediaType('notes.md'), 'text/markdown');
  assert.equal(attachmentMediaType('main.go'), 'text/plain');
  assert.equal(attachmentMediaType('Makefile'), 'text/plain');
  assert.ok(isImageType('image/webp'));
  assert.ok(!isImageType('text/plain'));
});

test('binary detection looks for NUL bytes', () => {
  assert.ok(!looksBinary(new TextEncoder().encode('hello\nworld')));
  assert.ok(looksBinary(new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x00])));
});

test('attachment problems', () => {
  const text = new TextEncoder().encode('fine');
  assert.equal(attachmentProblem('a.ts', text, true), undefined);
  assert.equal(attachmentProblem('a.ts', text, false), undefined);
  assert.equal(attachmentProblem('a.png', text, true), undefined);
  assert.match(attachmentProblem('a.png', text, false)!, /sessions started by Vineyard/);
  assert.match(attachmentProblem('a.png', new Uint8Array(MAX_IMAGE_BYTES + 1), true)!, /5 MB/);
  assert.match(attachmentProblem('a.txt', new Uint8Array(MAX_TEXT_BYTES + 1).fill(0x61), true)!, /512 KB/);
  assert.match(attachmentProblem('a.bin', new Uint8Array([1, 0, 2]), true)!, /binary/);
});
