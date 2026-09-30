/**
 * Streams one transcript into a chat view: the tail when it opens, lines written since as the agent
 * works, and older pages when the view scrolls to the top. Shared by the VS Code panel
 * (extension/chatPanel.ts) and the web app (web/chatHost.ts).
 *
 * Every read names a byte range, so pages never overlap: the live stream continues from `offset`,
 * the tail and each older page end where the next older page begins (`start`). A start-over (reload,
 * or a transcript replaced under us) bumps `gen`, and a page read for an earlier generation is
 * dropped rather than prepended to a log it does not belong to.
 */

export interface TranscriptData {
  path: string;
  entries: Record<string, unknown>[];
  offset: number;
  size: number;
  /** Byte offset of the first entry; 0 at the start of the file. Missing from older daemons. */
  start?: number;
  truncated?: boolean;
}

export interface TranscriptArgs {
  lines: number;
  offset?: number;
  before?: number;
}

export type FeedMessage =
  /** `more`: older lines exist before these (sent with a reset). */
  | { type: 'entries'; entries: Record<string, unknown>[]; reset: boolean; more?: boolean }
  /** An older page, to go before everything shown. */
  | { type: 'older'; entries: Record<string, unknown>[]; more: boolean };

export interface FeedDeps {
  read(args: TranscriptArgs): Promise<TranscriptData>;
  post(m: FeedMessage): void;
  lines(): number;
  error(message: string): void;
  /** Wait before catching up with lines written during a read (a timer; tests pass their own). */
  later(fn: () => void): void;
}

export class TranscriptFeed {
  private offset = 0;
  /** Where the next older page ends; undefined until a tail says (or when the daemon cannot page). */
  private start: number | undefined;
  private gen = 0;
  private fetching = false;
  private pendingFetch = false;
  private resetWanted = false;
  private olderBusy = false;
  private olderAgain = false;
  private disposed = false;
  private readonly deps: FeedDeps;

  constructor(deps: FeedDeps) {
    this.deps = deps;
  }

  dispose(): void {
    this.disposed = true;
  }

  /** Read what was written since the last read, or the tail when starting over. */
  async fetch(reset = false): Promise<void> {
    if (this.disposed) return;
    if (reset) this.startOver();
    if (this.fetching) {
      this.pendingFetch = true;
      return;
    }
    this.fetching = true;
    const fresh = this.resetWanted || this.offset === 0;
    this.resetWanted = false;
    try {
      const data = await this.deps.read({ lines: this.deps.lines(), offset: fresh ? 0 : this.offset });
      if (this.disposed) return;
      if (this.resetWanted) {
        // A reload arrived while this was in flight: its read replaces this one.
        this.pendingFetch = true;
        return;
      }
      const startOver = fresh || !!data.truncated;
      this.offset = data.offset;
      if (startOver) {
        if (!fresh) this.gen++; // replaced under us: older pages in flight are for the old file
        this.start = data.start;
        this.deps.post({ type: 'entries', entries: data.entries, reset: true, more: this.hasMore() });
      } else if (data.entries.length) {
        this.deps.post({ type: 'entries', entries: data.entries, reset: false });
      }
      if (data.size > data.offset) this.pendingFetch = true; // more arrived while we read
    } catch (err) {
      const msg = (err as Error).message;
      if (!/no transcript/i.test(msg)) this.deps.error(msg);
    } finally {
      this.fetching = false;
      if (this.pendingFetch && !this.disposed) {
        this.pendingFetch = false;
        this.deps.later(() => void this.fetch(false));
      }
    }
  }

  /** Read the page before what is shown. Ignored until a tail says where that is. */
  async older(): Promise<void> {
    if (this.disposed) return;
    if (this.olderBusy) {
      this.olderAgain = true; // answered once the one in flight settles (it may be for an old log)
      return;
    }
    const before = this.start;
    if (!before || before <= 0) return;
    this.olderBusy = true;
    const gen = this.gen;
    try {
      const data = await this.deps.read({ lines: this.deps.lines(), before });
      if (this.disposed || gen !== this.gen || this.start !== before) return;
      if (data.truncated) {
        void this.fetch(true); // the file was replaced: show it afresh
        return;
      }
      this.start = data.start ?? 0;
      this.deps.post({ type: 'older', entries: data.entries, more: this.hasMore() });
    } catch (err) {
      if (gen !== this.gen) return;
      this.deps.error((err as Error).message);
      this.deps.post({ type: 'older', entries: [], more: this.hasMore() }); // lets the view ask again
    } finally {
      this.olderBusy = false;
      if (this.olderAgain && !this.disposed) {
        this.olderAgain = false;
        void this.older();
      }
    }
  }

  private startOver(): void {
    this.resetWanted = true;
    this.start = undefined;
    this.gen++;
  }

  private hasMore(): boolean {
    return typeof this.start === 'number' && this.start > 0;
  }
}
