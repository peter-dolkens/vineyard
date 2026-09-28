/**
 * The screen a browser sees until it is paired: a single-use code from VS Code (Vineyard: Pair a Phone
 * with the Web App) or from a phone that is already paired, typed in or carried by the link's
 * #pair=… fragment. The server answers with this device's credential in an HttpOnly cookie, out of
 * reach of anything the transcripts could inject into the page.
 */

import type { Api } from './api.ts';
import { h, icon } from './ui.ts';

function guessName(): string {
  const ua = navigator.userAgent;
  if (/iPhone/.test(ua)) return 'iPhone';
  if (/iPad/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1)) return 'iPad';
  if (/Android/.test(ua)) return /Mobile/.test(ua) ? 'Android phone' : 'Android tablet';
  if (/Macintosh/.test(ua)) return 'Mac browser';
  if (/Windows/.test(ua)) return 'Windows browser';
  return 'Browser';
}

/** True when nobody else on the network can read this page's traffic (loopback, or HTTPS). */
export function privateConnection(): boolean {
  return location.protocol === 'https:' || /^(localhost|127\.|\[::1\])/.test(location.hostname);
}

export function showPairing(root: HTMLElement, api: Api, serverName: string, code: string | undefined, onPaired: () => void): void {
  const wrap = h('div', 'pair');
  const card = h('div', 'pair-card');
  const logo = h('img', 'pair-logo');
  logo.src = 'build/icon.png';
  logo.alt = '';
  card.append(logo, h('div', 'pair-title', 'Pair this device'), h('div', 'pair-text', `Vineyard on ${serverName} only answers devices you have paired. In VS Code run “Vineyard: Pair a Phone with the Web App”, or in the app on a paired phone open Settings › Pair another device, then enter the code here.`));

  const codeIn = h('input', 'pair-code');
  codeIn.placeholder = 'ABCD-EFGH';
  codeIn.maxLength = 9;
  codeIn.value = code ? `${code.slice(0, 4)}-${code.slice(4)}` : '';
  codeIn.setAttribute('autocapitalize', 'characters');
  codeIn.setAttribute('autocomplete', 'one-time-code');
  codeIn.setAttribute('autocorrect', 'off');
  codeIn.spellcheck = false;
  codeIn.oninput = () => {
    const raw = codeIn.value.toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 8);
    codeIn.value = raw.length > 4 ? `${raw.slice(0, 4)}-${raw.slice(4)}` : raw;
  };

  const nameLabel = h('label', 'pair-label', 'Name this device');
  const nameIn = h('input', 'pair-name');
  nameIn.value = guessName();
  nameIn.maxLength = 60;

  const err = h('div', 'pair-error');
  err.hidden = true;
  const btn = h('button', 'pill-btn pair-btn', 'Pair');
  const go = async () => {
    err.hidden = true;
    btn.disabled = true;
    btn.textContent = 'Pairing…';
    try {
      await api.pair(codeIn.value, nameIn.value);
      onPaired();
    } catch (e) {
      err.textContent = (e as Error).message;
      err.hidden = false;
      btn.disabled = false;
      btn.textContent = 'Pair';
    }
  };
  btn.onclick = () => void go();
  for (const input of [codeIn, nameIn]) input.onkeydown = (e) => e.key === 'Enter' && void go();

  card.append(codeIn, nameLabel, nameIn, err, btn);
  if (!privateConnection()) {
    const warn = h('div', 'pair-warn');
    warn.append(icon('warning'), h('span', undefined, 'This connection is not encrypted. Anyone on this network can read what the app shows, and could copy this device’s pairing. Securing the route (Tailscale, a VPN, a TLS proxy) is up to whoever runs it.'));
    card.append(warn);
  }
  wrap.append(card);
  root.replaceChildren(wrap);
  if (!code) setTimeout(() => codeIn.focus(), 100);
}
