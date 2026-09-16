/**
 * Minimal ssh/scp helpers used only to bootstrap a machine: copy the daemon binary and fleet
 * certificate, then run `vineyardd init|install`. Day-to-day traffic never goes over SSH.
 */

import { spawn } from 'node:child_process';
import * as vscode from 'vscode';

export interface SshTarget {
  host: string;
  user?: string;
  port?: number;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  code: number | null;
  timedOut: boolean;
}

export type RemoteOS = 'darwin' | 'linux' | 'windows';
export type RemoteArch = 'arm64' | 'amd64';

export function sshUserHost(t: SshTarget): string {
  return t.user ? `${t.user}@${t.host}` : t.host;
}

function extraArgs(): string[] {
  return vscode.workspace.getConfiguration('vineyard').get<string[]>('ssh.extraArgs', []);
}

function baseArgs(t: SshTarget, portFlag: string): string[] {
  const args = ['-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', '-o', 'StrictHostKeyChecking=accept-new', '-o', 'LogLevel=ERROR'];
  if (t.port) args.push(portFlag, String(t.port));
  args.push(...extraArgs());
  return args;
}

export function run(cmd: string, args: string[], opts: { stdin?: string | Buffer; timeoutMs?: number; log?: vscode.OutputChannel } = {}): Promise<ExecResult> {
  return new Promise((resolve) => {
    opts.log?.appendLine(`$ ${cmd} ${args.map((a) => (/\s/.test(a) ? JSON.stringify(a) : a)).join(' ')}`);
    const child = spawn(cmd, args, { stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
    let stdout = '';
    let stderr = '';
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill('SIGKILL');
    }, opts.timeoutMs ?? 120_000);
    child.stdout.on('data', (d: Buffer) => {
      stdout += d.toString();
      opts.log?.append(d.toString());
    });
    child.stderr.on('data', (d: Buffer) => {
      stderr += d.toString();
      opts.log?.append(d.toString());
    });
    child.on('error', (err) => {
      clearTimeout(timer);
      resolve({ stdout, stderr: stderr + err.message, code: -1, timedOut });
    });
    child.on('close', (code) => {
      clearTimeout(timer);
      resolve({ stdout, stderr, code, timedOut });
    });
    child.stdin.on('error', () => undefined);
    child.stdin.end(opts.stdin);
  });
}

export function ssh(t: SshTarget, command: string, opts: { stdin?: string | Buffer; timeoutMs?: number; log?: vscode.OutputChannel } = {}): Promise<ExecResult> {
  return run('ssh', [...baseArgs(t, '-p'), sshUserHost(t), command], opts);
}

export function scp(t: SshTarget, localPath: string, remotePath: string, log?: vscode.OutputChannel): Promise<ExecResult> {
  return run('scp', [...baseArgs(t, '-P'), localPath, `${sshUserHost(t)}:${remotePath}`], { log, timeoutMs: 300_000 });
}

export function failed(r: ExecResult): string | undefined {
  if (r.timedOut) return 'timed out';
  if (r.code !== 0) return (r.stderr || r.stdout).trim() || `exit code ${r.code}`;
  return undefined;
}

export async function detectRemote(t: SshTarget, log?: vscode.OutputChannel): Promise<{ os: RemoteOS; arch: RemoteArch }> {
  const u = await ssh(t, 'uname -sm', { timeoutMs: 20_000, log });
  const out = u.stdout.trim();
  if (u.code === 0 && /^(Darwin|Linux)\s/.test(out)) {
    const [os, machine] = out.split(/\s+/);
    const arch: RemoteArch = /arm64|aarch64/i.test(machine ?? '') ? 'arm64' : 'amd64';
    return { os: os === 'Darwin' ? 'darwin' : 'linux', arch };
  }
  const p = await ssh(t, 'powershell -NoProfile -Command "$env:PROCESSOR_ARCHITECTURE"', { timeoutMs: 20_000, log });
  if (p.code === 0 && p.stdout.trim()) {
    return { os: 'windows', arch: /arm64/i.test(p.stdout) ? 'arm64' : 'amd64' };
  }
  const why = failed(u) ?? failed(p) ?? 'unknown platform';
  throw new Error(`Could not detect the remote platform: ${why}`);
}
