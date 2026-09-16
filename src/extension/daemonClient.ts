/**
 * Viewer connection to the local vineyardd over mutual TLS with the shared fleet certificate.
 * Speaks the newline-delimited JSON protocol in daemon/internal/protocol.
 */

import * as vscode from 'vscode';
import * as tls from 'node:tls';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as crypto from 'node:crypto';
import type { FleetEntry, PeerStatus } from '../core/model.ts';

export const PROTOCOL_VERSION = 1;
export const DEFAULT_PORT = 7734;
export const FLEET_SERVER_NAME = 'vineyard';

export interface PeerAddr {
  machineId: string;
  addr: string;
}

export interface LocalDaemonConfig {
  dir: string;
  machineId: string;
  name: string;
  listen: string;
  advertise?: string;
  peers: PeerAddr[];
  port: number;
  certPem: string;
  keyPem: string;
}

export function vineyardDir(): string {
  return process.env.VINEYARD_DIR || path.join(os.homedir(), '.vineyard');
}

export function readLocalConfig(): LocalDaemonConfig | undefined {
  const dir = vineyardDir();
  try {
    const raw = JSON.parse(fs.readFileSync(path.join(dir, 'config.json'), 'utf8')) as Partial<LocalDaemonConfig>;
    const certPem = fs.readFileSync(path.join(dir, 'fleet.crt'), 'utf8');
    const keyPem = fs.readFileSync(path.join(dir, 'fleet.key'), 'utf8');
    const listen = raw.listen || `:${DEFAULT_PORT}`;
    const port = Number(listen.split(':').pop()) || DEFAULT_PORT;
    return {
      dir,
      machineId: raw.machineId || os.hostname().toLowerCase(),
      name: raw.name || os.hostname().split('.')[0] || 'local',
      listen,
      advertise: raw.advertise,
      peers: Array.isArray(raw.peers) ? raw.peers : [],
      port,
      certPem,
      keyPem,
    };
  } catch {
    return undefined;
  }
}

export type DaemonConnState = 'no-config' | 'connecting' | 'connected' | 'disconnected';

interface FleetMsg {
  t: 'fleet';
  self: string;
  entries: FleetEntry[];
  peers: PeerStatus[];
}

export class DaemonClient implements vscode.Disposable {
  private socket: tls.TLSSocket | undefined;
  private buffer = '';
  private reconnectTimer: NodeJS.Timeout | undefined;
  private backoffMs = 1000;
  private stopped = false;
  private pending = new Map<string, { resolve: (v: unknown) => void; reject: (e: Error) => void; timer: NodeJS.Timeout }>();

  state: DaemonConnState = 'no-config';
  config: LocalDaemonConfig | undefined;
  lastError: string | undefined;
  self: string | undefined;

  private readonly _onFleet = new vscode.EventEmitter<FleetMsg>();
  readonly onFleet = this._onFleet.event;
  private readonly _onUpdate = new vscode.EventEmitter<FleetEntry>();
  readonly onUpdate = this._onUpdate.event;
  private readonly _onPeerStatus = new vscode.EventEmitter<PeerStatus[]>();
  readonly onPeerStatus = this._onPeerStatus.event;
  private readonly _onStateChange = new vscode.EventEmitter<DaemonConnState>();
  readonly onStateChange = this._onStateChange.event;

  constructor(private readonly log: vscode.OutputChannel) {}

  start(): void {
    this.stopped = false;
    this.connect();
  }

  /** Re-read config (e.g. after setup) and connect immediately. */
  reconnectNow(): void {
    this.backoffMs = 1000;
    clearTimeout(this.reconnectTimer);
    this.socket?.destroy();
    this.socket = undefined;
    this.connect();
  }

  private setState(s: DaemonConnState): void {
    if (this.state === s) return;
    this.state = s;
    void vscode.commands.executeCommand('setContext', 'vineyard.daemonState', s);
    this._onStateChange.fire(s);
  }

  private connect(): void {
    if (this.stopped) return;
    this.config = readLocalConfig();
    if (!this.config) {
      this.setState('no-config');
      this.scheduleReconnect(5000);
      return;
    }
    this.setState('connecting');
    const cfg = this.config;
    const socket = tls.connect({
      host: '127.0.0.1',
      port: cfg.port,
      cert: cfg.certPem,
      key: cfg.keyPem,
      ca: [cfg.certPem],
      servername: FLEET_SERVER_NAME,
      minVersion: 'TLSv1.3',
    });
    this.socket = socket;
    socket.setEncoding('utf8');
    socket.setKeepAlive(true, 30_000);
    socket.on('secureConnect', () => {
      this.backoffMs = 1000;
      this.lastError = undefined;
      this.send({
        t: 'hello',
        role: 'viewer',
        machineId: `${cfg.machineId}#vscode-${process.pid}`,
        name: `VS Code on ${cfg.name}`,
        version: 'ext',
        protocol: PROTOCOL_VERSION,
      });
      this.setState('connected');
      this.log.appendLine(`connected to local daemon on :${cfg.port}`);
    });
    socket.on('data', (chunk: string) => this.onData(chunk));
    socket.on('error', (err) => {
      this.lastError = err.message;
      this.log.appendLine(`daemon connection error: ${err.message}`);
    });
    socket.on('close', () => {
      if (this.socket === socket) this.socket = undefined;
      for (const p of this.pending.values()) {
        clearTimeout(p.timer);
        p.reject(new Error('daemon disconnected'));
      }
      this.pending.clear();
      this.setState('disconnected');
      this.scheduleReconnect(this.backoffMs);
      this.backoffMs = Math.min(this.backoffMs * 2, 30_000);
    });
  }

  private scheduleReconnect(ms: number): void {
    clearTimeout(this.reconnectTimer);
    if (this.stopped) return;
    this.reconnectTimer = setTimeout(() => this.connect(), ms);
  }

  private onData(chunk: string): void {
    this.buffer += chunk;
    let idx: number;
    while ((idx = this.buffer.indexOf('\n')) >= 0) {
      const line = this.buffer.slice(0, idx);
      this.buffer = this.buffer.slice(idx + 1);
      if (!line.trim()) continue;
      let msg: Record<string, unknown>;
      try {
        msg = JSON.parse(line);
      } catch {
        continue;
      }
      this.dispatch(msg);
    }
  }

  private dispatch(msg: Record<string, unknown>): void {
    switch (msg.t) {
      case 'fleet': {
        const f = msg as unknown as FleetMsg;
        this.self = f.self;
        this._onFleet.fire(f);
        break;
      }
      case 'update':
        this._onUpdate.fire((msg as { entry: FleetEntry }).entry);
        break;
      case 'peerstatus':
        this._onPeerStatus.fire((msg as { peers: PeerStatus[] }).peers);
        break;
      case 'ping':
        this.send({ t: 'pong' });
        break;
      case 'res': {
        const r = msg as { id: string; ok: boolean; data?: unknown; error?: string };
        const p = this.pending.get(r.id);
        if (!p) break;
        this.pending.delete(r.id);
        clearTimeout(p.timer);
        if (r.ok) p.resolve(r.data);
        else p.reject(new Error(r.error || 'request failed'));
        break;
      }
      default:
        break;
    }
  }

  private send(v: unknown): void {
    if (!this.socket || this.socket.destroyed) return;
    this.socket.write(JSON.stringify(v) + '\n');
  }

  /** One-shot request to any machine; the local daemon relays to peers it is connected to. */
  request<T = unknown>(op: string, target: string | undefined, args?: unknown, timeoutMs = 30_000): Promise<T> {
    if (this.state !== 'connected' || !this.socket) {
      return Promise.reject(new Error('not connected to the local daemon'));
    }
    const id = crypto.randomBytes(8).toString('hex');
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error('request timed out'));
      }, timeoutMs);
      this.pending.set(id, { resolve: resolve as (v: unknown) => void, reject, timer });
      this.send({ t: 'req', id, target: target ?? '', op, args });
    });
  }

  dispose(): void {
    this.stopped = true;
    clearTimeout(this.reconnectTimer);
    this.socket?.destroy();
    this._onFleet.dispose();
    this._onUpdate.dispose();
    this._onPeerStatus.dispose();
    this._onStateChange.dispose();
  }
}
