/**
 * Raw HTML in transcript Markdown. Transcript text is untrusted (tool output fetched from the web,
 * files the user attached), and marked passes raw tags through to the renderer. Tags the chat knows
 * how to show are listed here; anything else (`<attached-file>`, `<string>`, `<script>`) is escaped
 * before parsing so it renders as literal text instead of becoming markup or vanishing.
 */

/** Tags allowed to pass through Markdown as raw HTML. The DOM sanitiser accepts the same set. */
export const RAW_HTML_TAGS: ReadonlySet<string> = new Set([
  'a', 'abbr', 'b', 'blockquote', 'br', 'caption', 'cite', 'code', 'dd', 'del', 'details', 'div', 'dl', 'dt', 'em',
  'figcaption', 'figure', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'hr', 'i', 'img', 'ins', 'kbd', 'li', 'mark', 'ol', 'p',
  'pre', 'q', 's', 'samp', 'small', 'span', 'strong', 'sub', 'summary', 'sup', 'table', 'tbody', 'td', 'tfoot', 'th',
  'thead', 'tr', 'u', 'ul', 'var',
]);

/**
 * A `<` that opens a tag whose name is not allowed, or a closing tag of one. The name must be followed
 * by whitespace, `/` or `>` so autolinks (`<https://…>`, `<me@x.io>`) and comparisons (`a <b`) are
 * left alone.
 */
const TAG_RE = /<(\/?)([A-Za-z][\w-]*)(?=[\s/>])/g;

const FENCE_RE = /^ {0,3}(`{3,}|~{3,})/;

/**
 * Escape the `<` of every raw tag not in RAW_HTML_TAGS so Markdown shows it as text. Fenced code
 * blocks and inline code spans are left as they are: marked escapes those itself. (Indented code
 * blocks are not detected; a stray tag there would show as `&lt;tag>`.)
 */
export function escapeUnknownTags(text: string): string {
  if (!text.includes('<')) return text;
  const lines = text.split('\n');
  const out: string[] = [];
  let fence: string | undefined;
  let openTicks = 0; // length of an inline code span's opening run still waiting for its close
  for (const line of lines) {
    if (fence) {
      out.push(line);
      const m = FENCE_RE.exec(line);
      if (m && m[1]!.charAt(0) === fence.charAt(0) && m[1]!.length >= fence.length && !line.slice(m[0].length).trim()) fence = undefined;
      continue;
    }
    const m = FENCE_RE.exec(line);
    if (m && openTicks === 0) {
      fence = m[1]!;
      out.push(line);
      continue;
    }
    out.push(escapeLine(line));
  }
  return out.join('\n');

  function escapeLine(line: string): string {
    let res = '';
    let i = 0;
    while (i < line.length) {
      const ch = line.charAt(i);
      if (ch === '`') {
        let j = i;
        while (line.charAt(j) === '`') j++;
        const run = j - i;
        if (openTicks === 0) openTicks = run;
        else if (openTicks === run) openTicks = 0;
        res += line.slice(i, j);
        i = j;
        continue;
      }
      if (ch === '<' && openTicks === 0) {
        TAG_RE.lastIndex = i;
        const t = TAG_RE.exec(line);
        if (t && t.index === i && !RAW_HTML_TAGS.has(t[2]!.toLowerCase())) {
          res += '&lt;';
          i++;
          continue;
        }
      }
      res += ch;
      i++;
    }
    return res;
  }
}
