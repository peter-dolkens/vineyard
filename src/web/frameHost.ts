/**
 * Runs inside the chat iframe before the chat UI (src/webview/main.ts) loads: stands in for VS Code's
 * webview API by posting to the app page (chatHost.ts), and does itself what has to happen inside the
 * tap that asked for it, which a message to the parent would be too late for on iOS: the file picker,
 * copying the session id, opening help links.
 */

import { queueFiles } from '../webview/files.ts';

const HELP_URL = 'https://github.com/peter-dolkens/vineyard#readme';
const ISSUES_URL = 'https://github.com/peter-dolkens/vineyard/issues/new';

const touch = matchMedia('(pointer: coarse)').matches;
document.documentElement.classList.toggle('touch', touch);

(globalThis as { vineyardHost?: unknown }).vineyardHost = {
  // On a phone Return makes a new line and the button sends, as in Messages.
  enterSends: !touch,
  touch,
  // No VS Code window, terminal or Markdown preview on this side.
  hideActions: ['openWorkspace', 'openTerminal', 'resumeTerminal', 'rawTranscript'],
};

let sessionId: string | undefined;
window.addEventListener('message', (ev) => {
  if (ev.source !== parent) return;
  const m = ev.data as { type?: string; agent?: { sessionId?: string } };
  if ((m.type === 'init' || m.type === 'agent') && m.agent?.sessionId) sessionId = m.agent.sessionId;
});

export function toParent(m: unknown): void {
  parent.postMessage(m, location.origin);
}

/** Show a status banner in the chat without a round trip. */
function localStatus(text: string, kind: 'ok' | 'error' | 'info'): void {
  window.dispatchEvent(new MessageEvent('message', { data: { type: 'status', text, kind }, source: parent }));
}

(globalThis as { acquireVsCodeApi?: unknown }).acquireVsCodeApi = () => ({
  postMessage(m: { type?: string; id?: string }) {
    if (m.type === 'attach') return pickFiles();
    if (m.type === 'action' && m.id === 'copySessionId') {
      if (sessionId) localStatus(copy(sessionId) ? 'Session ID copied.' : `Session ID: ${sessionId}`, 'ok');
      return;
    }
    if (m.type === 'action' && (m.id === 'help' || m.id === 'report')) {
      window.open(m.id === 'help' ? HELP_URL : ISSUES_URL, '_blank', 'noopener');
      return;
    }
    toParent(m);
  },
});

function copy(text: string): boolean {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0;font-size:16px';
  document.body.append(ta);
  ta.select();
  ta.setSelectionRange(0, text.length);
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  ta.remove();
  return ok;
}

let picker: HTMLInputElement | undefined;

/** The "+" button. Picked files take the same road as pasted ones (webview/files.ts). */
function pickFiles(): void {
  if (!picker) {
    picker = document.createElement('input');
    picker.type = 'file';
    picker.multiple = true;
    picker.hidden = true;
    document.body.append(picker);
    picker.onchange = () => {
      const files = [...(picker!.files ?? [])];
      picker!.value = '';
      if (files.length) void queueFiles(files, toParent, (text) => localStatus(text, 'error'));
    };
  }
  picker.click();
}
