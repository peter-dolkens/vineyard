import * as vscode from 'vscode';
import { DaemonClient, type DaemonConnState } from './daemonClient.ts';
import { FleetService, type MachineView } from './fleet.ts';
import type { Agent } from '../core/model.ts';
import { FleetTree, type AgentNode, type Node } from './tree.ts';
import { FleetStatusBar } from './statusbar.ts';
import { Notifier } from './notify.ts';
import { TRANSCRIPT_SCHEME, TranscriptProvider, transcriptUri } from './transcript.ts';
import { Setup } from './setup.ts';
import { ChatPanels } from './chatPanel.ts';
import { Updater } from './updater.ts';
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
  const chats = new ChatPanels(context, fleet, log);
  const updater = new Updater(context, client, fleet, setup, log);

  context.subscriptions.push(log, client, fleet, view, statusBar, notifier, chats, updater, vscode.workspace.registerTextDocumentContentProvider(TRANSCRIPT_SCHEME, transcripts));

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
    if (m) await updater.updateMachine(m, { auto: false, force: true });
  });
  cmd('vineyard.updateAllDaemons', () => updater.updateAll());
  cmd('vineyard.checkForUpdates', () => updater.checkExtensionUpdate(true));
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
    if (a) chats.open(a.machine, a.agent);
  });

  cmd('vineyard.showRawTranscript', async (node?: Node) => {
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

  // ---- managed sessions ---------------------------------------------------------------------

  const spawnFlow = async (machine: MachineView, cwd: string, resume?: Agent) => {
    if (!machine.online) throw new Error(`${machine.name} is offline`);
    const prompt = await vscode.window.showInputBox({
      title: resume ? `Resume ${agentLabel(resume)} under Vineyard control` : `New agent in ${basename(cwd)} on ${machine.name}`,
      prompt: resume ? 'Optional first message for the resumed session' : 'What should the agent do? (leave empty to start it idle)',
      ignoreFocusOut: true,
    });
    if (prompt === undefined) return;
    const cfg = vscode.workspace.getConfiguration('vineyard');
    const modelPick = await vscode.window.showQuickPick(
      [
        { label: 'Default model', description: 'whatever Claude Code is configured to use', value: '' },
        { label: 'Fable 5.1', value: 'claude-fable-5-1' },
        { label: 'Opus 5', value: 'claude-opus-5' },
        { label: 'Sonnet 5', value: 'claude-sonnet-5' },
        { label: 'Haiku 4.5', value: 'claude-haiku-4-5-20251001' },
      ],
      { title: 'Model', placeHolder: 'Model for this agent', ignoreFocusOut: true },
    );
    if (!modelPick) return;
    const effortPick = await vscode.window.showQuickPick(
      [
        { label: 'Default effort', description: 'whatever Claude Code is configured to use', value: '' },
        { label: 'low', value: 'low' },
        { label: 'medium', value: 'medium' },
        { label: 'high', value: 'high' },
        { label: 'xhigh', value: 'xhigh' },
        { label: 'max', value: 'max' },
      ],
      { title: 'Reasoning effort', placeHolder: 'Effort for this agent (changeable later from the chat)', ignoreFocusOut: true },
    );
    if (!effortPick) return;
    const modePick = await vscode.window.showQuickPick(
      [
        { label: 'default', description: 'ask before edits and commands (you approve here in Vineyard)', value: 'default' },
        { label: 'acceptEdits', description: 'auto-accept file edits, ask for commands', value: 'acceptEdits' },
        { label: 'plan', description: 'read-only planning until you approve the plan', value: 'plan' },
        { label: 'auto', description: 'Claude Code auto mode', value: 'auto' },
        { label: 'bypassPermissions', description: 'no prompts at all — use with care', value: 'bypassPermissions' },
      ],
      { title: 'Permission mode', placeHolder: cfg.get<string>('spawn.defaultPermissionMode', 'default'), ignoreFocusOut: true },
    );
    if (!modePick) return;
    const res = await fleet.client.request<{ sessionId: string }>(
      'spawn',
      machine.id,
      { cwd, prompt, model: modelPick.value || undefined, effort: effortPick.value || undefined, permissionMode: modePick.value, resume: resume?.sessionId, name: resume ? undefined : `vineyard-${basename(cwd)}` },
      30_000,
    );
    // Wait briefly for the agent to appear in the fleet, then open its chat.
    const id = `${machine.id}::${res.sessionId}`;
    const deadline = Date.now() + 15_000;
    const tryOpen = () => {
      const found = fleet.findAgent(id);
      if (found) {
        chats.open(found.machine, found.agent);
        return;
      }
      if (Date.now() < deadline) setTimeout(tryOpen, 400);
      else void vscode.window.showWarningMessage('Agent started but has not reported yet; it will appear in the tree shortly.');
    };
    tryOpen();
  };

  cmd('vineyard.newAgent', async (node?: Node) => {
    let machine: MachineView | undefined;
    let cwd: string | undefined;
    if (node?.kind === 'workspace' || node?.kind === 'agent') {
      machine = node.machine;
      cwd = pathOf(node);
    } else {
      machine = await machineOf(node);
      if (!machine) return;
      const known = machine.entry.snapshot.workspaces.map((w) => w.path);
      const pick = await vscode.window.showQuickPick([...known.map((p) => ({ label: basename(p), description: p, value: p })), { label: '$(folder) Other path…', description: '', value: '' }], { title: `Workspace on ${machine.name}`, ignoreFocusOut: true });
      if (!pick) return;
      cwd = pick.value || (await vscode.window.showInputBox({ title: 'Workspace path', prompt: `Absolute path on ${machine.name}`, ignoreFocusOut: true }));
    }
    if (!machine || !cwd) return;
    await spawnFlow(machine, cwd);
  });

  cmd('vineyard.resumeManaged', async (node?: Node) => {
    const a = await agentOf(node);
    if (!a) return;
    if (a.agent.alive) {
      const ok = await vscode.window.showWarningMessage(`${agentLabel(a.agent)} is still running on ${a.machine.name}. Resuming it in a second process would have two writers on one transcript. Stop it there first, or continue anyway?`, { modal: true }, 'Continue anyway');
      if (!ok) return;
    }
    await spawnFlow(a.machine, a.agent.workspacePath, a.agent);
  });

  cmd('vineyard.stopAgent', async (node?: Node) => {
    const a = await agentOf(node);
    if (!a?.agent.managed || a.agent.managed.exited) {
      void vscode.window.showInformationMessage('Only sessions started by Vineyard can be stopped from here.');
      return;
    }
    const ok = await vscode.window.showWarningMessage(`Stop ${agentLabel(a.agent)} on ${a.machine.name}?`, { modal: true }, 'Stop');
    if (ok) await fleet.client.request('stop', a.machine.id, { sessionId: a.agent.sessionId }, 10_000);
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
  updater.start();
}

export function deactivate(): void {}

function shellQuote(s: string): string {
  if (/^[A-Za-z0-9_\-./~]+$/.test(s)) return s;
  return `'${s.replace(/'/g, `'\\''`)}'`;
}
