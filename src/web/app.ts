/**
 * The Vineyard web app: the fleet on a phone, served by `vineyardd web` next to a daemon. The tree
 * of the VS Code view becomes a stack of screens (screens.ts) with iOS-style pushes and pops, an edge
 * swipe to go back when installed on the home screen, and the chat the extension uses (chatHost.ts).
 */

import './app.css';
import { needsAttention } from '../core/model.ts';
import { agentLabel } from '../core/format.ts';
import { Api, type ServerInfo } from './api.ts';
import { Flows } from './flows.ts';
import { R } from './routes.ts';
import { ChatScreen, screenFor, titleForRoute, type Ctx, type Screen } from './screens.ts';
import { onSettingsChange, settings } from './settings.ts';
import { FleetStore } from './store.ts';
import { showPairing } from './pair.ts';
import { h, toast } from './ui.ts';

const root = document.getElementById('root')!;
root.replaceChildren();
const stackEl = h('div', 'stack');
const overlays = h('div');
overlays.id = 'overlays';
root.append(stackEl, overlays); // overlays follow the keyboard with the rest of the page
document.addEventListener('touchstart', () => undefined, { passive: true }); // lets :active show on iOS

const api = new Api();
const store = new FleetStore();
let info: ServerInfo | undefined;
const standalone = matchMedia('(display-mode: standalone)').matches || (navigator as { standalone?: boolean }).standalone === true;

// ---- router -------------------------------------------------------------------------------------

const stack: Screen[] = [];
/** Set when the app itself went back, so the pop animates; Safari's own swipe-back has animated already. */
let expectPop = false;

const currentRoute = () => decodeHash(location.hash);

function decodeHash(hash: string): string {
  const r = hash.replace(/^#/, '');
  return r.startsWith('/') ? r : '/';
}

const ctx: Ctx = {
  api,
  store,
  flows: new Flows(api, store, () => info, (r) => navigate(r), currentRoute),
  info: () => info,
  navigate,
  back,
};

function navigate(route: string): void {
  if (route === currentRoute()) return;
  location.hash = route;
}

function back(): void {
  if (stack.length > 1) {
    expectPop = true;
    history.back();
    return;
  }
  // Opened cold on a deep screen: go to the screen above it, replacing this history entry.
  const top = stack[0]!;
  const parent = top.parent();
  history.replaceState(null, '', '#' + parent);
  const s = build(parent);
  stackEl.insertBefore(s.el, top.el);
  stack.unshift(s);
  s.el.classList.add('under');
  s.render();
  s.mount();
  pop(1, true);
}

function build(route: string): Screen {
  const s = screenFor(route, ctx);
  return s;
}

function backLabelFor(i: number): string | undefined {
  if (i > 0) return stack[i - 1]!.title();
  const r = stack[i]!.route;
  return r === '/' ? undefined : titleForRoute(stack[i]!.parent(), store);
}

function show(route: string): void {
  const top = stack[stack.length - 1];
  if (top?.route === route) return;
  const idx = stack.findIndex((s) => s.route === route);
  if (idx >= 0) {
    pop(stack.length - 1 - idx, expectPop || standalone);
  } else {
    push(build(route), !!top);
  }
  expectPop = false;
}

function push(s: Screen, animate: boolean): void {
  const prev = stack[stack.length - 1];
  stack.push(s);
  s.setBack(backLabelFor(stack.length - 1));
  s.render();
  if (animate) s.el.classList.add('off-right');
  stackEl.append(s.el);
  if (s instanceof ChatScreen) s.frameEl.addEventListener('load', applyInsets);
  s.mount();
  if (!animate) {
    for (const o of stack.slice(0, -1)) o.el.hidden = true;
    return;
  }
  void s.el.offsetWidth;
  s.el.classList.add('anim');
  prev?.el.classList.add('anim');
  s.el.classList.remove('off-right');
  prev?.el.classList.add('under');
  setTimeout(() => {
    s.el.classList.remove('anim');
    if (prev && stack.includes(prev) && stack[stack.length - 1] !== prev) {
      prev.el.classList.remove('anim');
      prev.el.hidden = true;
    }
  }, 360);
  applyInsets();
}

function pop(count: number, animate: boolean): void {
  if (count <= 0) return;
  const leaving = stack.splice(stack.length - count, count);
  const top = stack[stack.length - 1]!;
  top.el.hidden = false;
  top.setBack(backLabelFor(stack.length - 1));
  top.render();
  const last = leaving[leaving.length - 1]!;
  for (const s of leaving.slice(0, -1)) {
    s.destroy();
    s.el.remove();
  }
  if (!animate) {
    top.el.classList.remove('under', 'anim');
    last.destroy();
    last.el.remove();
    return;
  }
  void top.el.offsetWidth;
  top.el.classList.add('anim');
  last.el.classList.add('anim');
  top.el.classList.remove('under');
  last.el.classList.add('off-right');
  setTimeout(() => {
    top.el.classList.remove('anim');
    last.destroy();
    last.el.remove();
  }, 360);
}

window.addEventListener('hashchange', () => show(currentRoute()));

// ---- edge swipe back (home-screen app only; Safari has its own) --------------------------------

if (standalone) {
  let startX = 0;
  let startY = 0;
  let dx = 0;
  let tracking = false;
  let decided = false;
  stackEl.addEventListener(
    'touchstart',
    (e) => {
      const t = e.touches[0]!;
      tracking = stack.length > 0 && t.clientX < 22 && (stack.length > 1 || stack[0]!.route !== '/');
      decided = false;
      startX = t.clientX;
      startY = t.clientY;
      dx = 0;
    },
    { passive: true },
  );
  stackEl.addEventListener(
    'touchmove',
    (e) => {
      if (!tracking) return;
      const t = e.touches[0]!;
      dx = Math.max(0, t.clientX - startX);
      if (!decided) {
        if (Math.abs(t.clientY - startY) > 12 && Math.abs(t.clientY - startY) > dx) {
          tracking = false;
          return;
        }
        if (dx < 8) return;
        decided = true;
      }
      const top = stack[stack.length - 1]!;
      const under = stack[stack.length - 2];
      top.el.classList.add('dragging');
      top.el.style.transform = `translateX(${dx}px)`;
      if (under) {
        under.el.hidden = false;
        under.el.classList.add('dragging');
        under.el.style.transform = `translateX(${-28 + (dx / innerWidth) * 28}%)`;
      }
    },
    { passive: true },
  );
  stackEl.addEventListener('touchend', () => {
    if (!tracking || !decided) return;
    tracking = false;
    const top = stack[stack.length - 1]!;
    const under = stack[stack.length - 2];
    for (const s of [top, under]) {
      if (!s) continue;
      s.el.classList.remove('dragging');
      s.el.style.transform = '';
    }
    if (dx > innerWidth * 0.33) back();
    else if (under) {
      under.el.classList.add('under');
      setTimeout(() => stack[stack.length - 1] === top && (under.el.hidden = true), 360);
    }
  });
}

// ---- viewport: follow the keyboard, hand safe-area insets to the chat frame --------------------

const probe = h('div');
probe.style.cssText = 'position:fixed;visibility:hidden;pointer-events:none;padding:env(safe-area-inset-top) 0 env(safe-area-inset-bottom) 0';
document.body.append(probe);
let keyboardOpen = false;

function applyInsets(): void {
  const cs = getComputedStyle(probe);
  const sat = cs.paddingTop;
  const sab = keyboardOpen ? '0px' : cs.paddingBottom;
  for (const s of stack) {
    if (!(s instanceof ChatScreen)) continue;
    const el = s.frameDoc?.documentElement;
    if (!el) continue;
    el.style.setProperty('--sat', sat);
    el.style.setProperty('--sab', sab);
  }
}

const vv = window.visualViewport;
function fitViewport(): void {
  if (!vv) return;
  keyboardOpen = window.innerHeight - vv.height > 120;
  root.style.height = `${vv.height}px`;
  root.style.transform = vv.offsetTop ? `translateY(${vv.offsetTop}px)` : '';
  if (window.scrollY) window.scrollTo(0, 0);
  applyInsets();
}
vv?.addEventListener('resize', fitViewport);
vv?.addEventListener('scroll', fitViewport);
window.addEventListener('orientationchange', () => setTimeout(fitViewport, 300));

// ---- live updates -------------------------------------------------------------------------------

let renderQueued = false;
function renderTop(): void {
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => {
    renderQueued = false;
    stack[stack.length - 1]?.render();
    const n = store.summary().attention;
    document.title = n ? `(${n}) Vineyard` : 'Vineyard';
  });
}

store.onChange(renderTop);
onSettingsChange(renderTop);
setInterval(renderTop, 15_000); // relative times

store.onTransition(({ machine, previous, current }) => {
  if (!settings().notifyAttention || !current.alive || !needsAttention(current.state)) return;
  if (previous && needsAttention(previous.state)) return;
  if (currentRoute() === R.chat(current.id)) return; // already looking at it
  const what = current.state === 'question' ? 'has a question' : 'needs permission';
  toast(`${machine.name}: ${agentLabel(current)} ${what}`, 'attention', () => ctx.flows.openChat(machine, current));
});

api.onMessage((m) => store.apply(m));
let signingOut = false;
api.onUnpaired = () => {
  // Signed out (revoked here or from VS Code): start again at the pairing screen.
  if (signingOut) return;
  signingOut = true;
  location.reload();
};
fitViewport();
void boot();

/** Pair first if this browser is not paired yet (a #pair=CODE link fills the code in), then start. */
async function boot(): Promise<void> {
  const link = /^#pair=([A-Za-z0-9-]+)/.exec(location.hash);
  const code = link?.[1]?.toUpperCase().replace(/-/g, '');
  if (link) history.replaceState(null, '', location.pathname + '#/');
  try {
    info = await api.info();
  } catch {
    const card = h('div', 'pair');
    const box = h('div', 'pair-card');
    const retry = h('button', 'pill-btn pair-btn', 'Retry');
    retry.onclick = () => location.reload();
    box.append(h('div', 'pair-title', 'Cannot reach Vineyard'), h('div', 'pair-text', 'The web server on this address is not answering. It may have been turned off in VS Code’s vineyard.webApp setting.'), retry);
    card.append(box);
    root.replaceChildren(card);
    return;
  }
  if (!info.paired) {
    showPairing(root, api, info.name, code, () => {
      history.replaceState(null, '', location.pathname + '#/');
      location.reload();
    });
    return;
  }
  api.start();
  show(currentRoute());
  renderTop();
}
