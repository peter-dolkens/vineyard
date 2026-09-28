/**
 * The vineyard.webApp setting: whether every machine's daemon (or the ones listed) serves the mobile
 * web app. Off by default, and turning it on in a VS Code install first asks the user to confirm that
 * securing the route is up to them; until they do, this window asks no daemon to start it.
 *
 * The choice goes to each online machine that reports a different one, stamped with when it was
 * made; a daemon keeps whichever choice is later, so two windows with different settings cannot turn
 * a machine's app on and off in turns. Nothing is sent while every machine already agrees.
 */

import * as vscode from 'vscode';
import type { FleetService, MachineView } from './fleet.ts';
import { relativeTime } from '../core/format.ts';
import { shouldSend, wantedListen, type WebAppWish } from '../core/webApp.ts';

const ACK_KEY = 'vineyard.webApp.acknowledged';
/** Bump when the warning says something new, so it is shown again. */
const ACK_VERSION = 1;
const CHANGED_KEY = 'vineyard.webApp.changedAt';

type Desired = WebAppWish;

interface PairCode {
  code: string;
  expiresInSeconds: number;
  links: string[];
}

interface WebDevice {
  id: string;
  name: string;
  pairedAt: number;
  lastSeen?: number;
}

export class WebAppSync implements vscode.Disposable {
  private readonly subs: vscode.Disposable[] = [];
  private inflight = new Set<string>();
  /** machine@daemonVersion that does not know the op (too old): not asked again until it is upgraded. */
  private unsupported = new Set<string>();
  /** A machine whose daemon failed the op is asked again only after this (epoch ms). */
  private retryAt = new Map<string, number>();
  private timer: NodeJS.Timeout | undefined;
  private asking = false;

  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly fleet: FleetService,
    private readonly log: vscode.OutputChannel,
  ) {
    this.subs.push(
      fleet.onDidChange(() => this.schedule()),
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (e.affectsConfiguration('vineyard.webApp')) void this.settingChanged();
      }),
    );
    // Turned on elsewhere (Settings Sync, settings.json) but never confirmed here: ask once now.
    if (this.desired().on && !this.acknowledged()) void this.confirmOn();
  }

  private desired(): Desired {
    const c = vscode.workspace.getConfiguration('vineyard.webApp');
    return { on: c.get<boolean>('enabled', false), port: c.get<number>('port', 7735), machines: c.get<string[]>('machines', []) };
  }

  private acknowledged(): boolean {
    return this.context.globalState.get<number>(ACK_KEY, 0) >= ACK_VERSION;
  }

  private async settingChanged(): Promise<void> {
    if (this.desired().on && !this.acknowledged()) {
      if (!(await this.confirmOn())) return;
    }
    await this.context.globalState.update(CHANGED_KEY, Date.now());
    this.schedule();
  }

  /** The security gate: the user confirms they are responsible for securing the route, or it goes back off. */
  private async confirmOn(): Promise<boolean> {
    if (this.asking) return false;
    this.asking = true;
    try {
      const { port, machines } = this.desired();
      const where = machines.length ? `on ${machines.join(', ')}` : 'on every machine in your fleet';
      const pick = await vscode.window.showWarningMessage(
        `Serve the Vineyard web app ${where}?`,
        {
          modal: true,
          detail: [
            `Each of those daemons will answer on port ${port} of its whole network.`,
            'A phone has to be paired with a single-use code first, but once paired it can read every transcript and drive every agent in the fleet.',
            'The app is plain HTTP. Anyone on the same network can read the traffic and copy a paired phone’s credential.',
            'Securing this route is up to you: use it only on networks you trust, or reach it over Tailscale, a VPN or a TLS reverse proxy, and firewall the port elsewhere.',
          ].join('\n\n'),
        },
        'I understand, turn it on',
      );
      if (pick) {
        await this.context.globalState.update(ACK_KEY, ACK_VERSION);
        return true;
      }
      await vscode.workspace.getConfiguration('vineyard.webApp').update('enabled', false, vscode.ConfigurationTarget.Global);
      return false;
    } finally {
      this.asking = false;
    }
  }

  private schedule(): void {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.sync(), 1000);
  }

  /** Tell each online machine whose web app differs from this window's (later) choice. */
  private sync(): void {
    const at = this.context.globalState.get<number>(CHANGED_KEY, 0);
    if (!at) return; // never touched here: nothing to say
    const d = { ...this.desired(), on: this.desired().on && this.acknowledged() };
    for (const m of this.fleet.machines()) {
      if (!m.online) continue;
      const st = m.entry.snapshot.webApp;
      const listen = wantedListen(d, m);
      const key = `${m.id}@${m.entry.snapshot.daemonVersion ?? ''}`;
      if (this.inflight.has(m.id) || this.unsupported.has(key) || (this.retryAt.get(m.id) ?? 0) > Date.now()) continue;
      if (!shouldSend(st, listen, at)) continue;
      this.inflight.add(m.id);
      this.fleet.client
        .request('webapp', m.id, { listen, at }, 15_000)
        .then(() => this.log.appendLine(`web app ${listen ? `on at ${listen}` : 'off'} on ${m.name}`))
        .catch((err: Error) => {
          if (st) this.retryAt.set(m.id, Date.now() + 60_000);
          else this.unsupported.add(key);
          this.log.appendLine(`web app on ${m.name}: ${err.message}${st ? '' : ' (its daemon is older than the web app; it is asked again once upgraded)'}`);
        })
        .finally(() => this.inflight.delete(m.id));
    }
  }

  /** Vineyard: Pair a Phone with the Web App: a single-use code from that machine's app. */
  async pair(m: MachineView): Promise<void> {
    if (!this.desired().on || !this.acknowledged()) {
      const open = await vscode.window.showInformationMessage('The web app is off. Turn on vineyard.webApp.enabled first.', 'Open Setting');
      if (open) await vscode.commands.executeCommand('workbench.action.openSettings', 'vineyard.webApp.enabled');
      return;
    }
    const st = m.entry.snapshot.webApp;
    if (!st?.urls?.length) throw new Error(st?.error ? `The web app on ${m.name} could not start: ${st.error}` : `The web app is not running on ${m.name} yet.`);
    const p = await this.fleet.client.request<PairCode>('webpair', m.id, undefined, 10_000);
    const code = `${p.code.slice(0, 4)}-${p.code.slice(4)}`;
    const link = p.links[0] ?? '';
    const pick = await vscode.window.showInformationMessage(
      `Pairing code for ${m.name}: ${code}`,
      {
        modal: true,
        detail: `On the phone, open ${link.split('#')[0]} and enter the code, or open this link, which carries it:\n${link}\n\nOther addresses: ${p.links.slice(1).map((l) => l.split('#')[0]).join(', ') || 'none'}.\n\nSingle use, valid ${Math.round(p.expiresInSeconds / 60)} minutes. Plain HTTP: securing the route is up to you.`,
      },
      'Copy Link',
      'Copy Code',
    );
    if (pick === 'Copy Link') await vscode.env.clipboard.writeText(link);
    if (pick === 'Copy Code') await vscode.env.clipboard.writeText(p.code);
  }

  /** Vineyard: Web App Devices: the browsers paired with a machine's app, to sign one or all out. */
  async devices(m: MachineView): Promise<void> {
    const { devices } = await this.fleet.client.request<{ devices: WebDevice[] }>('webdevices', m.id, undefined, 10_000);
    if (!devices.length) {
      void vscode.window.showInformationMessage(`No devices are paired with the web app on ${m.name}.`);
      return;
    }
    type Item = vscode.QuickPickItem & { id?: string; all?: boolean };
    const items: Item[] = devices.map((d) => ({ label: `$(device-mobile) ${d.name}`, description: `paired ${relativeTime(d.pairedAt)}`, detail: d.lastSeen ? `last seen ${relativeTime(d.lastSeen)}` : undefined, id: d.id }));
    items.push({ label: '$(sign-out) Sign out every device', all: true });
    const pick = await vscode.window.showQuickPick(items, { title: `Web app devices on ${m.name}`, placeHolder: 'Pick a device to sign it out' });
    if (!pick) return;
    const what = pick.all ? `every device paired with ${m.name}` : pick.label.replace(/^\$\([^)]*\) /, '');
    const ok = await vscode.window.showWarningMessage(`Sign out ${what}?`, { modal: true, detail: 'It will need a new pairing code to use the web app again.' }, 'Sign Out');
    if (!ok) return;
    await this.fleet.client.request('webrevoke', m.id, pick.all ? { all: true } : { id: pick.id }, 10_000);
  }

  dispose(): void {
    clearTimeout(this.timer);
    for (const s of this.subs) s.dispose();
  }
}
