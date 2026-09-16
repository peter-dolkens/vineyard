/**
 * Keeping the fleet current.
 *
 * Daemons: the extension bundles a vineyardd build for every platform. Whenever it sees a machine
 * running an older daemon than the bundled one, it streams the matching binary to that machine over
 * the mesh (the `upgrade` request, relayed by the local daemon), and the daemon there verifies and
 * installs it. No SSH, no polling, no traffic unless a viewer is attached and a machine is outdated.
 * Daemons too old to know the `upgrade` request are updated once over SSH (or locally) instead.
 *
 * Extension: at most once a day, one request to the GitHub Releases API; if a newer release exists we
 * offer to download and install the .vsix. Both behaviours have settings.
 */

import * as vscode from 'vscode';
import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { isNewer } from '../core/version.ts';
import type { DaemonClient } from './daemonClient.ts';
import type { FleetService, MachineView } from './fleet.ts';
import type { Setup } from './setup.ts';
import { failed, run, type RemoteArch, type RemoteOS } from './ssh.ts';

const CHUNK = 512 * 1024;
const RELEASES_API = 'https://api.github.com/repos/peter-dolkens/vineyard/releases/latest';
const CHECK_EVERY_MS = 24 * 60 * 60 * 1000;
const LAST_CHECK_KEY = 'vineyard.updates.lastCheck';
const SKIPPED_KEY = 'vineyard.updates.skipped';

const PLATFORM_OS: Record<string, RemoteOS> = { darwin: 'darwin', linux: 'linux', win32: 'windows' };
const PLATFORM_ARCH: Record<string, RemoteArch> = { arm64: 'arm64', x64: 'amd64', ia32: '386' };

interface UpgradeResult {
  received: number;
  installed?: boolean;
  version?: string;
}

export class Updater implements vscode.Disposable {
  /** Version reported by the bundled daemon for this platform; undefined until probed or if unsupported. */
  bundledVersion: string | undefined;
  private readonly subs: vscode.Disposable[] = [];
  private readonly inFlight = new Set<string>();
  private readonly attempted = new Set<string>(); // `${machineId}@${bundledVersion}`: one automatic try per version
  private debounce: NodeJS.Timeout | undefined;
  private warnedManual = false;

  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly client: DaemonClient,
    private readonly fleet: FleetService,
    private readonly setup: Setup,
    private readonly log: vscode.OutputChannel,
  ) {}

  start(): void {
    void this.probeBundledVersion().then(() => this.scheduleScan());
    this.subs.push(this.fleet.onDidChange(() => this.scheduleScan()));
    void this.checkExtensionUpdate(false);
  }

  private cfg<T>(key: string, def: T): T {
    return vscode.workspace.getConfiguration('vineyard').get<T>(key, def);
  }

  // ---- daemons --------------------------------------------------------------------------------

  private async probeBundledVersion(): Promise<void> {
    const osName = PLATFORM_OS[process.platform];
    const arch = PLATFORM_ARCH[process.arch];
    if (!osName || !arch) return;
    try {
      const r = await run(this.setup.binaryFor(osName, arch), ['version'], { timeoutMs: 15_000 });
      const v = r.stdout.trim().split('\n').pop()?.trim();
      if (!failed(r) && v) {
        this.bundledVersion = v;
        this.log.appendLine(`bundled vineyardd is ${v}`);
      }
    } catch (err) {
      this.log.appendLine(`could not determine the bundled daemon version: ${(err as Error).message}`);
    }
  }

  /** Machines that are online and report a daemon older than the bundled build. */
  outdated(): MachineView[] {
    if (!this.bundledVersion) return [];
    return this.fleet.machines().filter((m) => m.online && isNewer(this.bundledVersion, m.entry.snapshot.daemonVersion));
  }

  private scheduleScan(): void {
    clearTimeout(this.debounce);
    this.debounce = setTimeout(() => void this.scan(), 2000);
  }

  private async scan(): Promise<void> {
    if (!this.bundledVersion || this.client.state !== 'connected') return;
    const targets = this.outdated().filter((m) => !this.attempted.has(`${m.id}@${this.bundledVersion}`) && !this.inFlight.has(m.id));
    if (!targets.length) return;
    if (!this.cfg('autoUpdateDaemons', true)) {
      if (this.warnedManual) return;
      this.warnedManual = true;
      const names = targets.map((m) => `${m.name} (${m.entry.snapshot.daemonVersion ?? '?'})`).join(', ');
      const pick = await vscode.window.showInformationMessage(`Vineyard ${this.bundledVersion} is bundled with this extension; older daemons: ${names}.`, 'Update all', 'Not now');
      if (pick === 'Update all') await this.updateAll();
      return;
    }
    for (const m of targets) {
      this.attempted.add(`${m.id}@${this.bundledVersion}`);
      void this.updateMachine(m, { auto: true }).catch((err) => {
        this.log.appendLine(`auto-update of ${m.name} failed: ${(err as Error).message}`);
        void vscode.window.showWarningMessage(`Vineyard could not update the daemon on ${m.name}: ${(err as Error).message}`, 'Show Log').then((c) => c && this.log.show());
      });
    }
  }

  async updateAll(): Promise<void> {
    const targets = this.outdated();
    if (!targets.length) {
      void vscode.window.showInformationMessage(this.bundledVersion ? `Every online daemon already runs ${this.bundledVersion}.` : 'The bundled daemon version is unknown on this platform.');
      return;
    }
    await Promise.all(targets.map((m) => this.updateMachine(m, { auto: false })));
  }

  /**
   * Bring one machine's daemon up to the bundled build. Prefers the mesh push; falls back to the SSH /
   * local installer when the running daemon predates the `upgrade` request or is offline.
   */
  async updateMachine(m: MachineView, opts: { auto: boolean; force?: boolean } = { auto: false }): Promise<void> {
    if (this.inFlight.has(m.id)) return;
    this.inFlight.add(m.id);
    try {
      if (!m.online) {
        if (opts.auto) return;
        this.log.appendLine(`${m.name} is offline; installing over SSH`);
        return await this.setup.updateMachine(m);
      }
      const location = opts.auto ? vscode.ProgressLocation.Window : vscode.ProgressLocation.Notification;
      await vscode.window.withProgress({ location, title: `Vineyard: updating daemon on ${m.name}` }, async (progress) => {
        const pushed = await this.push(m, opts.force ?? false, progress);
        if (pushed === 'unsupported') {
          this.log.appendLine(`${m.name} runs a daemon without in-band upgrade support; using the installer instead`);
          progress.report({ message: 'legacy daemon: installing the old way' });
          await this.setup.updateMachine(m);
          return;
        }
        progress.report({ message: `waiting for ${m.name} to come back` });
        const ok = await this.waitForVersion(m.id, pushed, 90_000);
        if (!ok) throw new Error(`${m.name} accepted the update but has not reported version ${pushed} yet; check its daemon log.`);
      });
      void vscode.window.showInformationMessage(`Vineyard daemon on ${m.name} is now ${this.bundledVersion}.`);
    } finally {
      this.inFlight.delete(m.id);
    }
  }

  /** Streams the bundled binary for m's platform. Resolves to the new version, or 'unsupported'. */
  private async push(m: MachineView, force: boolean, progress: vscode.Progress<{ message?: string; increment?: number }>): Promise<string | 'unsupported'> {
    const host = m.entry.snapshot.host;
    const osName = host.os as RemoteOS | undefined;
    const arch = host.arch as RemoteArch | undefined;
    if (!osName || !arch) throw new Error(`${m.name} has not reported its platform yet`);
    const file = this.setup.binaryFor(osName, arch);
    const data = fs.readFileSync(file);
    const sha256 = crypto.createHash('sha256').update(data).digest('hex');
    const target = m.local ? undefined : m.id;
    const version = this.bundledVersion ?? 'unknown';
    this.log.appendLine(`pushing ${path.basename(file)} (${data.length} bytes, ${sha256.slice(0, 12)}) to ${m.name}`);
    let sent = 0;
    while (sent < data.length) {
      const end = Math.min(sent + CHUNK, data.length);
      const done = end === data.length;
      let res: UpgradeResult;
      try {
        res = await this.client.request<UpgradeResult>('upgrade', target, { version, sha256, size: data.length, offset: sent, data: data.subarray(sent, end).toString('base64'), done, force }, 60_000);
      } catch (err) {
        const msg = (err as Error).message;
        if (/unknown op/i.test(msg)) return 'unsupported';
        throw err;
      }
      progress.report({ message: `${Math.round((end / data.length) * 100)}%`, increment: ((end - sent) / data.length) * 100 });
      sent = end;
      if (done) {
        if (!res.installed) throw new Error('daemon did not confirm the install');
        return res.version ?? version;
      }
    }
    throw new Error('empty binary');
  }

  private waitForVersion(machineId: string, version: string, timeoutMs: number): Promise<boolean> {
    return new Promise((resolve) => {
      const done = (v: boolean) => {
        clearTimeout(timer);
        sub.dispose();
        resolve(v);
      };
      const check = () => {
        const m = this.fleet.machine(machineId);
        if (m?.online && m.entry.snapshot.daemonVersion === version && m.entry.via !== 'cache') done(true);
      };
      const sub = this.fleet.onDidChange(check);
      const timer = setTimeout(() => done(false), timeoutMs);
    });
  }

  // ---- extension ------------------------------------------------------------------------------

  async checkExtensionUpdate(interactive: boolean): Promise<void> {
    if (!interactive) {
      if (!this.cfg('checkForUpdates', true)) return;
      const last = this.context.globalState.get<number>(LAST_CHECK_KEY, 0);
      if (Date.now() - last < CHECK_EVERY_MS) return;
    }
    const current = (this.context.extension.packageJSON as { version?: string }).version;
    try {
      const res = await fetch(RELEASES_API, { headers: { Accept: 'application/vnd.github+json', 'User-Agent': `vineyard-vscode/${current ?? 'dev'}` } });
      await this.context.globalState.update(LAST_CHECK_KEY, Date.now());
      if (!res.ok) throw new Error(`GitHub API returned ${res.status}`);
      const rel = (await res.json()) as { tag_name: string; html_url: string; assets: { name: string; browser_download_url: string }[] };
      const latest = rel.tag_name.replace(/^v/, '');
      if (!isNewer(latest, current)) {
        if (interactive) void vscode.window.showInformationMessage(`Vineyard ${current} is up to date.`);
        return;
      }
      if (!interactive && this.context.globalState.get<string>(SKIPPED_KEY) === latest) return;
      const asset = rel.assets.find((a) => a.name.endsWith('.vsix'));
      const choice = await vscode.window.showInformationMessage(`Vineyard ${latest} is available (you have ${current}).`, asset ? 'Download and install' : 'Open release', 'Release notes', 'Skip this version');
      if (choice === 'Skip this version') await this.context.globalState.update(SKIPPED_KEY, latest);
      else if (choice === 'Release notes' || choice === 'Open release') await vscode.env.openExternal(vscode.Uri.parse(rel.html_url));
      else if (choice === 'Download and install' && asset) await this.installVsix(asset.browser_download_url, asset.name);
    } catch (err) {
      this.log.appendLine(`update check failed: ${(err as Error).message}`);
      if (interactive) throw err;
    }
  }

  private async installVsix(url: string, name: string): Promise<void> {
    await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: `Vineyard: downloading ${name}` }, async () => {
      const dir = this.context.globalStorageUri.fsPath;
      fs.mkdirSync(dir, { recursive: true });
      const file = path.join(dir, name);
      const res = await fetch(url, { headers: { 'User-Agent': 'vineyard-vscode' } });
      if (!res.ok) throw new Error(`download failed: ${res.status}`);
      fs.writeFileSync(file, Buffer.from(await res.arrayBuffer()));
      await vscode.commands.executeCommand('workbench.extensions.installExtension', vscode.Uri.file(file));
    });
    const reload = await vscode.window.showInformationMessage('Vineyard was updated. Reload to finish; the new build then updates the fleet daemons on its own.', 'Reload Window');
    if (reload) await vscode.commands.executeCommand('workbench.action.reloadWindow');
  }

  dispose(): void {
    clearTimeout(this.debounce);
    for (const s of this.subs) s.dispose();
  }
}
