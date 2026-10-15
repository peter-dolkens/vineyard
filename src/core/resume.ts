/**
 * Resuming a session in a terminal, from the tree or the chat's / menu. The session runs with the
 * same Claude Code settings as one Vineyard manages, by naming the file the machine's daemon keeps
 * (see daemon/internal/config/claudesettings.go).
 */

import type { HostInfo } from './model.ts';

/**
 * The command to type into a terminal on that machine. The settings file is named only when the
 * machine reports one: Claude Code refuses to start on a --settings path that is not there, and a
 * daemon from before the file existed reports nothing.
 */
export function resumeCommand(sessionId: string, host: HostInfo | undefined): string {
  const settings = host?.vineyardSettings;
  if (!settings) return `claude --resume ${sessionId}`;
  return `claude --resume ${sessionId} --settings ${quoteFor(host?.os, settings)}`;
}

/**
 * One path, quoted for the shell the terminal opens on that machine: cmd.exe and PowerShell both
 * read a double-quoted path literally enough for a file name, every POSIX shell the single-quoted
 * form. A remote machine's command is quoted again on its way through ssh, which this survives.
 */
function quoteFor(os: string | undefined, path: string): string {
  if (os === 'windows') return `"${path}"`;
  return `'${path.replace(/'/g, `'\\''`)}'`;
}
