import * as vscode from 'vscode';
import type { FleetService } from './fleet.ts';

export class FleetStatusBar implements vscode.Disposable {
  private readonly item: vscode.StatusBarItem;
  private readonly sub: vscode.Disposable;

  constructor(private readonly fleet: FleetService) {
    this.item = vscode.window.createStatusBarItem('vineyard.summary', vscode.StatusBarAlignment.Left, 50);
    this.item.name = 'Vineyard';
    this.item.command = 'vineyard.focus';
    this.sub = fleet.onDidChange(() => this.update());
    this.update();
  }

  update(): void {
    const enabled = vscode.workspace.getConfiguration('vineyard').get<boolean>('statusBar.enabled', true);
    if (!enabled) {
      this.item.hide();
      return;
    }
    const s = this.fleet.summary();
    const parts: string[] = [];
    if (s.attention) parts.push(`$(question) ${s.attention}`);
    if (s.busy) parts.push(`$(loading~spin) ${s.busy}`);
    if (s.idle) parts.push(`$(circle-large-filled) ${s.idle}`);
    if (!parts.length) parts.push('$(hubot) 0');
    this.item.text = parts.join('  ');
    this.item.backgroundColor = s.attention ? new vscode.ThemeColor('statusBarItem.warningBackground') : undefined;
    const md = new vscode.MarkdownString('', true);
    md.appendMarkdown(`**Vineyard**  \n${s.machinesOnline}/${s.machinesTotal} machines online  \n`);
    md.appendMarkdown(`${s.agentsLive} live agent${s.agentsLive === 1 ? '' : 's'}`);
    if (s.attention) md.appendMarkdown(`  \n$(question) ${s.attention} waiting on you`);
    if (s.busy) md.appendMarkdown(`  \n$(loading~spin) ${s.busy} working`);
    if (s.idle) md.appendMarkdown(`  \n$(circle-large-filled) ${s.idle} idle`);
    this.item.tooltip = md;
    this.item.show();
  }

  dispose(): void {
    this.item.dispose();
    this.sub.dispose();
  }
}
