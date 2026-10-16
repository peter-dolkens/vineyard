import { test } from 'node:test';
import assert from 'node:assert/strict';
import { resumeCommand } from '../src/core/resume.ts';

test('a resumed session runs with the settings file its machine reports', () => {
  assert.equal(
    resumeCommand('abc', { os: 'darwin', vineyardSettings: '/Users/p/.vineyard/claude-settings.json' }),
    'claude --resume abc --settings /Users/p/.vineyard/claude-settings.json',
    'nothing for a shell to act on: no quotes, so every shell reads it the same',
  );
  assert.equal(
    resumeCommand('abc', { os: 'windows', vineyardSettings: 'C:\\Users\\pat\\.vineyard\\claude-settings.json' }),
    'claude --resume abc --settings C:\\Users\\pat\\.vineyard\\claude-settings.json',
    'the usual Windows path needs no quoting either',
  );
});

test('a path that needs quoting gets the quoting of its machine family', () => {
  assert.equal(
    resumeCommand('abc', { os: 'windows', vineyardSettings: 'C:\\Users\\Pat Smith\\.vineyard\\claude-settings.json' }),
    'claude --resume abc --settings "C:\\Users\\Pat Smith\\.vineyard\\claude-settings.json"',
    'cmd.exe and PowerShell both take the double-quoted form',
  );
  assert.equal(
    resumeCommand('abc', { os: 'darwin', vineyardSettings: '/Users/Pat Smith/.vineyard/claude-settings.json' }),
    `claude --resume abc --settings '/Users/Pat Smith/.vineyard/claude-settings.json'`,
  );
  assert.equal(
    resumeCommand('abc', { os: 'linux', vineyardSettings: "/home/o'brien/.vineyard/claude-settings.json" }),
    `claude --resume abc --settings '/home/o'\\''brien/.vineyard/claude-settings.json'`,
    'a quote in the path cannot end the quoting',
  );
});

test('a machine that reports no settings file is resumed exactly as before', () => {
  assert.equal(resumeCommand('abc', { os: 'linux' }), 'claude --resume abc', 'daemon from before the file');
  assert.equal(resumeCommand('abc', undefined), 'claude --resume abc', 'no host info at all');
});
