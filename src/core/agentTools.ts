/**
 * The vineyard.agentTools settings: which of Vineyard's agent tools the sessions it starts get (the
 * daemon's `vineyardd mcp` server). Like the web app's setting, each choice carries when it was made
 * and a daemon keeps the later one, so this only decides whether a window has something newer and
 * different to tell a machine (src/extension/agentTools.ts sends it).
 */

import type { AgentToolsStatus } from './model.ts';

/** Setting name → the tool group the daemon knows it by (model.AgentTool* in Go). */
export const AGENT_TOOL_SETTINGS: Record<string, string> = {
  listMachines: 'machines',
  listWorkspaces: 'workspaces',
  listSessions: 'sessions',
  fetchTranscripts: 'transcripts',
  driveAgents: 'drive',
  sendFiles: 'files',
};

/** The tool groups that settings turn on, sorted as the daemon reports them. */
export function wantedTools(get: (setting: string) => boolean): string[] {
  return Object.entries(AGENT_TOOL_SETTINGS)
    .filter(([setting]) => get(setting))
    .map(([, tool]) => tool)
    .sort();
}

/**
 * Whether to send `tools` (chosen at `at`) to a machine reporting `st`. A daemon older than agent
 * tools reports nothing: asking it to keep none is pointless, asking for some is worth one try.
 */
export function shouldSendTools(st: AgentToolsStatus | undefined, tools: string[], at: number): boolean {
  if (!at) return false;
  if (!st) return tools.length > 0;
  if ((st.at ?? 0) >= at) return false;
  const have = [...(st.tools ?? [])].sort();
  return have.length !== tools.length || have.some((t, i) => t !== tools[i]);
}
