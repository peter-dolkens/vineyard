import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SessionPrefStore } from '../src/extension/sessionPrefs.ts';

/** In-memory stand-in for vscode.Memento. */
function memento() {
  const store = new Map<string, unknown>();
  return {
    keys: () => [...store.keys()],
    get: <T>(key: string, def?: T) => (store.has(key) ? (store.get(key) as T) : (def as T)),
    update: async (key: string, value: unknown) => void store.set(key, value),
  };
}

test('remembers model and effort per machine and workspace', async () => {
  const prefs = new SessionPrefStore(memento());
  assert.deepEqual(prefs.get('m1', '/a'), {});
  await prefs.remember('m1', '/a', { model: 'claude-opus-5' });
  await prefs.remember('m1', '/a', { effort: 'high' });
  assert.deepEqual(prefs.get('m1', '/a'), { model: 'claude-opus-5', effort: 'high' });
  assert.deepEqual(prefs.get('m1', '/b'), {});
  assert.deepEqual(prefs.get('m2', '/a'), {});
});

test('choosing the default forgets the field', async () => {
  const prefs = new SessionPrefStore(memento());
  await prefs.remember('m1', '/a', { model: 'claude-opus-5', effort: 'max' });
  await prefs.remember('m1', '/a', { model: '' });
  assert.deepEqual(prefs.get('m1', '/a'), { effort: 'max' });
  await prefs.remember('m1', '/a', { effort: '' });
  assert.deepEqual(prefs.get('m1', '/a'), {});
});

test('an undefined field leaves the memory alone', async () => {
  const prefs = new SessionPrefStore(memento());
  await prefs.remember('m1', '/a', { model: 'claude-sonnet-5' });
  await prefs.remember('m1', '/a', { effort: 'low', model: undefined });
  assert.deepEqual(prefs.get('m1', '/a'), { model: 'claude-sonnet-5', effort: 'low' });
});
