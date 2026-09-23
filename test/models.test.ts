import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { ModelInfo } from '../src/core/model.ts';
import { ALL_EFFORTS, effortOptions, modelOptions, selectedModel } from '../src/core/models.ts';

// What Claude Code 2.1.280 answered to initialize on a Team account, trimmed.
const models: ModelInfo[] = [
  { value: 'default', resolvedModel: 'claude-opus-5-5[1m]', displayName: 'Default (recommended)', description: 'Opus 5.5 with 1M context', supportedEffortLevels: ['low', 'medium', 'high', 'xhigh', 'max'] },
  { value: 'opus[1m]', resolvedModel: 'claude-opus-5-5[1m]', displayName: 'Opus (1M context)', supportedEffortLevels: ['low', 'medium', 'high', 'xhigh', 'max'] },
  { value: 'claude-fable-5-1[1m]', resolvedModel: 'claude-fable-5-1', displayName: 'Fable', supportedEffortLevels: ['low', 'medium', 'high', 'xhigh', 'max'] },
  { value: 'sonnet', resolvedModel: 'claude-sonnet-5', displayName: 'Sonnet', supportedEffortLevels: ['low', 'medium', 'high'] },
  { value: 'haiku', resolvedModel: 'claude-haiku-4-5-20251001', displayName: 'Haiku' },
];

test('modelOptions follows the harness list, default row becomes the empty value', () => {
  assert.deepEqual(
    modelOptions(models).map(([v, l]) => [v, l]),
    [
      ['', 'Default (recommended)'],
      ['opus[1m]', 'Opus (1M context)'],
      ['claude-fable-5-1[1m]', 'Fable'],
      ['sonnet', 'Sonnet'],
      ['haiku', 'Haiku'],
    ],
  );
  assert.equal(modelOptions(models)[0]?.[2], 'Opus 5.5 with 1M context');
  // Before the harness has answered there is nothing to list but the default.
  assert.deepEqual(modelOptions(undefined), [['', 'Default model']]);
  assert.deepEqual(modelOptions([]), [['', 'Default model']]);
});

test('selectedModel maps what the session reports onto a picker row', () => {
  assert.equal(selectedModel(models, ''), '');
  assert.equal(selectedModel(models, 'sonnet'), 'sonnet'); // what we asked for via set_model
  assert.equal(selectedModel(models, 'default'), '');
  assert.equal(selectedModel(models, 'claude-opus-5-5[1m]'), ''); // init reported the resolved default
  assert.equal(selectedModel(models, 'claude-opus-5-5'), ''); // transcript drops the context suffix
  assert.equal(selectedModel(models, 'claude-fable-5-1'), 'claude-fable-5-1[1m]');
  assert.equal(selectedModel(models, 'claude-mystery-9'), 'claude-mystery-9'); // unknown: keep so the picker shows it
  assert.equal(selectedModel(undefined, 'sonnet'), 'sonnet');
});

test('effortOptions narrows to what the selected model supports', () => {
  assert.deepEqual(effortOptions(models, 'sonnet').map(([v]) => v), ['', 'low', 'medium', 'high']);
  assert.deepEqual(effortOptions(models, '').map(([v]) => v), ['', 'low', 'medium', 'high', 'xhigh', 'max']);
  assert.deepEqual(effortOptions(models, 'haiku'), [['', 'Default effort']]);
  assert.equal(effortOptions(models, 'claude-mystery-9'), ALL_EFFORTS);
  assert.equal(effortOptions(undefined, 'sonnet'), ALL_EFFORTS);
});
