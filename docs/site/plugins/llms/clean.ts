// Copyright 2026 The pgToolBox Authors.
// SPDX-License-Identifier: Apache-2.0
//
// Turns a documentation source (.md or .mdx) into plain Markdown for a reader
// that is not a browser: front matter removed, MDX imports, exports and
// comments dropped, JSX components rendered to the text they carry (an image
// becomes its alt text), admonitions flattened to a bold label, relative
// links to other pages rewritten. Fenced code, Mermaid included, is never
// touched. No dependency: the transformations are line-based on purpose.

export interface FrontMatter {
  [key: string]: string;
}

/** Splits a leading `---` block into flat key/value pairs and the body. */
export function splitFrontMatter(source: string): {data: FrontMatter; body: string} {
  const match = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/.exec(source);
  if (!match) return {data: {}, body: source};
  const data: FrontMatter = {};
  for (const line of match[1].split(/\r?\n/)) {
    const kv = /^([A-Za-z_][\w-]*):\s*(.*)$/.exec(line);
    if (kv) data[kv[1]] = kv[2].replace(/^(['"])(.*)\1$/, '$2');
  }
  return {data, body: source.slice(match[0].length)};
}

interface Segment {
  fence: boolean;
  text: string;
}

/** Splits a body into fenced-code and prose segments, fences kept verbatim. */
function segments(body: string): Segment[] {
  const out: Segment[] = [];
  let prose: string[] = [];
  let fence: string[] = [];
  let marker = '';
  for (const line of body.split('\n')) {
    if (!marker) {
      const open = /^\s*(`{3,}|~{3,})/.exec(line);
      if (open) {
        if (prose.length) out.push({fence: false, text: prose.join('\n')});
        prose = [];
        marker = open[1];
        fence = [line];
      } else prose.push(line);
      continue;
    }
    fence.push(line);
    const close = /^\s*(`{3,}|~{3,})\s*$/.exec(line);
    if (close && close[1][0] === marker[0] && close[1].length >= marker.length) {
      out.push({fence: true, text: fence.join('\n')});
      marker = '';
      fence = [];
    }
  }
  if (marker) out.push({fence: true, text: fence.join('\n')});
  else if (prose.length) out.push({fence: false, text: prose.join('\n')});
  return out;
}

/** The index just after the `>` closing the JSX opening tag that starts at `start`. */
function openingTagEnd(src: string, start: number): number {
  let depth = 0;
  let quote = '';
  for (let i = start; i < src.length; i++) {
    const c = src[i];
    if (quote) {
      if (c === quote && src[i - 1] !== '\\') quote = '';
    } else if (c === '"' || c === "'" || (c === '`' && depth > 0)) quote = c;
    else if (c === '{') depth++;
    else if (c === '}') depth--;
    else if (c === '>' && depth === 0) return i + 1;
  }
  return -1;
}

function attribute(tag: string, name: string): string | undefined {
  const m = new RegExp(`\\b${name}=(?:"([^"]*)"|'([^']*)'|\\{\`([^\`]*)\`\\})`).exec(tag);
  return m ? (m[1] ?? m[2] ?? m[3]) : undefined;
}

/**
 * Renders the JSX elements that start a line to text: the alt text of an
 * image, otherwise the element's content with its own tags removed.
 */
function renderJsx(src: string): string {
  const lines = src.split('\n');
  const starts: number[] = [];
  let offset = 0;
  for (const l of lines) {
    starts.push(offset);
    offset += l.length + 1;
  }
  const out: string[] = [];
  let i = 0;
  while (i < lines.length) {
    const m = /^(\s*)<([A-Za-z][\w.]*)(?=[\s/>]|$)/.exec(lines[i]);
    const tagStart = m ? starts[i] + m[1].length : -1;
    const tagEnd = m ? openingTagEnd(src, tagStart) : -1;
    if (!m || tagEnd < 0) {
      out.push(lines[i]);
      i++;
      continue;
    }
    const opening = src.slice(tagStart, tagEnd);
    let end = tagEnd;
    let inner = '';
    if (!opening.endsWith('/>')) {
      const re = new RegExp(`<(/?)${m[2].replace('.', '\\.')}(?=[\\s/>]|$)`, 'g');
      re.lastIndex = tagEnd;
      let depth = 1;
      for (let t = re.exec(src); t; t = re.exec(src)) {
        if (t[1]) depth--;
        else if (!src.slice(t.index, openingTagEnd(src, t.index)).endsWith('/>')) depth++;
        if (depth === 0) {
          inner = src.slice(tagEnd, t.index);
          end = src.indexOf('>', t.index) + 1;
          break;
        }
      }
    }
    const alt = attribute(opening, 'alt');
    const label = attribute(opening, 'aria-label');
    let text = '';
    if (alt !== undefined) text = `*[Image: ${alt}]*`;
    else if (inner.trim()) text = renderJsx(inner.replace(/^\n+|\n+$/g, '')).trim();
    else if (label) text = `*[${label}]*`;
    out.push(m[1] + text);
    // Resume after the last source line the element covered.
    let j = i;
    while (j < lines.length && starts[j] + lines[j].length < end) j++;
    i = j + 1;
  }
  return out.join('\n');
}

const ADMONITION = /^:::(note|tip|info|warning|danger|caution)\b[ \t]*(?:\[([^\]]*)\]|(.*))$/;

/** Prose rules shared by .md and .mdx: an admonition becomes a bold label. */
function flatten(text: string): string {
  const out: string[] = [];
  for (const line of text.split('\n')) {
    const adm = ADMONITION.exec(line);
    if (adm) {
      const kind = adm[1][0].toUpperCase() + adm[1].slice(1);
      const title = (adm[2] ?? adm[3] ?? '').trim();
      out.push(`**${title ? `${kind}: ${title}` : kind}.**`);
    } else if (!/^:::\s*$/.test(line)) {
      out.push(line);
    }
  }
  return out.join('\n');
}

/** MDX-only rules: imports, exports, comments, components. */
function stripMdx(text: string): string {
  let t = text.replace(/\{\/\*[\s\S]*?\*\/\}/g, '');
  t = t.replace(/^import\s.+$/gm, '');
  t = t.replace(/^export\s+(?:default\s+)?(?:function|const|class)\b[\s\S]*?^\}\)?;?[ \t]*$/gm, '');
  t = t.replace(/^export\s.+$/gm, '');
  return renderJsx(t);
}

export interface CleanOptions {
  /** True for .mdx: JSX, imports and exports are processed. */
  mdx: boolean;
  /** Rewrites a relative link target (anchor removed); undefined keeps it. */
  rewriteLink?: (target: string) => string | undefined;
}

/**
 * HTML comments removed until none is left: one pass over `<!<!---->--`
 * would leave `<!--` behind.
 */
function stripComments(text: string): string {
  let t = text;
  for (let previous = ''; previous !== t; ) {
    previous = t;
    t = t.replace(/<!--[\s\S]*?-->/g, '');
  }
  return t;
}

/** The clean body of a page, without front matter, ending in one newline. */
export function cleanBody(body: string, options: CleanOptions): string {
  const parts = segments(body).map((s) => {
    if (s.fence) return s.text;
    let t = stripComments(s.text);
    if (options.mdx) t = stripMdx(t);
    t = flatten(t);
    const rewrite = options.rewriteLink;
    if (rewrite) {
      t = t.replace(/(\]\()([^)\s]+)(\s+"[^"]*")?\)/g, (all, open: string, href: string, title = '') => {
        if (/^([a-z][a-z0-9+.-]*:|#|\/\/)/i.test(href)) return all;
        const [path, anchor] = href.split('#');
        const rewritten = rewrite(path);
        return rewritten === undefined ? all : `${open}${rewritten}${anchor ? `#${anchor}` : ''}${title})`;
      });
    }
    return t;
  });
  return `${parts.join('\n').replace(/\n{3,}/g, '\n\n').trim()}\n`;
}
