import { test } from 'node:test';
import assert from 'node:assert/strict';
import { TranscriptFeed, type FeedMessage, type TranscriptArgs, type TranscriptData } from '../src/core/transcriptFeed.ts';

/** A transcript file as the daemon reads it (mesh/node.go readTranscript), one entry per line. */
class FakeFile {
  lines: string[] = [];
  /** Bumped when the file is replaced, so its lines differ from anything read before. */
  version = 0;
  paging = true;

  append(...ids: string[]) {
    this.lines.push(...ids);
  }
  replace(ids: string[]) {
    this.version++;
    this.lines = ids;
  }
  private at(i: number) {
    return this.lines.slice(0, i).reduce((n, l) => n + l.length + 1, 0);
  }
  private index(offset: number): number | undefined {
    for (let i = 0; i <= this.lines.length; i++) if (this.at(i) === offset) return i;
    return undefined;
  }
  read(a: TranscriptArgs): TranscriptData {
    const size = this.at(this.lines.length);
    const data = (from: number, to: number, extra: Partial<TranscriptData> = {}): TranscriptData => ({
      path: 'p',
      entries: this.lines.slice(from, to).map((uuid) => ({ uuid })),
      offset: this.at(to),
      size,
      ...(this.paging ? { start: this.at(from) } : {}),
      ...extra,
    });
    if (a.before) {
      const end = this.index(a.before);
      if (end === undefined) return { path: 'p', entries: [], offset: 0, size, start: 0, truncated: true };
      return data(Math.max(0, end - a.lines), end);
    }
    if (a.offset) {
      const from = this.index(a.offset);
      if (from === undefined || a.offset > size) return data(Math.max(0, this.lines.length - a.lines), this.lines.length, { truncated: true });
      return data(from, this.lines.length);
    }
    const n = this.lines.length;
    const d = data(Math.max(0, n - a.lines), n);
    return { ...d, size: d.offset };
  }
}

/** A chat view that applies what the feed posts, as webview/main.ts does (without the uuid filter). */
function harness(file: FakeFile, lines = 3) {
  const view: string[] = [];
  let more = false;
  const pending: { resolve: () => void; args: TranscriptArgs }[] = [];
  const timers: (() => void)[] = [];
  const errors: string[] = [];
  const feed = new TranscriptFeed({
    // The read happens when the daemon gets to it, which a test decides: resolve() runs it then.
    read: (args) => new Promise((ok) => pending.push({ args, resolve: () => ok(structuredClone(file.read(args))) })),
    post: (m: FeedMessage) => {
      const ids = m.entries.map((e) => String(e.uuid));
      if (m.type === 'older') {
        view.unshift(...ids);
        more = m.more;
      } else if (m.reset) {
        view.splice(0, view.length, ...ids);
        more = !!m.more;
      } else view.push(...ids);
    },
    lines: () => lines,
    error: (e) => errors.push(e),
    later: (fn) => timers.push(fn),
  });
  const settle = () => new Promise((r) => setImmediate(r));
  return {
    feed,
    view,
    errors,
    pending,
    get more() {
      return more;
    },
    /** Answer the i-th outstanding read, then let the feed act on it. */
    async answer(i = 0) {
      const [p] = pending.splice(i, 1);
      p!.resolve();
      await settle();
    },
    /** Answer everything, fire catch-up timers, until nothing is outstanding. */
    async drain() {
      for (let guard = 0; guard < 1000 && (pending.length || timers.length); guard++) {
        if (pending.length) await this.answer();
        else timers.shift()!();
        await settle();
      }
    },
    async scrollToTop() {
      for (let guard = 0; guard < 1000 && more; guard++) {
        void feed.older();
        await this.drain();
      }
    },
  };
}

const ids = (from: number, to: number) => Array.from({ length: to - from }, (_, i) => `e${from + i}`);

test('the tail, then older pages back to the start, each line once', async () => {
  const file = new FakeFile();
  file.append(...ids(0, 10));
  const h = harness(file);
  void h.feed.fetch(true);
  await h.drain();
  assert.deepEqual(h.view, ids(7, 10));
  assert.equal(h.more, true);
  await h.scrollToTop();
  assert.deepEqual(h.view, ids(0, 10));
  assert.equal(h.more, false);
  void h.feed.older(); // at the start: nothing more is read
  assert.equal(h.pending.length, 0);
});

test('lines written while an older page is on its way go after, not twice', async () => {
  const file = new FakeFile();
  file.append(...ids(0, 8));
  const h = harness(file);
  void h.feed.fetch(true);
  await h.drain();
  void h.feed.older();
  file.append(...ids(8, 11));
  void h.feed.fetch(false);
  await h.answer(1); // the live read lands first
  await h.drain();
  assert.deepEqual(h.view, ids(2, 11));
  await h.scrollToTop();
  assert.deepEqual(h.view, ids(0, 11));
});

test('a reload drops an older page read for the log it replaced', async () => {
  const file = new FakeFile();
  file.append(...ids(0, 12));
  const h = harness(file);
  void h.feed.fetch(true);
  await h.drain();
  void h.feed.older(); // in flight…
  void h.feed.fetch(true); // …when the view reloads
  void h.feed.older(); // and asks again straight away, before the new tail says where to
  await h.drain();
  assert.deepEqual(h.view, ids(9, 12), 'the stale page is not prepended');
  await h.scrollToTop();
  assert.deepEqual(h.view, ids(0, 12));
});

test('a reload while a live read is in flight starts over instead of appending', async () => {
  const file = new FakeFile();
  file.append(...ids(0, 5));
  const h = harness(file);
  void h.feed.fetch(true);
  await h.drain();
  file.append('e5');
  void h.feed.fetch(false);
  void h.feed.fetch(true); // queued behind it
  await h.drain();
  assert.deepEqual(h.view, ids(3, 6));
  await h.scrollToTop();
  assert.deepEqual(h.view, ids(0, 6));
});

test('a file replaced under an older read shows the new file afresh', async () => {
  const file = new FakeFile();
  file.append(...ids(0, 9));
  const h = harness(file);
  void h.feed.fetch(true);
  await h.drain();
  file.replace(['n0', 'n1', 'n2', 'n3x', 'n4']);
  void h.feed.older();
  await h.drain();
  assert.deepEqual(h.view, ['n2', 'n3x', 'n4']);
  await h.scrollToTop();
  assert.deepEqual(h.view, ['n0', 'n1', 'n2', 'n3x', 'n4']);
});

test('a daemon that cannot page is never asked for older lines', async () => {
  const file = new FakeFile();
  file.paging = false;
  file.append(...ids(0, 10));
  const h = harness(file);
  void h.feed.fetch(true);
  await h.drain();
  assert.equal(h.more, false);
  void h.feed.older();
  assert.equal(h.pending.length, 0);
});

test('fuzz: whatever order reads land in, the view is the file in order, each line once', async () => {
  let seed = 7;
  const rnd = (n: number) => {
    seed = (seed * 1103515245 + 12345) & 0x7fffffff;
    return seed % n;
  };
  for (let round = 0; round < 200; round++) {
    const file = new FakeFile();
    let next = 0;
    const add = (k: number) => file.append(...ids(next, (next += k)));
    add(rnd(15));
    const h = harness(file, 1 + rnd(4));
    void h.feed.fetch(true);
    for (let step = 0; step < 40; step++) {
      switch (rnd(7)) {
        case 0:
          add(1 + rnd(3));
          void h.feed.fetch(false);
          break;
        case 1:
          void h.feed.older();
          break;
        case 2:
          if (rnd(4) === 0) void h.feed.fetch(true);
          break;
        default:
          if (h.pending.length) await h.answer(rnd(h.pending.length));
      }
      // At every moment: in file order, nothing twice, no gaps.
      const at = file.lines.indexOf(h.view[0]!);
      if (h.view.length) assert.deepEqual(h.view, file.lines.slice(at, at + h.view.length), `round ${round} step ${step}`);
    }
    void h.feed.fetch(false);
    await h.drain();
    await h.scrollToTop();
    assert.deepEqual(h.view, file.lines, `round ${round}: the whole file once scrolled to the top`);
    assert.deepEqual(h.errors, []);
  }
});
