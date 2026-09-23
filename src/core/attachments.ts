/**
 * Files attached to a prompt from the chat: what type to declare and whether they can travel as text.
 * The daemon turns images into image blocks (managed sessions only) and inlines everything else.
 */

/** Anthropic accepts images up to 5 MB; text is capped lower so one file cannot swamp the context. */
export const MAX_IMAGE_BYTES = 5 * 1024 * 1024;
export const MAX_TEXT_BYTES = 512 * 1024;

const IMAGE_TYPES: Record<string, string> = { png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp' };
const TEXT_TYPES: Record<string, string> = { md: 'text/markdown', json: 'application/json', csv: 'text/csv', html: 'text/html', xml: 'text/xml', yaml: 'text/yaml', yml: 'text/yaml' };

/** The media type to declare for a file name; anything not recognised is sent as plain text. */
export function attachmentMediaType(name: string): string {
  const ext = name.toLowerCase().split('.').pop() ?? '';
  return IMAGE_TYPES[ext] ?? TEXT_TYPES[ext] ?? 'text/plain';
}

export function isImageType(mediaType: string): boolean {
  return Object.values(IMAGE_TYPES).includes(mediaType);
}

/** A NUL byte in the first 8 KB means this is not text and cannot be inlined. */
export function looksBinary(bytes: Uint8Array): boolean {
  const n = Math.min(bytes.length, 8192);
  for (let i = 0; i < n; i++) if (bytes[i] === 0) return true;
  return false;
}

/**
 * Why a file cannot be attached, or undefined when it can. `managed` says whether the session takes
 * image blocks (only sessions Vineyard started do).
 */
export function attachmentProblem(name: string, bytes: Uint8Array, managed: boolean): string | undefined {
  const type = attachmentMediaType(name);
  if (isImageType(type)) {
    if (!managed) return 'images can only be sent to sessions started by Vineyard';
    if (bytes.length > MAX_IMAGE_BYTES) return 'images up to 5 MB';
    return undefined;
  }
  if (bytes.length > MAX_TEXT_BYTES) return 'text files up to 512 KB';
  if (looksBinary(bytes)) return 'binary files cannot be attached';
  return undefined;
}
