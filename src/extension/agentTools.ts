/**
 * The vineyard.agentTools settings: which of Vineyard's agent tools (the daemon's `vineyardd mcp`
 * server) the sessions every machine starts get. Off by default.
 *
 * As with the web app, the choice goes to each online machine that reports a different one, stamped
 * with when it was made; a daemon keeps whichever choice is later, so two windows with different
 * settings cannot flip a machine back and forth. Nothing is sent while every machine already agrees.
 */

import * as vscode from 'vscode';
import type { FleetService } from './fleet.ts';
import { normalInside, shouldSendTools, wantedTools } from '../core/agentTools.ts';

const CHANGED_KEY = 'vineyard.agentTools.changedAt';

export class AgentToolsSync implements vscode.Disposable {
  private readonly subs: vscode.Disposable[] = [];
  private inflight = new Set<string>();
  /** machine@daemonVersion that does not know the op (too old): not asked again until it is upgraded. */
  private unsupported = new Set<string>();
  /** A machine whose daemon failed the op is asked again only after this (epoch ms). */
  private retryAt = new Map<string, number>();
  private timer: NodeJS.Timeout | undefined;

  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly fleet: FleetService,
    private readonly log: vscode.OutputChannel,
  ) {
    this.subs.push(
      fleet.onDidChange(() => this.schedule()),
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (e.affectsConfiguration('vineyard.agentTools')) void this.settingChanged();
      }),
    );
  }

  private desired(): string[] {
    const c = vscode.workspace.getConfiguration('vineyard.agentTools');
    return wantedTools((s) => c.get<boolean>(s, false));
  }

  private async settingChanged(): Promise<void> {
    await this.context.globalState.update(CHANGED_KEY, Date.now());
    this.retryAt.clear();
    this.schedule();
  }

  private schedule(): void {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.sync(), 1000);
  }

  /** Tell each online machine whose agent tools differ from this window's (later) choice. */
  private sync(): void {
    const at = this.context.globalState.get<number>(CHANGED_KEY, 0);
    if (!at) return; // never touched here: nothing to say
    const tools = this.desired();
    const inside = normalInside(vscode.workspace.getConfiguration('vineyard.agentTools').get<string>('workspacesInside', '~'));
    for (const m of this.fleet.machines()) {
      if (!m.online) continue;
      const st = m.entry.snapshot.agentTools;
      const key = `${m.id}@${m.entry.snapshot.daemonVersion ?? ''}`;
      if (this.inflight.has(m.id) || this.unsupported.has(key) || (this.retryAt.get(m.id) ?? 0) > Date.now()) continue;
      if (!shouldSendTools(st, tools, at, inside)) continue;
      this.inflight.add(m.id);
      this.fleet.client
        .request('agenttools', m.id, { tools, at, workspacesInside: inside }, 15_000)
        .then(() => this.log.appendLine(`agent tools on ${m.name}: ${tools.length ? tools.join(', ') : 'none'}`))
        .catch((err: Error) => {
          if (st) this.retryAt.set(m.id, Date.now() + 60_000);
          else this.unsupported.add(key);
          this.log.appendLine(`agent tools on ${m.name}: ${err.message}${st ? '' : ' (its daemon is older than agent tools; it is asked again once upgraded)'}`);
        })
        .finally(() => this.inflight.delete(m.id));
    }
  }

  dispose(): void {
    clearTimeout(this.timer);
    for (const s of this.subs) s.dispose();
  }
}
