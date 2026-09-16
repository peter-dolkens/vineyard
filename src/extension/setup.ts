/**
 * Bootstrap flows: set up the local daemon, install/update the daemon on another machine over SSH,
 * remove a machine, restart, show logs.
 */

import * as vscode from 'vscode';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { DEFAULT_PORT, readLocalConfig, vineyardDir, type DaemonClient, type PeerAddr } from './daemonClient.ts';
import type { FleetService, MachineView } from './fleet.ts';
import { detectRemote, effectiveHost, failed, run, scp, ssh, type RemoteArch, type RemoteOS, type SshTarget } from './ssh.ts';

const PLATFORM_OS: Record<string, RemoteOS> = { darwin: 'darwin', linux: 'linux', win32: 'windows' };
const PLATFORM_ARCH: Record<string, RemoteArch> = { arm64: 'arm64', x64: 'amd64', ia32: '386' };

export class Setup {
  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly client: DaemonClient,
    private readonly fleet: FleetService,
    private readonly log: vscode.OutputChannel,
  ) {}

  binaryFor(osName: RemoteOS, arch: RemoteArch): string {
    const file = `vineyardd-${osName}-${arch}${osName === 'windows' ? '.exe' : ''}`;
    const p = path.join(this.context.extensionPath, 'bin', file);
    if (!fs.existsSync(p)) {
      throw new Error(`Daemon binary ${file} is not bundled. Run scripts/build-daemon.sh in the extension repo.`);
    }
    return p;
  }

  private localBinary(): string {
    const osName = PLATFORM_OS[process.platform];
    const arch = PLATFORM_ARCH[process.arch];
    if (!osName || !arch) throw new Error(`Unsupported local platform ${process.platform}/${process.arch}`);
    return this.binaryFor(osName, arch);
  }

  private port(): number {
    return vscode.workspace.getConfiguration('vineyard').get<number>('daemon.port', DEFAULT_PORT);
  }

  /** Every peer the new machine should know about: all machines we know, plus ourselves. */
  private knownPeers(exclude: string): PeerAddr[] {
    const peers = new Map<string, string>();
    const cfg = readLocalConfig();
    if (cfg?.advertise) peers.set(cfg.machineId, cfg.advertise);
    for (const p of cfg?.peers ?? []) peers.set(p.machineId, p.addr);
    for (const m of this.fleet.machines()) {
      const addr = m.entry.snapshot.listen || m.peer?.addr;
      if (addr) peers.set(m.id, addr);
    }
    peers.delete(exclude);
    return [...peers].map(([machineId, addr]) => ({ machineId, addr }));
  }

  // ---- local ------------------------------------------------------------------------------------

  async setupLocal(): Promise<void> {
    const bin = this.localBinary();
    const hostname = os.hostname().toLowerCase();
    const port = this.port();
    await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: 'Vineyard: setting up this machine' }, async (progress) => {
      this.log.show(true);
      if (!readLocalConfig()) {
        progress.report({ message: 'writing config and fleet certificate' });
        const r = await run(bin, ['init', '--machine-id', hostname, '--name', hostname.split('.')[0] ?? hostname, '--port', String(port), '--advertise', `${hostname}:${port}`], { log: this.log });
        const err = failed(r);
        if (err) throw new Error(`vineyardd init failed: ${err}`);
      }
      progress.report({ message: 'installing background service' });
      const r = await run(bin, ['install'], { log: this.log, timeoutMs: 60_000 });
      const err = failed(r);
      if (err) throw new Error(`vineyardd install failed: ${err}`);
      this.client.reconnectNow();
    });
    void vscode.window.showInformationMessage(`Vineyard daemon installed on ${hostname}. Add other machines from the Vineyard view.`);
  }

  // ---- remote -----------------------------------------------------------------------------------

  async addMachine(): Promise<void> {
    if (!readLocalConfig()) {
      const pick = await vscode.window.showWarningMessage('Set up this machine first so there is a fleet certificate to share.', 'Set Up This Machine');
      if (pick) await this.setupLocal();
      if (!readLocalConfig()) return;
    }
    const host = await vscode.window.showInputBox({
      title: 'Add machine',
      prompt: 'SSH host of the machine (hostname, IP or ~/.ssh/config alias). You must be able to SSH to it without a password prompt.',
      placeHolder: 'falcon.dolkens.net',
      validateInput: (v) => (v.trim() ? undefined : 'Host is required'),
    });
    if (!host) return;
    const userAt = host.includes('@') ? host.split('@')[0] : undefined;
    const bareHost = host.includes('@') ? host.split('@')[1]! : host.trim();
    await this.installRemote({ host: bareHost, user: userAt }, bareHost.toLowerCase());
  }

  async installRemote(target: SshTarget, machineId: string): Promise<void> {
    const cfg = readLocalConfig();
    if (!cfg) throw new Error('Local Vineyard config missing; run "Set Up This Machine" first.');
    const port = this.port();
    const advertiseHost = await effectiveHost(target);
    const advertise = `${advertiseHost}:${port}`;
    const name = machineId.split('.')[0] ?? machineId;
    const peers = this.knownPeers(machineId);

    await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: `Vineyard: installing daemon on ${name}`, cancellable: false }, async (progress) => {
      this.log.show(true);
      progress.report({ message: 'detecting platform' });
      const plat = await detectRemote(target, this.log);
      const bin = this.binaryFor(plat.os, plat.arch);
      this.log.appendLine(`remote ${target.host}: ${plat.os}/${plat.arch}`);

      const check = (r: Awaited<ReturnType<typeof ssh>>, what: string) => {
        const err = failed(r);
        if (err) throw new Error(`${what}: ${err}`);
      };

      progress.report({ message: 'copying daemon and fleet certificate' });
      if (plat.os === 'windows') {
        check(await ssh(target, 'powershell -NoProfile -Command "New-Item -ItemType Directory -Force $HOME\\.vineyard\\bin | Out-Null"', { log: this.log }), 'create directory');
      } else {
        check(await ssh(target, 'mkdir -p ~/.vineyard/bin && chmod 700 ~/.vineyard', { log: this.log }), 'create directory');
      }
      const newBin = plat.os === 'windows' ? '.vineyard/bin/vineyardd.upload.exe' : '.vineyard/bin/vineyardd.upload';
      check(await scp(target, bin, newBin, this.log), 'copy binary');
      check(await scp(target, path.join(cfg.dir, 'fleet.crt'), '.vineyard/fleet.crt', this.log), 'copy certificate');
      check(await scp(target, path.join(cfg.dir, 'fleet.key'), '.vineyard/fleet.key', this.log), 'copy key');

      progress.report({ message: 'configuring and starting service' });
      const initArgs = `init --machine-id ${q(machineId)} --name ${q(name)} --port ${port} --advertise ${q(advertise)}`;
      const peerCmds = peers.map((p) => ({ id: p.machineId, addr: p.addr }));
      if (plat.os === 'windows') {
        const exe = '$HOME\\.vineyard\\bin\\vineyardd.upload.exe';
        const script = [
          `if (!(Test-Path $HOME\\.vineyard\\config.json)) { & ${exe} ${initArgs}; if ($LASTEXITCODE -ne 0) { exit 1 } }`,
          ...peerCmds.map((p) => `& ${exe} peer add ${q(p.id)} ${q(p.addr)}`),
          `& ${exe} install; if ($LASTEXITCODE -ne 0) { exit 1 }`,
          `Remove-Item ${exe} -ErrorAction SilentlyContinue`,
          `& $HOME\\.vineyard\\bin\\vineyardd.exe version`,
        ].join('; ');
        const r = await ssh(target, `powershell -NoProfile -Command "${script.replace(/"/g, '\\"')}"`, { log: this.log, timeoutMs: 120_000 });
        check(r, 'install');
        verifyVersion(r.stdout);
      } else {
        const exe = '~/.vineyard/bin/vineyardd.upload';
        const script = [
          `chmod 600 ~/.vineyard/fleet.crt ~/.vineyard/fleet.key`,
          `chmod +x ${exe}`,
          `if [ ! -f ~/.vineyard/config.json ]; then ${exe} ${initArgs}; fi`,
          ...peerCmds.map((p) => `${exe} peer add ${q(p.id)} ${q(p.addr)}`),
          `${exe} install`,
          `rm -f ${exe}`,
          `~/.vineyard/bin/vineyardd version`,
        ].join(' && ');
        const r = await ssh(target, `sh -c ${q(script)}`, { log: this.log, timeoutMs: 120_000 });
        check(r, 'install');
        verifyVersion(r.stdout);
      }

      progress.report({ message: 'registering peer locally' });
      await this.client.request('addpeer', undefined, { machineId, addr: advertise }, 10_000);
      progress.report({ message: `waiting for ${name} to connect` });
      const online = await this.waitOnline(machineId, 20_000);
      if (!online) {
        const m = this.fleet.machine(machineId);
        throw new Error(`Daemon installed on ${name} but it is not reachable at ${advertise}${m?.peer?.lastError ? ` (${m.peer.lastError})` : ''}. Check firewall/port ${port} and the daemon log.`);
      }
    });
    void vscode.window.showInformationMessage(`Vineyard daemon installed on ${name} and connected.`);
  }

  // ---- invites (no SSH needed) ----------------------------------------------------------------

  /** Ask the local daemon for a single-use invite code and hand it to the user. */
  async createInvite(): Promise<void> {
    if (this.client.state !== 'connected') throw new Error('Not connected to the local daemon; set up this machine first.');
    const res = await this.client.request<{ code: string; expiresInSeconds: number }>('invite', undefined, undefined, 10_000);
    const minutes = Math.round(res.expiresInSeconds / 60);
    await vscode.env.clipboard.writeText(res.code);
    const uri = `vscode://dolkens.vineyard/join?code=${encodeURIComponent(res.code)}`;
    const choice = await vscode.window.showInformationMessage(
      `Invite code copied to the clipboard (single use, valid ${minutes} min). On the other machine run "Vineyard: Join Fleet with Invite Code" and paste it, or open the link.`,
      'Copy as vscode:// link',
      'Show code',
    );
    if (choice === 'Copy as vscode:// link') await vscode.env.clipboard.writeText(uri);
    if (choice === 'Show code') await vscode.window.showInputBox({ title: 'Vineyard invite code', value: res.code, prompt: 'Paste this on the joining machine. It also works as a vscode:// link.' });
  }

  /** Join an existing fleet from this machine using an invite code; then install the service. */
  async joinWithCode(code?: string): Promise<void> {
    code = code?.trim() || (await vscode.window.showInputBox({ title: 'Join fleet', prompt: 'Paste the invite code from another machine (starts with vineyard: or vscode://)', ignoreFocusOut: true }))?.trim();
    if (!code) return;
    const bin = this.localBinary();
    const hostname = os.hostname().toLowerCase();
    const port = this.port();
    const existing = readLocalConfig();
    if (existing) {
      const ok = await vscode.window.showWarningMessage('This machine already belongs to a fleet. Joining replaces its certificate and peer list.', { modal: true }, 'Replace and join');
      if (!ok) return;
    }
    await vscode.window.withProgress({ location: vscode.ProgressLocation.Notification, title: 'Vineyard: joining fleet' }, async (progress) => {
      this.log.show(true);
      progress.report({ message: 'contacting the inviting machine' });
      const args = ['join', code!, '--machine-id', hostname, '--name', hostname.split('.')[0] ?? hostname, '--port', String(port), '--advertise', `${hostname}:${port}`];
      if (existing) args.push('--force');
      const r = await run(bin, args, { log: this.log, timeoutMs: 60_000 });
      const err = failed(r);
      if (err) throw new Error(`join failed: ${err}`);
      progress.report({ message: 'installing background service' });
      const i = await run(bin, ['install'], { log: this.log, timeoutMs: 60_000 });
      const ierr = failed(i);
      if (ierr) throw new Error(`vineyardd install failed: ${ierr}`);
      this.client.reconnectNow();
    });
    void vscode.window.showInformationMessage(`Joined the fleet. ${hostname} will appear on the other machines as soon as they look.`);
  }

  private waitOnline(machineId: string, timeoutMs: number): Promise<boolean> {
    return new Promise((resolve) => {
      const done = (v: boolean) => {
        clearTimeout(timer);
        sub.dispose();
        resolve(v);
      };
      const check = () => {
        if (this.fleet.machine(machineId)?.online) done(true);
      };
      const sub = this.fleet.onDidChange(check);
      const timer = setTimeout(() => done(false), timeoutMs);
      check();
    });
  }

  async updateMachine(m: MachineView): Promise<void> {
    if (m.local) return this.setupLocal();
    return this.installRemote({ host: m.host }, m.id);
  }

  async removeMachine(m: MachineView): Promise<void> {
    if (m.local) {
      void vscode.window.showInformationMessage('This machine is always shown. Uninstall the daemon with "vineyardd uninstall" if you no longer want it.');
      return;
    }
    const choice = await vscode.window.showWarningMessage(`Remove ${m.name} from the fleet?`, { modal: true }, 'Remove', 'Remove and uninstall daemon');
    if (!choice) return;
    await this.client.request('removepeer', undefined, { machineId: m.id, addr: '' }, 10_000);
    if (choice === 'Remove and uninstall daemon') {
      const r = await ssh({ host: m.host }, 'sh -c "~/.vineyard/bin/vineyardd uninstall" || powershell -NoProfile -Command "& $HOME\\.vineyard\\bin\\vineyardd.exe uninstall"', { log: this.log, timeoutMs: 60_000 });
      const err = failed(r);
      if (err) void vscode.window.showWarningMessage(`Removed locally, but uninstall on ${m.name} failed: ${err}`);
    }
  }

  async restartDaemon(m: MachineView): Promise<void> {
    if (m.local) {
      const r = await run(this.localBinary(), ['restart'], { log: this.log, timeoutMs: 30_000 });
      const err = failed(r);
      if (err) throw new Error(err);
      this.client.reconnectNow();
      return;
    }
    const r = await ssh({ host: m.host }, '~/.vineyard/bin/vineyardd restart || powershell -NoProfile -Command "& $HOME\\.vineyard\\bin\\vineyardd.exe restart"', { log: this.log, timeoutMs: 30_000 });
    const err = failed(r);
    if (err) throw new Error(err);
  }

  showDaemonLog(m: MachineView): void {
    if (m.local) {
      const file = path.join(vineyardDir(), 'vineyardd.log');
      void vscode.window.showTextDocument(vscode.Uri.file(file), { preview: true });
      return;
    }
    const term = vscode.window.createTerminal({ name: `vineyardd log: ${m.name}` });
    term.sendText(`ssh -t ${m.host} 'tail -n 200 -f ~/.vineyard/vineyardd.log'`);
    term.show();
  }
}

/** The install script ends with `vineyardd version`; an empty result means the binary is broken. */
function verifyVersion(stdout: string): void {
  const last = stdout.trim().split('\n').pop() ?? '';
  if (!/^[0-9A-Za-z.\-+]+$/.test(last)) {
    throw new Error(`installed daemon did not report a version (output: ${JSON.stringify(last.slice(0, 80))}); the binary may be corrupt`);
  }
}

function q(s: string): string {
  if (/^[A-Za-z0-9_\-.:/~=]+$/.test(s)) return s;
  return `'${s.replace(/'/g, `'\\''`)}'`;
}
