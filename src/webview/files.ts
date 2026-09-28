/**
 * Files the chat gets from the page itself rather than a host dialog: pasted into the message box,
 * dropped on the composer, or (in the web app) picked with its own file input. They are readied here
 * (named, oversized images scaled down), given an id, and posted to the host as `attachFiles`; the
 * host checks them and queues them like files from the "+" button. Images keep a data URL here so the
 * chips and the local echo can show a thumbnail.
 */

import { MAX_IMAGE_BYTES, attachmentMediaType, isImageType, pastedFileName } from '../core/attachments.ts';

export interface ReadyFile {
  id: string;
  name: string;
  buffer: ArrayBuffer;
}

/** Data URLs of queued images, by attachment id; the chips and the echo draw from it. */
export const thumbnails = new Map<string, string>();

const SENDABLE = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);
const MAX_SIDE = 2048;

function newId(): string {
  const b = new Uint8Array(8);
  crypto.getRandomValues(b);
  return 'f' + [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}

/** The files a paste or drop carries, if any. */
export function filesIn(dt: DataTransfer | null): File[] {
  if (!dt) return [];
  if (dt.files?.length) return [...dt.files];
  // Some clipboards list an image only as an item.
  return [...(dt.items ?? [])].filter((i) => i.kind === 'file').map((i) => i.getAsFile()).filter((f): f is File => !!f);
}

/**
 * Ready files and post them to the host in one `attachFiles`. `fail` reports a file that could not
 * be read or converted; the rest still go.
 */
export async function queueFiles(files: File[], post: (m: unknown) => void, fail: (text: string) => void): Promise<void> {
  const now = new Date();
  const out: ReadyFile[] = [];
  let unnamed = 0;
  for (const f of files) {
    const generic = !f.name || /^image\.\w+$/i.test(f.name);
    const name = pastedFileName(f.name, f.type, now, generic ? unnamed++ : 0);
    try {
      const ready = await prepare(f, name);
      out.push(ready);
      const type = attachmentMediaType(ready.name);
      if (isImageType(type)) thumbnails.set(ready.id, `data:${type};base64,${base64(new Uint8Array(ready.buffer))}`);
    } catch (err) {
      fail(`${name}: ${(err as Error).message}`);
    }
  }
  if (out.length) post({ type: 'attachFiles', files: out });
}

/**
 * Screenshots from a large display, and phone photos (often HEIC), can be over the 5 MB the API
 * takes or in a format it does not: redraw those no larger than 2048 px on a side, as PNG when that
 * fits and JPEG otherwise. Everything else goes as it is.
 */
async function prepare(f: File, name: string): Promise<ReadyFile> {
  const isImage = f.type.startsWith('image/') || /\.(heic|heif)$/i.test(name);
  if (!isImage || (SENDABLE.has(f.type) && f.size <= MAX_IMAGE_BYTES * 0.9)) return { id: newId(), name, buffer: await f.arrayBuffer() };
  const canvas = await draw(f);
  const stem = name.replace(/\.[^.]+$/, '');
  if (f.type === 'image/png') {
    const png = await toBlob(canvas, 'image/png');
    if (png && png.size <= MAX_IMAGE_BYTES * 0.9) return { id: newId(), name: `${stem}.png`, buffer: await png.arrayBuffer() };
  }
  const jpeg = await toBlob(canvas, 'image/jpeg', 0.85);
  if (!jpeg) throw new Error('could not convert the image');
  return { id: newId(), name: `${stem}.jpg`, buffer: await jpeg.arrayBuffer() };
}

/** The image on a canvas, scaled to fit MAX_SIDE. createImageBitmap first: it needs no blob: URL,
 * which VS Code's webview policy does not allow for images. */
async function draw(f: File): Promise<HTMLCanvasElement> {
  let src: CanvasImageSource & { width: number; height: number };
  let url: string | undefined;
  try {
    src = await createImageBitmap(f);
  } catch {
    url = URL.createObjectURL(f);
    const img = new Image();
    img.src = url;
    await img.decode();
    src = Object.assign(img, { width: img.naturalWidth, height: img.naturalHeight });
  }
  try {
    const scale = Math.min(1, MAX_SIDE / Math.max(src.width, src.height));
    const canvas = document.createElement('canvas');
    canvas.width = Math.round(src.width * scale);
    canvas.height = Math.round(src.height * scale);
    canvas.getContext('2d')!.drawImage(src, 0, 0, canvas.width, canvas.height);
    return canvas;
  } finally {
    if (url) URL.revokeObjectURL(url);
    if ('close' in src && typeof src.close === 'function') src.close();
  }
}

function toBlob(canvas: HTMLCanvasElement, type: string, quality?: number): Promise<Blob | null> {
  return new Promise((resolve) => canvas.toBlob(resolve, type, quality));
}

function base64(bytes: Uint8Array): string {
  let s = '';
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(s);
}

/**
 * Paste into `input` and drop onto `zone` attach files when the clipboard or drag carries any; plain
 * text pastes and drags are left to the browser. `enabled` says whether the session takes messages now.
 */
export function wireFileInputs(input: HTMLTextAreaElement, zone: HTMLElement, enabled: () => boolean, post: (m: unknown) => void, fail: (text: string) => void): void {
  input.addEventListener('paste', (e) => {
    const files = filesIn(e.clipboardData);
    if (!files.length || !enabled()) return;
    e.preventDefault();
    void queueFiles(files, post, fail);
  });
  const carriesFiles = (e: DragEvent) => !!e.dataTransfer && [...e.dataTransfer.types].includes('Files');
  let depth = 0;
  zone.addEventListener('dragenter', (e) => {
    if (!carriesFiles(e) || !enabled()) return;
    e.preventDefault();
    if (depth++ === 0) zone.classList.add('drop-target');
  });
  zone.addEventListener('dragover', (e) => {
    if (!carriesFiles(e) || !enabled()) return;
    e.preventDefault();
    e.dataTransfer!.dropEffect = 'copy';
  });
  zone.addEventListener('dragleave', () => {
    if (depth > 0 && --depth === 0) zone.classList.remove('drop-target');
  });
  zone.addEventListener('drop', (e) => {
    depth = 0;
    zone.classList.remove('drop-target');
    const files = filesIn(e.dataTransfer);
    if (!files.length || !enabled()) return;
    e.preventDefault();
    void queueFiles(files, post, fail);
  });
}
