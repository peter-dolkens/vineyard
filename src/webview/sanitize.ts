/**
 * Markdown → DOM for transcript text. Everything the transcript carries is untrusted (tool output
 * fetched from the web, attached files, messages from other sessions), so the HTML marked produces
 * goes through DOMPurify before it reaches the page. Scripts are already stopped by the webview CSP;
 * this also drops forms, frames, embeds, styles, event handlers and javascript: URLs, and keeps
 * links, images (https:/data: only, as the CSP img-src), tables, code, details and the usual text
 * markup. Tags outside RAW_HTML_TAGS are escaped before parsing so they show as text (see rawHtml.ts).
 */

import DOMPurify from 'dompurify';
import { marked } from 'marked';
import { escapeUnknownTags, RAW_HTML_TAGS } from '../core/rawHtml.ts';

marked.setOptions({ gfm: true, breaks: false });

const HREF_RE = /^(https?:|mailto:|vscode:)/i;
const IMG_SRC_RE = /^(https:|data:image\/)/i;
const CODE_CLASS_RE = /^language-[\w+-]+$/;

const CONFIG = {
  // `input` is not in RAW_HTML_TAGS (a raw one in the text is escaped) but marked emits it for GFM
  // task lists; the element hook keeps only disabled checkboxes.
  ALLOWED_TAGS: [...RAW_HTML_TAGS, 'input'],
  ALLOWED_ATTR: ['href', 'title', 'alt', 'src', 'width', 'height', 'align', 'colspan', 'rowspan', 'start', 'open', 'type', 'checked', 'disabled', 'class'],
  ALLOW_DATA_ATTR: false,
  ALLOW_ARIA_ATTR: false,
  ALLOW_UNKNOWN_PROTOCOLS: false,
  KEEP_CONTENT: true,
};

DOMPurify.addHook('uponSanitizeElement', (node, data) => {
  if (data.tagName === 'input') {
    const input = node as Element;
    if (input.getAttribute('type') !== 'checkbox') input.parentNode?.removeChild(input);
    else input.setAttribute('disabled', '');
  }
});

DOMPurify.addHook('uponSanitizeAttribute', (node, data) => {
  const tag = node.tagName.toLowerCase();
  const value = data.attrValue.trim();
  switch (data.attrName) {
    case 'href':
      if (!HREF_RE.test(value)) data.keepAttr = false;
      break;
    case 'src':
      if (tag !== 'img' || !IMG_SRC_RE.test(value)) data.keepAttr = false;
      break;
    case 'class':
      if (tag !== 'code' || !CODE_CLASS_RE.test(value)) data.keepAttr = false;
      break;
    case 'type':
    case 'checked':
    case 'disabled':
      if (tag !== 'input') data.keepAttr = false;
      break;
    case 'open':
      if (tag !== 'details') data.keepAttr = false;
      break;
  }
});

/** Anchors open outside the webview: VS Code turns a target="_blank" click into openExternal. */
DOMPurify.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName === 'A' && node.hasAttribute('href')) {
    node.setAttribute('target', '_blank');
    node.setAttribute('rel', 'noopener noreferrer');
  }
});

/** Sanitised HTML string for an HTML fragment that may carry transcript-derived markup. */
export function safeHtml(html: string): string {
  return DOMPurify.sanitize(html, CONFIG);
}

/** Markdown → element. breaks=true keeps single newlines (user prompts are plain text, not Markdown). */
export function md(text: string, breaks = false): HTMLElement {
  const d = document.createElement('div');
  d.className = 'md';
  const html = marked.parse(escapeUnknownTags(text), { breaks }) as string;
  d.appendChild(DOMPurify.sanitize(html, { ...CONFIG, RETURN_DOM_FRAGMENT: true }));
  return d;
}
