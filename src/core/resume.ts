/**
 * Resuming a session in a terminal, from the tree or the chat's / menu. The session runs with the
 * same Claude Code settings as one Vineyard manages, by naming the file the machine's daemon keeps
 * (see daemon/internal/config/claudesettings.go).
 */

import type { HostInfo } from './model.ts';

/**
 * The command to type into a terminal on that machine. The settings file is named only when the
 * machine reports one: Claude Code refuses to start on a --settings path that is not there, a
 * daemon from before the file existed reports nothing, and a daemon whose file has gone missing
 * stops reporting it.
 */
export function resumeCommand(sessionId: string, host: HostInfo | undefined): string {
  const settings = host?.vineyardSettings;
  if (!settings) return `claude --resume ${sessionId}`;
  return `claude --resume ${sessionId} --settings ${quotePath(settings, host?.os)}`;
}

/** A path with nothing in it for any shell to act on: the usual ~/.vineyard/claude-settings.json. */
const PLAIN = /^[A-Za-z0-9_\-./~]+$/;
/** The same on Windows, where the drive letter and separators are the only extras. */
const PLAIN_WINDOWS = /^[A-Za-z]:[\\/][A-Za-z0-9_\-.\\/]*$/;

/**
 * One path for the shell the terminal happens to open with. A path that needs no quoting is left
 * bare, which every shell reads the same way; the rest are quoted for the machine's family, since
 * cmd.exe and PowerShell understand a double-quoted path and POSIX shells a single-quoted one.
 */
function quotePath(path: string, os: string | undefined): string {
  const windows = os === 'windows';
  if ((windows ? PLAIN_WINDOWS : PLAIN).test(path)) return path;
  if (windows) return `"${path}"`;
  return `'${path.replace(/'/g, `'\\''`)}'`;
}
