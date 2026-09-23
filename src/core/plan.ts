/**
 * Plan review (ExitPlanMode) helpers shared by the webview card and its tests.
 *
 * Claude Code only honours `updatedPermissions` entries it offered itself in the control request's
 * `permission_suggestions` (the SDK's PermissionUpdate shape: `{type:'setMode', mode, destination}`,
 * `{type:'addRules', rules, behavior, destination}`, ...). So "Yes, and don't ask again" can only ride
 * on a suggestion; when none was offered the caller switches the mode with set_permission_mode instead.
 */

const ACCEPT_EDITS = 'acceptEdits';

function kind(s: unknown): { type?: string; mode?: string } {
  return s && typeof s === 'object' ? (s as { type?: string; mode?: string }) : {};
}

/**
 * The suggestions to send with an allow that should also switch the session to acceptEdits: the
 * offered setMode(acceptEdits) entry plus any addRules entries. Undefined when Claude Code offered no
 * such mode switch, in which case the caller falls back to a plain allow followed by a configure.
 */
export function acceptEditsSuggestions(suggestions: unknown[] | undefined): unknown[] | undefined {
  const list = Array.isArray(suggestions) ? suggestions : [];
  const setMode = list.find((s) => kind(s).type === 'setMode' && kind(s).mode === ACCEPT_EDITS);
  if (!setMode) return undefined;
  return [setMode, ...list.filter((s) => kind(s).type === 'addRules')];
}

/** The plan text of an ExitPlanMode request, or '' when the input carries none. */
export function planText(input: unknown): string {
  const plan = input && typeof input === 'object' ? (input as { plan?: unknown }).plan : undefined;
  return typeof plan === 'string' ? plan : '';
}

export const PLAN_REJECTED = 'User rejected the plan; keep planning.';
