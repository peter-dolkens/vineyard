/**
 * Runs inside the chat iframe before the chat UI (src/webview/main.ts) loads: stands in for VS Code's
 * webview API by posting to the app page (chatHost.ts), and does itself what has to happen inside the
 * tap that asked for it, which a message to the parent would be too late for on iOS: the file picker,
 * copying the session id, opening help links.
 */

import { MAX_IMAGE_BYTES } from '../core/attachments.ts';

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

function pickFiles(): void {
  if (!picker) {
    picker = document.createElement('input');
    picker.type = 'file';
    picker.multiple = true;
    picker.hidden = true;
    document.body.append(picker);
    picker.onchange = async () => {
      const files = [...(picker!.files ?? [])];
      picker!.value = '';
      if (!files.length) return;
      const out: { name: string; buffer: ArrayBuffer }[] = [];
      for (const f of files) {
        try {
          out.push(await prepare(f));
        } catch (err) {
          localStatus(`${f.name}: ${(err as Error).message}`, 'error');
        }
      }
      if (out.length) toParent({ type: 'attachFiles', files: out });
    };
  }
  picker.click();
}

const SENDABLE = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);
const MAX_SIDE = 2048;

/**
 * Photos from a phone are often HEIC, or JPEGs larger than the 5 MB the API takes: redraw those as a
 * JPEG no larger than 2048 px on a side. Everything else goes as picked.
 */
async function prepare(f: File): Promise<{ name: string; buffer: ArrayBuffer }> {
  const isImage = f.type.startsWith('image/') || /\.(heic|heif)$/i.test(f.name);
  if (!isImage || (SENDABLE.has(f.type) && f.size <= MAX_IMAGE_BYTES * 0.9)) return { name: f.name, buffer: await f.arrayBuffer() };
  const url = URL.createObjectURL(f);
  try {
    const img = new Image();
    img.src = url;
    await img.decode();
    const scale = Math.min(1, MAX_SIDE / Math.max(img.naturalWidth, img.naturalHeight));
    const canvas = document.createElement('canvas');
    canvas.width = Math.round(img.naturalWidth * scale);
    canvas.height = Math.round(img.naturalHeight * scale);
    canvas.getContext('2d')!.drawImage(img, 0, 0, canvas.width, canvas.height);
    const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/jpeg', 0.85));
    if (!blob) throw new Error('could not convert the image');
    return { name: f.name.replace(/\.[^.]+$/, '') + '.jpg', buffer: await blob.arrayBuffer() };
  } finally {
    URL.revokeObjectURL(url);
  }
}
