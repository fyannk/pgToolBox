// Copyright © contributors to the pgtoolbox project.
// SPDX-License-Identifier: Apache-2.0
//
// A deliberately small reader for tokens.css and the vendored fyannk.css. Both
// have one shape: top-level rules whose bodies are custom-property
// declarations, and @media blocks each wrapping one rule. It is not a CSS parser; the tests fail loudly on any
// shape it does not understand, so the file cannot drift into one it misreads.

/** Remove comments, which may contain braces and semicolons. */
function stripComments(css) {
  return css.replace(/\/\*[\s\S]*?\*\//g, "");
}

/**
 * Split css into blocks of {selector, body}, descending one level into @media.
 * @param {string} css
 * @returns {{scope: string, declarations: Map<string,string>}[]}
 */
export function parseBlocks(css) {
  const text = stripComments(css);
  const blocks = [];
  let i = 0;
  const readUntil = (ch) => {
    const j = text.indexOf(ch, i);
    if (j < 0) throw new Error(`tokens.css: expected ${ch} after offset ${i}`);
    const s = text.slice(i, j).trim();
    i = j + 1;
    return s;
  };
  const readBody = () => {
    let depth = 1;
    const start = i;
    while (depth > 0) {
      if (i >= text.length) throw new Error("tokens.css: unbalanced braces");
      if (text[i] === "{") depth++;
      if (text[i] === "}") depth--;
      i++;
    }
    return text.slice(start, i - 1);
  };
  const declarations = (body) => {
    const map = new Map();
    for (const raw of body.split(";")) {
      const decl = raw.trim();
      if (!decl) continue;
      const colon = decl.indexOf(":");
      if (colon < 0) throw new Error(`tokens.css: not a declaration: ${decl}`);
      const name = decl.slice(0, colon).trim();
      const value = decl
        .slice(colon + 1)
        .replace(/\s+/g, " ")
        .trim();
      if (map.has(name)) throw new Error(`tokens.css: ${name} declared twice in one rule`);
      map.set(name, value);
    }
    return map;
  };
  while (true) {
    while (i < text.length && /\s/.test(text[i])) i++;
    if (i >= text.length) break;
    const selector = readUntil("{").replace(/\s+/g, " ");
    const body = readBody();
    if (selector.startsWith("@media")) {
      const inner = parseBlocks(body);
      for (const b of inner) blocks.push({ scope: `${selector} > ${b.scope}`, declarations: b.declarations });
    } else {
      blocks.push({ scope: selector, declarations: declarations(body) });
    }
  }
  return blocks;
}

const BASE = ":root";
const LIGHT_MEDIA = '@media (prefers-color-scheme: light) > :root:not([data-theme="dark"])';
const LIGHT_ATTR = ':root[data-theme="light"]';
const REDUCED_MOTION = "@media (prefers-reduced-motion: reduce) > :root";

function scoped(css, file, scopes) {
  const blocks = parseBlocks(css);
  const known = new Set(scopes);
  for (const b of blocks) if (!known.has(b.scope)) throw new Error(`${file}: unexpected block "${b.scope}"`);
  const out = {};
  for (const scope of scopes) {
    const found = blocks.filter((b) => b.scope === scope);
    if (found.length !== 1) throw new Error(`${file}: expected exactly one "${scope}" block, found ${found.length}`);
    out[scope] = found[0].declarations;
  }
  return out;
}

/**
 * The tokens by theme. Both files are dark-first, the shape fyannk/design
 * uses: the base rule is the dark theme, and light overrides it under the OS
 * preference (unless dark is pinned) and under [data-theme="light"].
 * fyannk.css holds the --fy-* palette, vendored; tokens.css the console's
 * semantic tokens, mapped onto it. Each theme map holds both, so var()
 * references between them resolve.
 * @param {string} css tokens.css
 * @param {string} fyCss fyannk.css
 */
export function readTokens(css, fyCss) {
  const spa = scoped(css, "tokens.css", [BASE, LIGHT_MEDIA, LIGHT_ATTR, REDUCED_MOTION]);
  const fy = scoped(fyCss, "fyannk.css", [BASE, LIGHT_MEDIA, LIGHT_ATTR]);
  const merge = (...maps) => {
    const out = {};
    for (const m of maps) for (const [k, v] of m) if (k.startsWith("--")) out[k] = v;
    return out;
  };
  return {
    base: spa[BASE],
    lightMedia: spa[LIGHT_MEDIA],
    lightAttr: spa[LIGHT_ATTR],
    reducedMotion: spa[REDUCED_MOTION],
    fy: { base: fy[BASE], lightMedia: fy[LIGHT_MEDIA], lightAttr: fy[LIGHT_ATTR] },
    dark: merge(fy[BASE], spa[BASE]),
    light: merge(fy[BASE], fy[LIGHT_ATTR], spa[BASE], spa[LIGHT_ATTR]),
  };
}
