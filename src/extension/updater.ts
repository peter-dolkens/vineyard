/**
 * Keeping the fleet current.
 *
 * Daemons: the extension bundles a vineyardd build for every platform, but on its own it only ever
 * updates the daemon on this machine: it streams the binary over loopback (the `upgrade` request), the
 * daemon verifies and installs it, and the new daemon then brings its peers up to its version itself,
 * over its direct connections to them. Remote pushes from here would be relayed through the very
 * daemon that is about to restart, which is how a fleet update once died halfway. For peers on other
 * platforms the daemon needs their binaries, so once it is current we seed its distribution store
 * (the `dist` and `stage` requests) with whatever its peers' platforms call for.
 *
 * "Update all" and the per-machine command still push directly as a fallback; a daemon already
 * receiving the same build from someone else says so, and we wait for it instead of failing.
 * Daemons too old to know the `upgrade` request are updated once over SSH (or locally) instead.
 *
 * Extension: at most once a day, one request to the GitHub Releases API; if a newer release exists we
 * offer to download and install the .vsix. Both behaviours have settings.
 */

import * as vscode from 'vscode';
import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as path from 'node:path';
import { compareVersions, isNewer } from '../core/version.ts';
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
  stored?: boolean;
  version?: string;
}

interface DistResult {
  version: string;
  have: string[];
  want?: string[];
}

export class Updater implements vscode.Disposable {
  /** Version reported by the bundled daemon for this platform; undefined until probed or if unsupported. */
  bundledVersion: string | undefined;
  private readonly subs: vscode.Disposable[] = [];
  private readonly inFlight = new Set<string>();
  private readonly attempted = new Set<string>(); // `${machineId}@${bundledVersion}`: one automatic try per version
  private readonly announced = new Set<string>(); // `${machineId}@${daemonVersion}` remote machines we have logged as outdated
  private outdatedBefore = new Map<string, string>(); // machineId -> name, for reporting daemon-driven updates
  private seeding: Promise<void> | undefined;
  private lastDistKey = '';
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
    this.noteFleetChanges();
    const outdated = this.outdated();
    const local = outdated.find((m) => m.local);
    const auto = this.cfg('autoUpdateDaemons', true);
    if (local) {
      // Ourselves first, and only ourselves: anything we pushed to a remote machine now would be relayed
      // through the daemon we are about to restart. Once it is back it updates its peers.
      if (this.inFlight.has(local.id) || this.attempted.has(`${local.id}@${this.bundledVersion}`)) return;
      if (!auto) {
        await this.offerManual(outdated);
        return;
      }
      this.attempted.add(`${local.id}@${this.bundledVersion}`);
      void this.updateMachine(local, { auto: true }).catch((err) => {
        this.log.appendLine(`auto-update of ${local.name} failed: ${(err as Error).message}`);
        void vscode.window.showWarningMessage(`Vineyard could not update the daemon on ${local.name}: ${(err as Error).message}`, 'Show Log').then((c) => c && this.log.show());
      });
      return;
    }
    // The local daemon is current. Give it the binaries its peers need and leave the rest to it.
    void this.seed();
    if (!auto && outdated.length) await this.offerManual(outdated);
  }

  private async offerManual(outdated: MachineView[]): Promise<void> {
    if (this.warnedManual) return;
    this.warnedManual = true;
    const names = outdated.map((m) => `${m.name} (${m.entry.snapshot.daemonVersion ?? '?'})`).join(', ');
    const pick = await vscode.window.showInformationMessage(`Vineyard ${this.bundledVersion} is bundled with this extension; older daemons: ${names}.`, 'Update all', 'Not now');
    if (pick === 'Update all') await this.updateAll();
  }

  /** Logs remote machines that fall behind and reports the ones the daemon has since brought current. */
  private noteFleetChanges(): void {
    const now = new Map<string, string>();
    for (const m of this.outdated()) {
      now.set(m.id, m.name);
      const key = `${m.id}@${m.entry.snapshot.daemonVersion}`;
      if (!m.local && !this.announced.has(key)) {
        this.announced.add(key);
        this.log.appendLine(`${m.name} runs ${m.entry.snapshot.daemonVersion ?? '?'} (${m.entry.snapshot.host.os}-${m.entry.snapshot.host.arch}); leaving the update to the local daemon`);
      }
    }
    for (const [id, name] of this.outdatedBefore) {
      if (now.has(id) || this.inFlight.has(id)) continue;
      const m = this.fleet.machine(id);
      if (m?.local) continue; // updateMachine reports our own daemon
      if (m?.online && compareVersions(m.entry.snapshot.daemonVersion, this.bundledVersion) >= 0) {
        this.log.appendLine(`${name} now runs ${m.entry.snapshot.daemonVersion}`);
        void vscode.window.showInformationMessage(`Vineyard daemon on ${name} is now ${m.entry.snapshot.daemonVersion}.`);
      }
    }
    this.outdatedBefore = now;
  }

  async updateAll(): Promise<void> {
    let targets = this.outdated();
    if (!targets.length) {
      void vscode.window.showInformationMessage(this.bundledVersion ? `Every online daemon already runs ${this.bundledVersion}.` : 'The bundled daemon version is unknown on this platform.');
      return;
    }
    const local = targets.find((m) => m.local);
    if (local) await this.updateMachine(local, { auto: false });
    await this.seed(true);
    // Whatever is still behind: the daemon is normally already on it (in which case it tells us to wait);
    // pushing from here also covers a machine the local daemon cannot upgrade itself.
    targets = this.outdated().filter((m) => !m.local);
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
        let pushed = await this.push(m, opts.force ?? false, progress);
        if (pushed === 'unsupported') {
          this.log.appendLine(`${m.name} runs a daemon without in-band upgrade support; using the installer instead`);
          progress.report({ message: 'legacy daemon: installing the old way' });
          await this.setup.updateMachine(m);
          return;
        }
        if (pushed === 'busy') {
          this.log.appendLine(`${m.name} is already receiving this build from another sender; waiting for it`);
          progress.report({ message: 'another update is already in progress; waiting' });
          if (await this.waitForVersion(m.id, this.bundledVersion ?? '', 120_000)) return;
          // Whoever that was has gone quiet; the daemon drops an idle upload after a minute, so try again.
          pushed = await this.push(m, opts.force ?? false, progress);
          if (pushed === 'unsupported' || pushed === 'busy') throw new Error(`${m.name} is still busy with another update; try again in a minute.`);
        }
        progress.report({ message: `waiting for ${m.name} to come back` });
        const ok = await this.waitForVersion(m.id, pushed, 90_000);
        if (!ok) throw new Error(`${m.name} accepted the update but has not reported version ${pushed} yet; check its daemon log.`);
      });
      void vscode.window.showInformationMessage(`Vineyard daemon on ${m.name} is now ${this.bundledVersion}.`);
      if (m.local) await this.seed(true);
    } finally {
      this.inFlight.delete(m.id);
    }
  }

  /**
   * Streams the bundled binary for m's platform. Resolves to the new version; 'unsupported' when the
   * daemon predates the request; 'busy' when it is already taking this build from someone else.
   */
  private async push(m: MachineView, force: boolean, progress: vscode.Progress<{ message?: string; increment?: number }>): Promise<string | 'unsupported' | 'busy'> {
    const host = m.entry.snapshot.host;
    const osName = host.os as RemoteOS | undefined;
    const arch = host.arch as RemoteArch | undefined;
    if (!osName || !arch) throw new Error(`${m.name} has not reported its platform yet`);
    const file = this.setup.binaryFor(osName, arch);
    const version = this.bundledVersion ?? 'unknown';
    let res: UpgradeResult;
    try {
      res = await this.stream(m, 'upgrade', file, { version, force }, progress);
    } catch (err) {
      const msg = (err as Error).message;
      if (/unknown op/i.test(msg)) return 'unsupported';
      if (/already in progress/i.test(msg)) return 'busy';
      if (/already running/i.test(msg)) return version;
      throw err;
    }
    if (!res.installed) throw new Error('daemon did not confirm the install');
    return res.version ?? version;
  }

  /** Sends file to m (or the local daemon) in chunks under op, with the given extra arguments on every chunk. */
  private async stream(m: MachineView | undefined, op: 'upgrade' | 'stage', file: string, extra: Record<string, unknown>, progress?: vscode.Progress<{ message?: string; increment?: number }>): Promise<UpgradeResult> {
    const data = fs.readFileSync(file);
    const sha256 = crypto.createHash('sha256').update(data).digest('hex');
    const target = !m || m.local ? undefined : m.id;
    this.log.appendLine(`${op === 'stage' ? 'staging' : 'pushing'} ${path.basename(file)} (${data.length} bytes, ${sha256.slice(0, 12)}) ${m ? `to ${m.name}` : 'on the local daemon'}`);
    let sent = 0;
    while (sent < data.length) {
      const end = Math.min(sent + CHUNK, data.length);
      const done = end === data.length;
      const res = await this.client.request<UpgradeResult>(op, target, { ...extra, sha256, size: data.length, offset: sent, data: data.subarray(sent, end).toString('base64'), done }, 60_000);
      progress?.report({ message: `${Math.round((end / data.length) * 100)}%`, increment: ((end - sent) / data.length) * 100 });
      sent = end;
      if (done) return res;
    }
    throw new Error('empty binary');
  }

  /**
   * Gives the local daemon the builds of its own version that its peers' platforms need, so it can
   * update them itself. Peers on the daemon's own platform need nothing: it sends its own executable.
   */
  private seed(force = false): Promise<void> {
    if (this.seeding) return this.seeding;
    this.seeding = this.doSeed(force)
      .catch((err) => this.log.appendLine(`could not seed the local daemon with fleet binaries: ${(err as Error).message}`))
      .finally(() => (this.seeding = undefined));
    return this.seeding;
  }

  private async doSeed(force: boolean): Promise<void> {
    if (!this.bundledVersion || this.client.state !== 'connected') return;
    const local = this.fleet.machines().find((m) => m.local);
    // Only a daemon on our bundled build is ours to seed; an older one is about to be updated, a newer one
    // belongs to a newer extension.
    if (!local?.online || compareVersions(local.entry.snapshot.daemonVersion, this.bundledVersion) !== 0) return;
    const platforms = this.fleet.machines().map((m) => `${m.entry.snapshot.host.os}-${m.entry.snapshot.host.arch}`).sort();
    const key = `${local.entry.snapshot.daemonVersion}:${[...new Set(platforms)].join(',')}`;
    if (!force && key === this.lastDistKey) return;
    this.lastDistKey = key;
    let dist: DistResult;
    try {
      dist = await this.client.request<DistResult>('dist', undefined, {}, 15_000);
    } catch (err) {
      if (/unknown op/i.test((err as Error).message)) return;
      throw err;
    }
    if (compareVersions(dist.version, this.bundledVersion) !== 0) return;
    for (const platform of dist.want ?? []) {
      const [osName, arch] = platform.split('-') as [RemoteOS, RemoteArch];
      let file: string;
      try {
        file = this.setup.binaryFor(osName, arch);
      } catch (err) {
        this.log.appendLine(`cannot seed ${platform}: ${(err as Error).message}`);
        continue;
      }
      await vscode.window.withProgress({ location: vscode.ProgressLocation.Window, title: `Vineyard: giving ${local.name} the ${platform} daemon` }, async (progress) => {
        const res = await this.stream(undefined, 'stage', file, { version: dist.version, platform }, progress);
        if (!res.stored) throw new Error(`the daemon did not confirm storing the ${platform} build`);
      });
    }
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
        if (m?.online && m.entry.via !== 'cache' && compareVersions(m.entry.snapshot.daemonVersion, version) === 0 && m.entry.snapshot.daemonVersion) done(true);
      };
      const sub = this.fleet.onDidChange(check);
      const timer = setTimeout(() => done(false), timeoutMs);
    });
  }

  // ---- extension ------------------------------------------------------------------------------

  async checkExtensionUpdate(interactive: boolean): Promise<void> {
    if (!interactive) {
      if (!this.cfg('checkForUpdates', true)) return;
      // Marketplace installs are kept current by VS Code itself; only VSIX installs need our check.
      const meta = (this.context.extension.packageJSON as { __metadata?: { source?: string } }).__metadata;
      if (meta?.source === 'gallery') return;
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
