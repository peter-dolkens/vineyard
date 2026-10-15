/**
 * Telling Claude's reasoning apart from its narration, for transcript rendering.
 *
 * With summarised thinking a response often ends with a second thinking block that is not reasoning
 * at all: one line saying what the model is about to do ("Now I'll write the parser based on this
 * structure"), written in no measurable time after the reasoning that took seconds. Claude Code
 * prints that line as ordinary output and keeps only the reasoning behind a "Thinking" disclosure;
 * folding both away hid the only running commentary some turns have.
 */

/**
 * Whether a thinking block is narration. `apiBlockIndex` is the block's position in the API
 * response, so > 0 means a block that follows the reasoning; `thinkingDurationMs` separates a
 * second genuine reasoning block from a line that cost nothing to write. Older transcripts carry
 * the index but not the duration, and there the index alone has matched every case seen.
 */
export function isNarration(entry: Record<string, unknown>): boolean {
  return Number(entry.apiBlockIndex ?? 0) > 0 && Number(entry.thinkingDurationMs ?? 0) < 1000;
}
