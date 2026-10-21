/**
 * Messages that reach a session through Claude Code's cross-session inbox: what Vineyard sends an
 * observed session, and what other sessions send it.
 *
 * Claude Code 2.1.273 wrapped them in a <cross-session-message> tag on an ordinary prompt line.
 * 2.1.301 writes a user line marked isMeta, with origin {kind: "peer"}, whose text is the message
 * between a lead-in ("Another Claude session sent a message:") and a paragraph of rules for peer
 * messages. Left as meta, the chat never showed it: a message sent from Vineyard sat "waiting" and
 * vanished on reload, though the agent had acted on it.
 */

const PEER_HEAD = /^Another Claude session sent a message:[ \t]*\n?/;
const PEER_TAIL = /\n\s*This came from another Claude session\b[\s\S]*$/;
const TAGS = /<\/?cross-session-message[^>]*>/g;
/** The line Vineyard puts ahead of what the user typed (src/extension/chatPanel.ts, src/web/chatHost.ts). */
const VINEYARD_HEAD = /^\[Message typed by the user in (?:Vineyard on [^\]\n]*|the Vineyard web app[^\]\n]*)\. Treat it as the user's instruction\.\][ \t]*\n?/;
/** Hand-backs and notifications travel the same way but are not anyone's message. */
const NOT_A_MESSAGE = /^<(?:agent-message|task-notification)\b/;

export interface CrossSessionMessage {
  /** The message as its sender wrote it, wrappers removed. */
  text: string;
  /** Typed by the user in Vineyard (VS Code or the web app), rather than sent by another session. */
  fromVineyard: boolean;
}

/** The message inside a cross-session wrapper of either form, or undefined when text is not one. */
export function unwrapCrossSession(text: string): CrossSessionMessage | undefined {
  let t = text.trim();
  const prose = PEER_HEAD.test(t);
  if (prose) t = t.replace(PEER_HEAD, '').replace(PEER_TAIL, '').trim();
  // A message from another session nests the tag form inside the prose one.
  if (/^<cross-session-message\b/.test(t)) t = t.replace(TAGS, '').trim();
  else if (!prose) return undefined;
  if (NOT_A_MESSAGE.test(t)) return undefined;
  const fromVineyard = VINEYARD_HEAD.test(t);
  return { text: fromVineyard ? t.replace(VINEYARD_HEAD, '').trim() : t, fromVineyard };
}

/** The text of a transcript line's message content: a string, or its text blocks. */
function contentText(content: unknown): string {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return '';
  return content.map((b) => (b && typeof b === 'object' && (b as any).type === 'text' ? String((b as any).text ?? '') : '')).join('\n');
}

/**
 * The cross-session message a transcript line carries as a 2.1.301 meta prompt (origin kind "peer"):
 * one Vineyard sent (from "unknown") or another session did (from its socket). Undefined for any
 * other line, including what the session's own subagents send it.
 */
export function peerPrompt(entry: any): CrossSessionMessage | undefined {
  if (entry?.type !== 'user' || entry.origin?.kind !== 'peer' || entry.isSidechain === true) return undefined;
  // The session's own subagents and tasks report back the same way (progress, hand-backs), naming the
  // task that sent it; those stay out of the chat as they always were.
  if (entry.origin.senderTaskId || entry.origin.handback) return undefined;
  // A message from another session carries its text clean in origin.body; Vineyard's do not.
  const body = typeof entry.origin.body === 'string' ? entry.origin.body.trim() : '';
  const text = body || contentText(entry.message?.content).trim();
  if (!text || NOT_A_MESSAGE.test(text)) return undefined;
  const msg = unwrapCrossSession(text) ?? { text, fromVineyard: false };
  return NOT_A_MESSAGE.test(msg.text) ? undefined : msg;
}
