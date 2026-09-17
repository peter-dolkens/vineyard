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
import { agentLabel, basename, relativeTime, shortModel, tildify } from '../core/format.ts';

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

  cmd('vineyard.openSettings', () => vscode.commands.executeCommand('workbench.action.openSettings', '@ext:peter-dolkens.vineyard'));
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('vineyard.sort') || e.affectsConfiguration('vineyard.showHistoricalWorkspaces') || e.affectsConfiguration('vineyard.showExitedAgents')) tree.refresh();
    }),
  );

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

  /**
   * Start (or resume) a managed session with no questions asked: default model and effort, the
   * configured permission mode, no first prompt. Everything is adjustable afterwards from the chat.
   */
  const spawnFlow = async (machine: MachineView, cwd: string, resume?: string) => {
    if (!machine.online) throw new Error(`${machine.name} is offline`);
    const cfg = vscode.workspace.getConfiguration('vineyard');
    const res = await fleet.client.request<{ sessionId: string }>(
      'spawn',
      machine.id,
      { cwd, permissionMode: cfg.get<string>('spawn.defaultPermissionMode', 'default'), resume, name: resume ? undefined : `vineyard-${basename(cwd)}` },
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

  /** Pick a workspace on a machine: a known one, or any path. Returns undefined when cancelled. */
  const workspaceOf = async (machine: MachineView, allowAll: boolean): Promise<string | undefined> => {
    const known = machine.entry.snapshot.workspaces.map((w) => w.path);
    const items: (vscode.QuickPickItem & { value: string })[] = known.map((p) => ({ label: basename(p), description: p, value: p }));
    if (allowAll) items.unshift({ label: '$(list-flat) All workspaces', description: 'every session on this machine', value: '*' });
    items.push({ label: '$(folder) Other path…', description: '', value: '' });
    const pick = await vscode.window.showQuickPick(items, { title: `Workspace on ${machine.name}`, ignoreFocusOut: true });
    if (!pick) return undefined;
    if (pick.value) return pick.value;
    return vscode.window.showInputBox({ title: 'Workspace path', prompt: `Absolute path on ${machine.name}`, ignoreFocusOut: true });
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
      cwd = await workspaceOf(machine, false);
    }
    if (!machine || !cwd) return;
    await spawnFlow(machine, cwd);
  });

  interface SessionSummary {
    sessionId: string;
    cwd: string;
    mtime: number;
    title?: string;
    firstPrompt?: string;
    lastPrompt?: string;
    model?: string;
    gitBranch?: string;
    turns?: number;
  }

  // Resume any past session on a machine from its transcripts on disk, not just live agents.
  cmd('vineyard.resumeFromHistory', async (node?: Node) => {
    const machine = node?.machine ?? (await machineOf(undefined));
    if (!machine) return;
    if (!machine.online) throw new Error(`${machine.name} is offline`);
    const cwd = node?.kind === 'workspace' || node?.kind === 'agent' ? pathOf(node) : await workspaceOf(machine, true);
    if (cwd === undefined) return;
    const scope = cwd === '*' ? undefined : cwd;
    const { sessions } = await fleet.client.request<{ sessions: SessionSummary[] }>('sessions', machine.id, { cwd: scope, limit: 80 }, 30_000);
    if (!sessions.length) {
      void vscode.window.showInformationMessage(`No past sessions found${scope ? ` in ${basename(scope)}` : ''} on ${machine.name}.`);
      return;
    }
    const live = new Set(machine.entry.snapshot.agents.filter((a) => a.alive).map((a) => a.sessionId));
    const items = sessions.map((s) => ({
      label: `${live.has(s.sessionId) ? '$(circle-large-filled) ' : ''}${s.title || s.firstPrompt || s.sessionId.slice(0, 8)}`,
      description: [relativeTime(s.mtime), s.model ? shortModel(s.model) : '', s.gitBranch, s.turns ? `${s.turns} turn${s.turns === 1 ? '' : 's'}` : ''].filter(Boolean).join(' · '),
      detail: `${scope ? '' : tildify(s.cwd, machine.entry.snapshot.host.home) + '  ·  '}${s.lastPrompt && s.lastPrompt !== s.firstPrompt ? s.lastPrompt : s.sessionId}`,
      s,
    }));
    const pick = await vscode.window.showQuickPick(items, { title: `Resume a session on ${machine.name}`, placeHolder: 'Newest first. Type to filter by title, prompt, branch or model.', matchOnDescription: true, matchOnDetail: true, ignoreFocusOut: true });
    if (!pick) return;
    if (live.has(pick.s.sessionId)) {
      const found = fleet.findAgent(`${machine.id}::${pick.s.sessionId}`);
      const ok = await vscode.window.showWarningMessage('That session is still running. Resuming it in a second process would have two writers on one transcript. Open its chat instead?', { modal: true }, 'Open chat', 'Resume anyway');
      if (!ok) return;
      if (ok === 'Open chat' && found) {
        chats.open(found.machine, found.agent);
        return;
      }
    }
    await spawnFlow(machine, pick.s.cwd, pick.s.sessionId);
  });

  cmd('vineyard.resumeManaged', async (node?: Node) => {
    const a = await agentOf(node);
    if (!a) return;
    if (a.agent.alive) {
      const ok = await vscode.window.showWarningMessage(`${agentLabel(a.agent)} is still running on ${a.machine.name}. Resuming it in a second process would have two writers on one transcript. Stop it there first, or continue anyway?`, { modal: true }, 'Continue anyway');
      if (!ok) return;
    }
    await spawnFlow(a.machine, a.agent.workspacePath, a.agent.sessionId);
  });

  // Ends any live session: managed ones through their control channel, others by terminating the
  // Claude Code process on that machine.
  cmd('vineyard.stopAgent', async (node?: Node) => {
    const a = await agentOf(node);
    if (!a || !a.agent.alive) return;
    const managed = !!a.agent.managed && !a.agent.managed.exited;
    const ok = await vscode.window.showWarningMessage(
      `${managed ? 'Stop' : 'Terminate'} ${agentLabel(a.agent)} on ${a.machine.name}?`,
      { modal: true, detail: managed ? 'The session ends cleanly; you can resume it later.' : 'The Claude Code process is sent SIGTERM (killed after 5 s if it ignores it). Its transcript stays on disk and can be resumed.' },
      managed ? 'Stop' : 'Terminate',
    );
    if (ok) await fleet.client.request(managed ? 'stop' : 'kill', a.machine.id, { sessionId: a.agent.sessionId }, 15_000);
  });

  // Sign a machine in to Claude from here: the daemon there runs `claude auth login`, we open its URL
  // in this browser and relay the code it shows back.
  cmd('vineyard.login', async (node?: Node) => {
    const machine = node?.machine ?? (await machineOf(undefined));
    if (!machine) return;
    if (!machine.online) throw new Error(`${machine.name} is offline`);
    const kind = await vscode.window.showQuickPick(
      [
        { label: 'Claude subscription', description: 'claude.ai account (Pro / Max / Team)', console: false },
        { label: 'Anthropic Console', description: 'API usage billing', console: true },
      ],
      { title: `Sign in to Claude on ${machine.name}`, ignoreFocusOut: true },
    );
    if (!kind) return;
    const start = await fleet.client.request<{ id: string; url: string }>('login', machine.id, { action: 'start', console: kind.console }, 60_000);
    await vscode.env.openExternal(vscode.Uri.parse(start.url));
    const code = await vscode.window.showInputBox({
      title: `Sign in on ${machine.name}`,
      prompt: 'Finish signing in in the browser that just opened, then paste the code it shows you here.',
      placeHolder: 'authorization code',
      ignoreFocusOut: true,
    });
    if (!code?.trim()) {
      await fleet.client.request('login', machine.id, { action: 'cancel', id: start.id }, 10_000).catch(() => undefined);
      return;
    }
    const res = await fleet.client.request<{ ok: boolean; message: string }>('login', machine.id, { action: 'code', id: start.id, code: code.trim() }, 150_000);
    void vscode.window.showInformationMessage(`Claude on ${machine.name}: ${res.message}`);
    fleet.refreshAll();
  });

  cmd('vineyard.renameSession', async (node?: Node) => {
    const a = await agentOf(node);
    if (!a) return;
    const title = await vscode.window.showInputBox({ title: `Rename session on ${a.machine.name}`, value: a.agent.title || a.agent.name || '', prompt: 'New session title', ignoreFocusOut: true });
    if (!title?.trim()) return;
    await fleet.client.request('rename', a.machine.id, { sessionId: a.agent.sessionId, title: title.trim(), path: a.agent.transcriptPath || undefined, cwd: a.agent.workspacePath }, 20_000);
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
