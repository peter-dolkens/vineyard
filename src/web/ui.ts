/**
 * Small iOS-style UI kit for the web app: grouped lists patched in place (a row keeps its element
 * across updates, so a snapshot arriving mid-tap never swallows the tap), action sheets, alerts,
 * a text prompt and toasts.
 */

import type { AgentState } from '../core/model.ts';

export function h<K extends keyof HTMLElementTagNameMap>(tag: K, cls?: string, text?: string): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function icon(name: string, extra = ''): HTMLElement {
  return h('i', `codicon codicon-${name}${extra ? ' ' + extra : ''}`);
}

export const STATE_ICON: Record<AgentState, string> = {
  question: 'question',
  permission: 'shield',
  working: 'loading',
  thinking: 'lightbulb',
  tool: 'tools',
  shell: 'terminal',
  idle: 'circle-large-filled',
  done: 'pass',
  exited: 'circle-slash',
  unknown: 'circle-outline',
};

export const STATE_TINT: Record<AgentState, string> = {
  question: 'var(--orange)',
  permission: 'var(--orange)',
  working: 'var(--blue)',
  thinking: 'var(--purple)',
  tool: 'var(--blue)',
  shell: 'var(--orange)',
  idle: 'var(--green)',
  done: 'var(--green)',
  exited: 'var(--gray)',
  unknown: 'var(--gray)',
};

// ---- grouped lists ------------------------------------------------------------------------------

export interface Row {
  key: string;
  icon?: string;
  /** Background of the icon square; a CSS colour. */
  tint?: string;
  spin?: boolean;
  title: string;
  sub?: string;
  /** A third, dimmer line (e.g. the last prompt). */
  note?: string;
  value?: string;
  badge?: string;
  chevron?: boolean;
  /** A button-like row: blue text, no chevron. */
  action?: boolean;
  destructive?: boolean;
  disabled?: boolean;
  indent?: number;
  onTap?: () => void;
  /** Trailing ⓘ button: details for this row. */
  onInfo?: () => void;
  toggle?: { on: boolean; onChange: (on: boolean) => void };
  /** Trailing small button, e.g. Stop on a running task. */
  button?: { label: string; destructive?: boolean; busy?: boolean; onTap: () => void };
}

export interface Section {
  key: string;
  header?: string;
  footer?: string;
  rows?: Row[];
  /** A custom block instead of rows (a status card, a text area); kept as given. */
  block?: HTMLElement;
  /** Rows only: no rounded group (a plain list inside a card, e.g.). */
  plain?: boolean;
}

interface RowEl extends HTMLDivElement {
  _parts?: {
    ic: HTMLElement;
    glyph: HTMLElement;
    t: HTMLElement;
    s: HTMLElement;
    n: HTMLElement;
    v: HTMLElement;
    b: HTMLElement;
    info: HTMLButtonElement;
    btn: HTMLButtonElement;
    chev: HTMLElement;
    sw: HTMLLabelElement;
    swIn: HTMLInputElement;
  };
  _spec?: Row;
}

function setText(e: HTMLElement, text: string | undefined): void {
  const v = text ?? '';
  if (e.textContent !== v) e.textContent = v;
  e.hidden = !v;
}

function setClass(e: Element, cls: string): void {
  if (e.getAttribute('class') !== cls) e.setAttribute('class', cls);
}

function buildRow(): RowEl {
  const el = h('div', 'row') as RowEl;
  const ic = h('span', 'ic');
  const glyph = icon('circle-outline');
  ic.append(glyph);
  const txt = h('div', 'txt');
  const t = h('div', 't');
  const s = h('div', 's');
  const n = h('div', 'n');
  txt.append(t, s, n);
  const v = h('span', 'val');
  const b = h('span', 'badge');
  const btn = h('button', 'rowbtn');
  const info = h('button', 'info');
  info.setAttribute('aria-label', 'Details');
  info.append(icon('info'));
  const chev = icon('chevron-right', 'chev');
  const sw = h('label', 'switch');
  const swIn = h('input');
  swIn.type = 'checkbox';
  sw.append(swIn, h('span', 'knob'));
  el.append(ic, txt, v, b, btn, info, sw, chev);
  el._parts = { ic, glyph, t, s, n, v, b, info, btn, chev, sw, swIn };
  el.onclick = (e) => {
    const spec = el._spec;
    if (!spec || spec.disabled || spec.toggle) return;
    if ((e.target as HTMLElement).closest('button.info, button.rowbtn, label.switch')) return;
    spec.onTap?.();
  };
  info.onclick = (e) => {
    e.stopPropagation();
    el._spec?.onInfo?.();
  };
  btn.onclick = (e) => {
    e.stopPropagation();
    el._spec?.button?.onTap();
  };
  swIn.onchange = () => el._spec?.toggle?.onChange(swIn.checked);
  return el;
}

function patchRow(el: RowEl, r: Row): void {
  el._spec = r;
  const p = el._parts!;
  const tappable = !!(r.onTap && !r.disabled && !r.toggle);
  setClass(el, ['row', tappable && 'tappable', r.action && 'action', r.destructive && 'destructive', r.disabled && 'disabled', r.icon ? 'has-ic' : ''].filter(Boolean).join(' '));
  el.style.setProperty('--indent', String(r.indent ?? 0));
  p.ic.hidden = !r.icon;
  if (r.icon) {
    setClass(p.glyph, `codicon codicon-${r.icon}${r.spin ? ' codicon-modifier-spin' : ''}`);
    const tint = r.tint ?? 'var(--gray)';
    if (p.ic.style.getPropertyValue('--tint') !== tint) p.ic.style.setProperty('--tint', tint);
  }
  setText(p.t, r.title);
  setText(p.s, r.sub);
  setText(p.n, r.note);
  setText(p.v, r.value);
  setText(p.b, r.badge);
  p.info.hidden = !r.onInfo;
  p.btn.hidden = !r.button;
  if (r.button) {
    setText(p.btn, r.button.label);
    p.btn.disabled = !!r.button.busy;
    setClass(p.btn, 'rowbtn' + (r.button.destructive ? ' destructive' : ''));
  }
  p.sw.hidden = !r.toggle;
  if (r.toggle && p.swIn.checked !== r.toggle.on) p.swIn.checked = r.toggle.on;
  p.chev.hidden = !(r.chevron ?? (tappable && !r.action && !r.destructive));
}

function patchRows(group: HTMLElement, rows: Row[]): void {
  const existing = new Map<string, RowEl>();
  for (const c of [...group.children] as RowEl[]) if (c.dataset.key) existing.set(c.dataset.key, c);
  let prev: Element | null = null;
  for (const r of rows) {
    let el = existing.get(r.key);
    if (el) existing.delete(r.key);
    else {
      el = buildRow();
      el.dataset.key = r.key;
    }
    patchRow(el, r);
    const want: Element | null = prev ? prev.nextElementSibling : group.firstElementChild;
    if (want !== el) group.insertBefore(el, want);
    prev = el;
  }
  for (const el of existing.values()) el.remove();
}

/** Lay out sections in `root`, reusing every section and row element whose key is unchanged. */
export function renderSections(root: HTMLElement, sections: Section[]): void {
  const existing = new Map<string, HTMLElement>();
  for (const c of [...root.children] as HTMLElement[]) if (c.dataset.key) existing.set(c.dataset.key, c);
  let prev: Element | null = null;
  for (const s of sections) {
    let el = existing.get(s.key);
    if (el) existing.delete(s.key);
    else {
      el = h('section', 'section');
      el.dataset.key = s.key;
      el.append(h('div', 'section-h'), h('div', 'group'), h('div', 'section-f'));
    }
    const [head, group, foot] = [...el.children] as HTMLElement[];
    setText(head!, s.header);
    setText(foot!, s.footer);
    setClass(group!, s.block ? 'block' : s.plain ? 'group plain' : 'group');
    if (s.block) {
      if (group!.firstElementChild !== s.block || group!.children.length !== 1) group!.replaceChildren(s.block);
    } else {
      if (group!.firstElementChild && !(group!.firstElementChild as HTMLElement).dataset.key) group!.replaceChildren();
      patchRows(group!, s.rows ?? []);
    }
    group!.hidden = !s.block && !(s.rows ?? []).length;
    const want: Element | null = prev ? prev.nextElementSibling : root.firstElementChild;
    if (want !== el) root.insertBefore(el, want);
    prev = el;
  }
  for (const el of existing.values()) el.remove();
}

// ---- overlays -----------------------------------------------------------------------------------

const overlayRoot = () => document.getElementById('overlays')!;

function backdrop(onClose: () => void): HTMLElement {
  const b = h('div', 'backdrop');
  b.onclick = (e) => {
    if (e.target === b) onClose();
  };
  overlayRoot().append(b);
  requestAnimationFrame(() => b.classList.add('shown'));
  return b;
}

function dismiss(b: HTMLElement): void {
  b.classList.remove('shown');
  b.classList.add('leaving');
  setTimeout(() => b.remove(), 260);
}

export interface SheetItem {
  label: string;
  detail?: string;
  icon?: string;
  destructive?: boolean;
  disabled?: boolean;
  run: () => void;
}

/** A bottom action sheet; picking an item closes it and runs the item (inside the tap). */
export function actionSheet(title: string | undefined, items: SheetItem[], message?: string, onCancel?: () => void): void {
  const cancelled = () => {
    dismiss(b);
    onCancel?.();
  };
  const b = backdrop(cancelled);
  const sheet = h('div', 'sheet');
  const group = h('div', 'sheet-group');
  if (title || message) {
    const head = h('div', 'sheet-head');
    if (title) head.append(h('div', 'sheet-title', title));
    if (message) head.append(h('div', 'sheet-msg', message));
    group.append(head);
  }
  for (const it of items) {
    const btn = h('button', 'sheet-item' + (it.destructive ? ' destructive' : ''));
    if (it.icon) btn.append(icon(it.icon));
    const txt = h('span', 'sheet-text');
    txt.append(h('span', 'sheet-label', it.label));
    if (it.detail) txt.append(h('span', 'sheet-detail', it.detail));
    btn.append(txt);
    btn.disabled = !!it.disabled;
    btn.onclick = () => {
      dismiss(b);
      it.run();
    };
    group.append(btn);
  }
  const cancel = h('button', 'sheet-item cancel', 'Cancel');
  cancel.onclick = cancelled;
  sheet.append(group, cancel);
  b.append(sheet);
}

export interface DialogOptions {
  title: string;
  message?: string;
  /** Buttons, most important last (iOS puts the default on the right); the first is Cancel unless given. */
  buttons?: { label: string; value: string; destructive?: boolean; primary?: boolean }[];
  /** Adds a text field; its value is returned alongside the button pressed. */
  input?: { value?: string; placeholder?: string; multiline?: boolean; type?: string };
  /** Extra content under the message (a link to open, a code to copy). */
  body?: HTMLElement;
}

/** An alert. Resolves with the pressed button's value (undefined for Cancel) and the field's text. */
export function dialog(o: DialogOptions): Promise<{ button?: string; text: string }> {
  return new Promise((resolve) => {
    let field: HTMLInputElement | HTMLTextAreaElement | undefined;
    const done = (button?: string) => {
      dismiss(b);
      resolve({ button, text: field?.value ?? '' });
    };
    const b = backdrop(() => done(undefined));
    b.classList.add('center');
    const box = h('div', 'alert');
    box.append(h('div', 'alert-title', o.title));
    if (o.message) box.append(h('div', 'alert-msg', o.message));
    if (o.body) box.append(o.body);
    if (o.input) {
      field = o.input.multiline ? h('textarea', 'alert-input') : h('input', 'alert-input');
      if (field instanceof HTMLInputElement) field.type = o.input.type ?? 'text';
      field.value = o.input.value ?? '';
      field.placeholder = o.input.placeholder ?? '';
      field.setAttribute('autocapitalize', 'off');
      field.setAttribute('autocorrect', 'off');
      field.spellcheck = false;
      field.onkeydown = (e) => {
        if (e.key === 'Enter' && !(field instanceof HTMLTextAreaElement)) {
          e.preventDefault();
          const primary = (o.buttons ?? []).find((x) => x.primary) ?? o.buttons?.[o.buttons.length - 1];
          done(primary?.value);
        }
      };
      box.append(field);
    }
    const row = h('div', 'alert-buttons');
    const buttons = o.buttons ?? [{ label: 'OK', value: 'ok', primary: true }];
    const all = buttons.some((x) => x.value === 'cancel') ? buttons : [{ label: 'Cancel', value: 'cancel' }, ...buttons];
    if (all.length > 2) row.classList.add('stacked');
    for (const btn of all) {
      const e = h('button', 'alert-btn' + (btn.destructive ? ' destructive' : '') + (btn.primary ? ' primary' : ''), btn.label);
      e.onclick = () => done(btn.value === 'cancel' ? undefined : btn.value);
      row.append(e);
    }
    box.append(row);
    b.append(box);
    if (field) setTimeout(() => field!.focus(), 50);
  });
}

export async function confirm(title: string, message: string | undefined, label: string, destructive = false): Promise<boolean> {
  const r = await dialog({ title, message, buttons: [{ label, value: 'yes', destructive, primary: !destructive }] });
  return r.button === 'yes';
}

export async function prompt(title: string, opts: { message?: string; value?: string; placeholder?: string; label?: string; type?: string } = {}): Promise<string | undefined> {
  const r = await dialog({ title, message: opts.message, input: { value: opts.value, placeholder: opts.placeholder, type: opts.type }, buttons: [{ label: opts.label ?? 'OK', value: 'ok', primary: true }] });
  return r.button === 'ok' ? r.text : undefined;
}

let toastTimer: number | undefined;

/** A banner from the top; tapping it runs onTap. Errors stay until tapped or replaced. */
export function toast(text: string, kind: 'info' | 'ok' | 'error' | 'attention' = 'info', onTap?: () => void): void {
  let t = document.getElementById('toast');
  if (!t) {
    t = h('div', 'toast');
    t.id = 'toast';
    overlayRoot().append(t);
  }
  t.textContent = text;
  t.className = `toast ${kind}`;
  t.onclick = () => {
    t!.classList.remove('shown');
    onTap?.();
  };
  requestAnimationFrame(() => t!.classList.add('shown'));
  clearTimeout(toastTimer);
  if (kind !== 'error') toastTimer = window.setTimeout(() => t!.classList.remove('shown'), kind === 'attention' ? 6000 : 3000);
}

/**
 * Copy inside a tap. navigator.clipboard needs a secure context, which a phone opening
 * http://<lan address> is not, so the old selection trick is the fallback.
 */
export function copyText(text: string): boolean {
  if (window.isSecureContext && navigator.clipboard) {
    void navigator.clipboard.writeText(text);
    return true;
  }
  const ta = h('textarea');
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

/** A value with a Copy button under it, for dialogs (invite codes, session ids, paths). */
export function copyBlock(value: string): HTMLElement {
  const wrap = h('div', 'copy-block');
  const code = h('code', 'copy-value', value);
  const btn = h('button', 'copy-btn', 'Copy');
  btn.onclick = () => {
    btn.textContent = copyText(value) ? 'Copied' : 'Select and copy';
    if (btn.textContent !== 'Copied') {
      const range = document.createRange();
      range.selectNodeContents(code);
      const sel = getSelection();
      sel?.removeAllRanges();
      sel?.addRange(range);
    }
  };
  wrap.append(code, btn);
  return wrap;
}

/** An id for local use that does not need crypto.randomUUID (secure contexts only). */
export function localId(): string {
  const b = new Uint8Array(8);
  crypto.getRandomValues(b);
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}
