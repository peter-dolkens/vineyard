/**
 * Switch Claude Account on Machine… (experimental, vineyard.experimental.claudeAccounts): pick which
 * of the accounts a machine keeps signed in its Claude uses, add one more, or forget one. The machine's
 * daemon does the switching; only labels come back here.
 */

import * as vscode from 'vscode';
import type { DaemonClient } from './daemonClient.ts';
import type { MachineView } from './fleet.ts';
import { ACCOUNT_TYPES, accountDetail, accountExpired, accountPlan, accountTitle, type AccountsReply, type ClaudeAccount } from '../core/accounts.ts';

export const ACCOUNTS_SETTING = 'experimental.claudeAccounts';

export function accountsEnabled(): boolean {
  return vscode.workspace.getConfiguration('vineyard').get<boolean>(ACCOUNTS_SETTING, false);
}

type Item = vscode.QuickPickItem & { account?: ClaudeAccount; add?: true };

const REMOVE: vscode.QuickInputButton = { iconPath: new vscode.ThemeIcon('trash'), tooltip: 'Forget this account on this machine' };

export async function switchAccount(client: DaemonClient, machine: MachineView): Promise<void> {
  if (!accountsEnabled()) {
    const open = await vscode.window.showInformationMessage('Switching Claude accounts is experimental. Turn on vineyard.experimental.claudeAccounts to use it.', 'Open Setting');
    if (open) await vscode.commands.executeCommand('workbench.action.openSettings', `vineyard.${ACCOUNTS_SETTING}`);
    return;
  }
  if (!machine.online) throw new Error(`${machine.name} is offline`);
  const request = <T>(args: object, timeout = 30_000) => client.request<T>('accounts', machine.id, args, timeout);

  const qp = vscode.window.createQuickPick<Item>();
  qp.title = `Claude account on ${machine.name}`;
  qp.placeholder = 'Account to sign this machine in as';
  qp.ignoreFocusOut = true;
  const show = (accounts: ClaudeAccount[]) => {
    const items: Item[] = accounts.map((a) => ({
      label: `${a.active ? '$(check)' : accountExpired(a) ? '$(warning)' : '$(account)'} ${accountTitle(a)}`,
      description: a.active ? 'signed in now' : accountPlan(a),
      detail: accountDetail(a) || undefined,
      buttons: a.active ? [] : [REMOVE],
      account: a,
    }));
    if (items.length) items.push({ label: '', kind: vscode.QuickPickItemKind.Separator });
    items.push({ label: '$(add) Add Account…', description: 'sign in to one more account here', add: true });
    qp.items = items;
  };
  qp.busy = true;
  qp.show();
  try {
    show((await request<AccountsReply>({ action: 'list' })).accounts);
  } catch (err) {
    qp.dispose();
    throw err;
  }
  qp.busy = false;

  const picked = await new Promise<Item | undefined>((resolve) => {
    qp.onDidAccept(() => resolve(qp.selectedItems[0]));
    qp.onDidHide(() => resolve(undefined));
    qp.onDidTriggerItemButton(async ({ item }) => {
      const a = item.account;
      if (!a) return;
      const ok = await vscode.window.showWarningMessage(
        `Forget ${accountTitle(a)} on ${machine.name}?`,
        { modal: true, detail: 'Its saved sign-in is deleted from that machine. Switching back to it later means signing in again. Nothing is signed out on claude.ai.' },
        'Forget',
      );
      if (!ok) return;
      qp.busy = true;
      try {
        show((await request<AccountsReply>({ action: 'remove', key: a.key })).accounts);
      } catch (err) {
        void vscode.window.showErrorMessage(`Vineyard: ${err instanceof Error ? err.message : String(err)}`);
      } finally {
        qp.busy = false;
      }
    });
  });
  qp.dispose();
  if (!picked) return;
  if (picked.add) return addAccount(client, machine);
  const a = picked.account;
  if (!a || a.active) return;
  if (accountExpired(a)) {
    const again = await vscode.window.showWarningMessage(`The sign-in saved for ${accountTitle(a)} on ${machine.name} has expired.`, 'Sign In Again');
    if (again) await addAccount(client, machine);
    return;
  }
  await request<AccountsReply>({ action: 'switch', key: a.key });
  void vscode.window.showInformationMessage(`Claude on ${machine.name} is now signed in as ${accountTitle(a)}. Running sessions switch on their next turn.`);
}

/** Sign in to one more account: choose its type, then the usual relayed `claude auth login`. */
export async function addAccount(client: DaemonClient, machine: MachineView): Promise<void> {
  const kind = await vscode.window.showQuickPick(
    ACCOUNT_TYPES.map((t) => ({ label: t.label, description: t.detail, type: t.type })),
    { title: `Add a Claude account on ${machine.name}`, placeHolder: 'Account type', ignoreFocusOut: true },
  );
  if (!kind) return;
  const request = <T>(args: object, timeout = 30_000) => client.request<T>('accounts', machine.id, { action: 'add', type: kind.type, ...args }, timeout);
  const start = await request<{ id: string; url: string }>({ step: 'start' }, 60_000);
  await vscode.env.openExternal(vscode.Uri.parse(start.url));
  const code = await vscode.window.showInputBox({
    title: `Sign in on ${machine.name}`,
    prompt: 'Sign in as the account to add in the browser that just opened, then paste the code it shows you here.',
    placeHolder: 'authorization code',
    ignoreFocusOut: true,
  });
  if (!code?.trim()) {
    await request({ step: 'cancel', id: start.id }, 10_000).catch(() => undefined);
    return;
  }
  const res = await request<AccountsReply & { message: string }>({ step: 'code', id: start.id, code: code.trim() }, 150_000);
  const now = res.accounts.find((a) => a.active);
  void vscode.window.showInformationMessage(now ? `Claude on ${machine.name} is now signed in as ${accountTitle(now)}; the previous account stays saved.` : `Claude on ${machine.name}: ${res.message}`);
}
