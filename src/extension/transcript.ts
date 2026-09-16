/**
 * Renders a session transcript as Markdown in a read-only virtual document. The transcript tail is
 * fetched through the local daemon, which relays to the owning machine.
 */

import * as vscode from 'vscode';
import type { Agent } from '../core/model.ts';
import type { FleetService, MachineView } from './fleet.ts';
import { agentLabel, shortModel, tokens } from '../core/format.ts';

export const TRANSCRIPT_SCHEME = 'vineyard-transcript';

interface Block {
  type?: string;
  text?: string;
  name?: string;
  id?: string;
  input?: Record<string, unknown>;
  content?: unknown;
  is_error?: boolean;
  thinking?: string;
  tool_use_id?: string;
}

export function transcriptUri(machine: MachineView, agent: Agent): vscode.Uri {
  return vscode.Uri.from({
    scheme: TRANSCRIPT_SCHEME,
    authority: machine.id,
    path: `/${agent.sessionId}/${encodeURIComponent(agentLabel(agent)).slice(0, 80)}.md`,
    query: new URLSearchParams({ path: agent.transcriptPath ?? '', cwd: agent.workspacePath }).toString(),
  });
}

export class TranscriptProvider implements vscode.TextDocumentContentProvider {
  private readonly _onDidChange = new vscode.EventEmitter<vscode.Uri>();
  readonly onDidChange = this._onDidChange.event;

  constructor(private readonly fleet: FleetService) {}

  refresh(uri: vscode.Uri): void {
    this._onDidChange.fire(uri);
  }

  async provideTextDocumentContent(uri: vscode.Uri): Promise<string> {
    const machineId = uri.authority;
    const sessionId = uri.path.split('/')[1] ?? '';
    const params = new URLSearchParams(uri.query);
    const machine = this.fleet.machine(machineId);
    if (!machine) return `# Transcript unavailable\n\nMachine \`${machineId}\` is not in the fleet any more.`;
    if (!machine.online) return `# ${sessionId}\n\n${machine.name} is offline; transcripts are read live from the machine.`;

    const lines = vscode.workspace.getConfiguration('vineyard').get<number>('transcriptLines', 400);
    let data: { path: string; entries: Record<string, unknown>[] };
    try {
      data = await this.fleet.client.request('transcript', machineId, { sessionId, path: params.get('path') || undefined, cwd: params.get('cwd') || undefined, lines }, 30_000);
    } catch (err) {
      return `# ${sessionId}\n\nCould not fetch transcript from ${machine.name}: ${(err as Error).message}`;
    }
    const agent = this.fleet.findAgent(`${machineId}::${sessionId}`)?.agent;
    return renderTranscript(data.entries, { machine, agent, sessionId, path: data.path });
  }
}

export function summarizeToolInput(name: string, input: Record<string, unknown> | undefined): string | undefined {
  if (!input) return undefined;
  const first = (...keys: string[]) => {
    for (const k of keys) {
      const v = input[k];
      if (typeof v === 'string' && v.trim()) return v.trim();
    }
    return undefined;
  };
  let s: string | undefined;
  switch (name) {
    case 'Bash':
      s = first('description', 'command');
      break;
    case 'Read':
    case 'Write':
    case 'Edit':
      s = first('file_path');
      break;
    case 'Grep':
    case 'Glob':
      s = first('pattern');
      break;
    default:
      s = first('description', 'command', 'file_path', 'path', 'pattern', 'query', 'url', 'prompt');
  }
  if (!s) return undefined;
  s = s.replace(/\s+/g, ' ');
  return s.length > 120 ? s.slice(0, 117) + '…' : s;
}

export function renderTranscript(entries: Record<string, unknown>[], ctx: { machine: MachineView; agent: Agent | undefined; sessionId: string; path: string }): string {
  const out: string[] = [];
  const a = ctx.agent;
  out.push(`# ${a ? agentLabel(a) : ctx.sessionId}`, '');
  const meta: string[] = [`Machine: **${ctx.machine.name}**`];
  if (a) {
    meta.push(`State: **${a.state}**${a.stateDetail ? ` — ${a.stateDetail}` : ''}`);
    if (a.model) meta.push(`Model: ${shortModel(a.model)}${a.effort ? ` (${a.effort} effort)` : ''}`);
    if (a.contextTokens) meta.push(`Context: ${tokens(a.contextTokens)} tokens`);
    meta.push(`Workspace: \`${a.workspacePath}\``);
  }
  meta.push(`Transcript: \`${ctx.path}\``);
  out.push(meta.join('  \n'), '', '---', '');

  const toolNames = new Map<string, string>();
  for (const e of entries) {
    const type = e.type;
    const msg = e.message as { content?: unknown } | undefined;
    const time = typeof e.timestamp === 'string' ? new Date(e.timestamp).toLocaleTimeString() : '';
    const side = e.isSidechain === true ? ' (subagent)' : '';

    if (type === 'system') {
      const sub = typeof e.subtype === 'string' ? e.subtype : 'system';
      const content = typeof e.content === 'string' ? e.content : '';
      out.push(`> _${time} system/${sub}_ ${content}`, '');
      continue;
    }
    if (type !== 'user' && type !== 'assistant') continue;

    const blocks: Block[] = Array.isArray(msg?.content) ? (msg!.content as Block[]) : typeof msg?.content === 'string' ? [{ type: 'text', text: msg.content }] : [];
    for (const b of blocks) {
      switch (b.type) {
        case 'text': {
          if (!b.text?.trim()) break;
          out.push(type === 'user' ? `### 🧑 You${side} · ${time}` : `### 🤖 Claude${side} · ${time}`, '');
          out.push(type === 'user' ? b.text.trim().split('\n').map((l) => `> ${l}`).join('\n') : b.text.trim(), '');
          break;
        }
        case 'thinking':
          if (b.thinking?.trim()) out.push(`<details><summary>💭 thinking · ${time}</summary>\n\n${b.thinking.trim()}\n\n</details>`, '');
          break;
        case 'tool_use': {
          const name = b.name ?? 'tool';
          if (b.id) toolNames.set(b.id, name);
          const summary = summarizeToolInput(name, b.input);
          out.push(`🔧 **${name}**${summary ? ` — ${summary}` : ''} · _${time}_`);
          if (name === 'AskUserQuestion' && Array.isArray(b.input?.questions)) {
            for (const q of b.input!.questions as { question?: string; options?: { label?: string }[] }[]) {
              out.push('', `> ❓ ${q.question ?? ''}`);
              for (const o of q.options ?? []) out.push(`> - ${o.label ?? ''}`);
            }
          }
          out.push('');
          break;
        }
        case 'tool_result': {
          const name = toolNames.get(b.tool_use_id ?? '') ?? 'tool';
          const raw = typeof b.content === 'string' ? b.content : Array.isArray(b.content) ? (b.content as Block[]).map((c) => c.text ?? '').join('\n') : '';
          const text = raw.trim();
          if (!text) break;
          const clipped = text.length > 1500 ? text.slice(0, 1500) + `\n… (${text.length - 1500} more chars)` : text;
          out.push(`<details><summary>${b.is_error ? '❌' : '↩︎'} ${name} result</summary>\n\n\`\`\`\n${clipped.replace(/```/g, '~~~')}\n\`\`\`\n\n</details>`, '');
          break;
        }
        default:
          break;
      }
    }
  }
  return out.join('\n');
}
