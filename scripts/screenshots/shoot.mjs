import { chromium } from 'playwright';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const url = (f, hash = '') => `file://${path.join(here, f)}${hash}`;

const browser = await chromium.launch();
const ctx = await browser.newContext({ deviceScaleFactor: 2, colorScheme: 'dark', reducedMotion: 'reduce' });

async function shot(file, name, { width, height, hash = '', before }) {
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error(`[${name}] page error:`, e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.error(`[${name}] console:`, m.text()); });
  await page.setViewportSize({ width, height });
  await page.goto(url(file, hash));
  await page.waitForTimeout(400);
  if (before) await before(page);
  await page.waitForTimeout(300);
  await page.screenshot({ path: path.join(here, '..', '..', 'docs', 'images', `${name}.png`) });
  console.log('wrote', name);
  await page.close();
}

await shot('tree.html', 'fleet-tree', { width: 560, height: 560 });

await shot('chat.html', 'chat-permission', {
  width: 720, height: 800, hash: '#perm',
  before: async (page) => {
    await page.evaluate(() => { const l = document.getElementById('log'); l.scrollTop = l.scrollHeight; });
  },
});

await shot('chat.html', 'chat-agent-map', {
  width: 720, height: 780, hash: '#map',
  before: async (page) => {
    await page.evaluate(() => { const l = document.getElementById('log'); l.scrollTop = l.scrollHeight; });
    await page.click('#agentsPill');
  },
});

await shot('chat.html', 'chat-question', {
  width: 720, height: 640, hash: '#question',
  before: async (page) => {
    await page.evaluate(() => { const l = document.getElementById('log'); l.scrollTop = l.scrollHeight; });
  },
});

// The setup guide (docs/SETUP.md), one window per step.
for (const step of ['welcome', 'setup', 'invite', 'join', 'joined', 'ssh', 'sshdone']) {
  // The welcome view is taller than the tree steps.
  const height = step === 'welcome' || step === 'join' ? 450 : 400;
  await shot('setup.html', `setup-${step}`, { width: 960, height, hash: `#${step}` });
}

await browser.close();
