/**
 * The web app's line to `vineyardd web`: an event stream carrying the fleet as the local daemon tells
 * a viewer, and one POST per operation. The stream is closed whenever the page is hidden, so a phone
 * in a pocket stops the server's daemon link (after its linger) and with it the fleet's traffic.
 */

import type { FleetEntry, PeerStatus } from '../core/model.ts';

/** How the server's link to the daemon stands; 'offline' means this page cannot reach the server. */
export type LinkState = 'offline' | 'idle' | 'connecting' | 'connected' | 'disconnected';

export type ServerMsg =
  | { t: 'state'; state: LinkState; error?: string }
  | { t: 'fleet'; self: string; entries: FleetEntry[]; peers: PeerStatus[] }
  | { t: 'update'; entry: FleetEntry }
  | { t: 'peerstatus'; peers: PeerStatus[] };

export interface ServerInfo {
  name: string;
  /** False until this browser has paired; the rest is only sent to a paired one. */
  paired: boolean;
  device?: { id: string; name: string };
  version?: string;
  self?: string;
  restart?: boolean;
  log?: boolean;
}

export interface PairedDevice {
  id: string;
  name: string;
  pairedAt: number;
  lastSeen?: number;
  current: boolean;
}

export interface PairCode {
  code: string;
  expiresInSeconds: number;
  links: string[];
}

const HEADERS = { 'Content-Type': 'application/json', 'X-Vineyard': '1' };

export class Api {
  private es: EventSource | undefined;
  private listeners: ((m: ServerMsg) => void)[] = [];
  /** Called when the server says this browser is not (or no longer) paired. */
  onUnpaired: () => void = () => undefined;

  onMessage(fn: (m: ServerMsg) => void): void {
    this.listeners.push(fn);
  }

  private emit(m: ServerMsg): void {
    for (const fn of this.listeners) fn(m);
  }

  start(): void {
    this.open();
    document.addEventListener('visibilitychange', () => (document.hidden ? this.close() : this.open()));
    window.addEventListener('pagehide', () => this.close());
    window.addEventListener('pageshow', () => !document.hidden && this.open());
  }

  private open(): void {
    if (this.es && this.es.readyState !== EventSource.CLOSED) return;
    const es = new EventSource('api/events');
    this.es = es;
    es.onmessage = (ev) => {
      try {
        this.emit(JSON.parse(ev.data) as ServerMsg);
      } catch {
        /* a partial or foreign line; the next full fleet corrects anything missed */
      }
    };
    es.onerror = () => {
      // EventSource retries a dropped connection on its own (the server asks for 2 s), but gives up
      // on an HTTP error: then find out whether this device was signed out.
      if (this.es !== es) return;
      this.emit({ t: 'state', state: 'offline' });
      if (es.readyState === EventSource.CLOSED) {
        setTimeout(() => {
          if (this.es !== es) return;
          void this.info()
            .then((i) => (i.paired ? this.reconnect() : this.onUnpaired()))
            .catch(() => setTimeout(() => this.es === es && this.reconnect(), 3000));
        }, 1500);
      }
    };
  }

  private close(): void {
    this.es?.close();
    this.es = undefined;
  }

  /** Reopen the stream now, e.g. from a Retry button. */
  reconnect(): void {
    this.close();
    this.open();
  }

  /** One operation on a machine ('' = the local daemon), relayed through the local daemon. */
  async request<T = unknown>(op: string, target: string | undefined, args?: unknown, timeoutMs = 30_000): Promise<T> {
    let res: Response;
    try {
      res = await fetch('api/req', {
        method: 'POST',
        headers: HEADERS,
        body: JSON.stringify({ op, target: target ?? '', args: args ?? null, timeoutMs }),
      });
    } catch {
      throw new Error('Cannot reach the Vineyard web server');
    }
    return (await this.read<{ data?: T }>(res)).data as T;
  }

  /** The JSON body of an API answer; a refusal becomes an error, a lost pairing a sign-out. */
  private async read<T>(res: Response): Promise<T & { ok?: boolean }> {
    let body: T & { ok?: boolean; error?: string; unpaired?: boolean };
    try {
      body = await res.json();
    } catch {
      throw new Error(`The web server answered ${res.status}`);
    }
    if (res.status === 401 || body.unpaired) {
      this.onUnpaired();
      throw new Error(body.error || 'This device is not paired');
    }
    if (body.ok === false) throw new Error(body.error || `request failed (${res.status})`);
    return body;
  }

  private async post<T>(path: string, body: unknown): Promise<T> {
    let res: Response;
    try {
      res = await fetch(path, { method: 'POST', headers: HEADERS, body: JSON.stringify(body ?? {}) });
    } catch {
      throw new Error('Cannot reach the Vineyard web server');
    }
    return this.read<T>(res);
  }

  async info(): Promise<ServerInfo> {
    const res = await fetch('api/info', { cache: 'no-store' });
    return (await res.json()) as ServerInfo;
  }

  /** Redeem a pairing code; the server answers with the device credential in an HttpOnly cookie. */
  async pair(code: string, name: string): Promise<void> {
    const res = await fetch('api/pair', { method: 'POST', headers: HEADERS, body: JSON.stringify({ code, name }) });
    const body = (await res.json().catch(() => ({}))) as { ok?: boolean; error?: string };
    if (!body.ok) throw new Error(body.error || `pairing failed (${res.status})`);
  }

  pairNew(): Promise<PairCode> {
    return this.post<PairCode>('api/pair/new', {});
  }

  async devices(): Promise<PairedDevice[]> {
    const res = await fetch('api/devices', { cache: 'no-store' });
    return (await this.read<{ devices: PairedDevice[] }>(res)).devices;
  }

  async revoke(target: { id: string } | { all: true }): Promise<void> {
    await this.post('api/devices/revoke', target);
  }

  async log(lines = 300): Promise<string> {
    const res = await fetch(`api/log?lines=${lines}`);
    if (res.status === 401) this.onUnpaired();
    if (!res.ok) throw new Error(await res.text());
    return res.text();
  }

  async restartDaemon(): Promise<void> {
    await this.post('api/restart', {});
  }
}
