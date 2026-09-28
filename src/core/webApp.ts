/**
 * When VS Code should tell a machine to turn its web app on or off (src/extension/webApp.ts). Each
 * choice carries the time it was made and a daemon keeps the later one, so this only decides whether
 * this window has something newer and different to say.
 */

import type { WebAppStatus } from './model.ts';

export interface WebAppWish {
  /** Whether the setting is on and confirmed in this VS Code install. */
  on: boolean;
  port: number;
  /** Machine names or ids to serve it on; empty means every machine. */
  machines: string[];
}

/** Where this machine should serve the app: ":<port>", or "" for off. */
export function wantedListen(w: WebAppWish, machine: { id: string; name: string }): string {
  if (!w.on) return '';
  if (w.machines.length && !w.machines.some((x) => x === machine.id || x.toLowerCase() === machine.name.toLowerCase())) return '';
  return `:${w.port}`;
}

/**
 * Whether to send `listen` (chosen at `at`) to a machine reporting `st`. A daemon older than the web
 * app reports nothing: asking it to stay off is pointless, asking it to turn on is worth one try.
 */
export function shouldSend(st: WebAppStatus | undefined, listen: string, at: number): boolean {
  if (!at) return false; // never chosen in this window
  if (!st) return listen !== '';
  if ((st.at ?? 0) >= at) return false; // decided later elsewhere, or already told
  return (st.listen ?? '') !== listen;
}
