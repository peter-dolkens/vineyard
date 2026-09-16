import * as vscode from 'vscode';
import { DaemonClient, type DaemonConnState } from './daemonClient.ts';
import { FleetService, type MachineView } from './fleet.ts';
import { FleetTree, type AgentNode, type Node } from './tree.ts';
import { FleetStatusBar } from './statusbar.ts';
import { Notifier } from './notify.ts';
import { TRANSCRIPT_SCHEME, TranscriptProvider, transcriptUri } from './transcript.ts';
import { Setup } from './setup.ts';
import { agentLabel, basename } from '../core/format.ts';

export function activate(context: vscode.ExtensionContext): void {
  const log = vscode.window.createOutputChannel('Vineyard');
  const client = new DaemonClient(log);
  const fleet = new FleetService(client);
  const tree = new FleetTree(fleet);
  const view = vscode.window.createTreeView<Node>('vineyard.fleet', { treeDataProvider: tree, showCollapseAll: true });
  const statusBar = new FleetStatusBar(fleet);
  const notifier = new Notifier(fleet);
  const transcripts = new TranscriptProvider(fleet);
  const setup = new Setup(context, client, fleet, log);

  context.subscriptions.push(log, client, fleet, view, statusBar, notifier, vscode.workspace.registerTextDocumentContentProvider(TRANSCRIPT_SCHEME, transcripts));

  const describe = (state: DaemonConnState) => {
    const s = fleet.summary();
    switch (state) {
      case 'connected':
        view.description = `${s.machinesOnline}/${s.machinesTotal} online`;
        break;
      case 'connecting':
        view.description = 'connecting to daemon…';
        break;
      case 'disconnected':
        view.description = 'daemon unreachable';
        break;
      case 'no-config':
        view.description = 'not set up';
        break;
    }
  };
  context.subscriptions.push(client.onStateChange(describe), fleet.onDidChange(() => describe(client.state)));

  const cmd = (name: string, fn: (...args: any[]) => unknown | Promise<unknown>) =>
    context.subscriptions.push(
      vscode.commands.registerCommand(name, async (...args: unknown[]) => {
        try {
          await fn(...args);
        } catch (err) {
          const message = err instanceof Error ? err.message : String(err);
          log.appendLine(`[${name}] ${message}`);
          void vscode.window.showErrorMessage(`Vineyard: ${message}`, 'Show Log').then((c) => c && log.show());
        }
      }),
    );

  const machineOf = async (node: Node | undefined): Promise<MachineView | undefined> => {
    if (node) return node.machine;
    const picks = fleet.machines().map((m) => ({ label: m.name, description: m.online ? 'online' : 'offline', m }));
    const pick = await vscode.window.showQuickPick(picks, { placeHolder: 'Machine' });
    return pick?.m;
  };

  const agentOf = async (node: Node | undefined): Promise<AgentNode | undefined> => {
    if (node?.kind === 'agent') return node;
    const items: (vscode.QuickPickItem & { node: AgentNode })[] = [];
    for (const machine of fleet.machines()) {
      for (const workspace of machine.entry.snapshot.workspaces) {
        for (const agent of workspace.agents) {
          if (!agent.alive && !tree.showExited) continue;
          items.push({ label: agentLabel(agent), description: `${agent.state} · ${machine.name} · ${basename(workspace.path)}`, node: { kind: 'agent', machine, workspace, agent } });
        }
      }
    }
    const pick = await vscode.window.showQuickPick(items, { placeHolder: 'Agent', matchOnDescription: true });
    return pick?.node;
  };

  cmd('vineyard.refresh', () => {
    fleet.refreshAll();
    tree.refresh();
  });
  cmd('vineyard.focus', () => vscode.commands.executeCommand('vineyard.fleet.focus'));
  cmd('vineyard.setupLocal', () => setup.setupLocal());
  cmd('vineyard.addMachine', () => setup.addMachine());
  cmd('vineyard.updateMachine', async (node?: Node) => {
    const m = await machineOf(node);
    if (m) await setup.updateMachine(m);
  });
  cmd('vineyard.removeMachine', async (node?: Node) => {
    const m = await machineOf(node);
    if (m) await setup.removeMachine(m);
  });
  cmd('vineyard.restartDaemon', async (node?: Node) => {
    const m = await machineOf(node);
    if (m) await setup.restartDaemon(m);
  });
  cmd('vineyard.showDaemonLog', async (node?: Node) => {
    const m = await machineOf(node);
    if (m) setup.showDaemonLog(m);
  });
  cmd('vineyard.showOutput', () => log.show());
  cmd('vineyard.createInvite', () => setup.createInvite());
  cmd('vineyard.joinWithCode', (code?: unknown) => setup.joinWithCode(typeof code === 'string' ? code : undefined));
  context.subscriptions.push(
    vscode.window.registerUriHandler({
      handleUri(uri) {
        if (uri.path === '/join') {
          const code = new URLSearchParams(uri.query).get('code');
          if (code) void vscode.commands.executeCommand('vineyard.joinWithCode', code);
        }
      },
    }),
  );
  cmd('vineyard.reconnect', () => client.reconnectNow());

  cmd('vineyard.toggleHistorical', () => {
    tree.showHistorical = !tree.showHistorical;
    tree.refresh();
  });
  cmd('vineyard.toggleExited', () => {
    tree.showExited = !tree.showExited;
    tree.refresh();
  });

  const pathOf = (node: Node): string | undefined => (node.kind === 'workspace' ? node.workspace.path : node.kind === 'agent' ? node.agent.workspacePath : undefined);

  cmd('vineyard.openWorkspace', async (node?: Node) => {
    if (!node) return;
    const p = pathOf(node);
    if (!p) return;
    if (node.machine.local) {
      await vscode.commands.executeCommand('vscode.openFolder', vscode.Uri.file(p), { forceNewWindow: true });
      return;
    }
    const remotePath = /^[A-Za-z]:[\\/]/.test(p) ? '/' + p.replace(/\\/g, '/') : p;
    const uri = vscode.Uri.parse(`vscode-remote://ssh-remote+${node.machine.host}${remotePath}`);
    await vscode.commands.executeCommand('vscode.openFolder', uri, { forceNewWindow: true });
  });

  const terminalFor = (node: Node, command?: string): vscode.Terminal => {
    const p = pathOf(node);
    const name = `${node.machine.name}${p ? `: ${basename(p)}` : ''}`;
    if (node.machine.local) {
      const t = vscode.window.createTerminal({ name, cwd: p });
      if (command) t.sendText(command);
      return t;
    }
    const t = vscode.window.createTerminal({ name });
    const parts: string[] = [];
    if (p) parts.push(`cd ${shellQuote(p)}`);
    parts.push(command ?? 'exec "${SHELL:-/bin/sh}" -l');
    t.sendText(`ssh -t ${node.machine.host} ${shellQuote(parts.join(' && '))}`);
    return t;
  };

  cmd('vineyard.openTerminal', (node?: Node) => {
    if (node) terminalFor(node).show();
  });

  cmd('vineyard.resumeSession', async (node?: Node) => {
    const a = await agentOf(node);
    if (!a) return;
    if (a.agent.alive) {
      const ok = await vscode.window.showWarningMessage(`${agentLabel(a.agent)} is still running on ${a.machine.name}. Resuming it in a second place can confuse the session. Continue?`, { modal: true }, 'Resume anyway');
      if (!ok) return;
    }
    terminalFor(a, `claude --resume ${a.agent.sessionId}`).show();
  });

  cmd('vineyard.showTranscript', async (node?: Node) => {
    const a = await agentOf(node);
    if (!a) return;
    const uri = transcriptUri(a.machine, a.agent);
    transcripts.refresh(uri);
    const doc = await vscode.workspace.openTextDocument(uri);
    try {
      await vscode.commands.executeCommand('markdown.showPreview', uri);
    } catch {
      await vscode.window.showTextDocument(doc, { preview: true });
    }
  });

  cmd('vineyard.copySessionId', async (node?: Node) => {
    const a = await agentOf(node);
    if (a) await vscode.env.clipboard.writeText(a.agent.sessionId);
  });
  cmd('vineyard.copyPath', async (node?: Node) => {
    const p = node ? pathOf(node) : undefined;
    if (p) await vscode.env.clipboard.writeText(p);
  });

  void vscode.commands.executeCommand('setContext', 'vineyard.daemonState', client.state);
  client.start();
}

export function deactivate(): void {}

function shellQuote(s: string): string {
  if (/^[A-Za-z0-9_\-./~]+$/.test(s)) return s;
  return `'${s.replace(/'/g, `'\\''`)}'`;
}
