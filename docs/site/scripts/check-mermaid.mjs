// Copyright 2026 The pgToolBox Authors.
// SPDX-License-Identifier: Apache-2.0
//
// Parses every ```mermaid block of the docs with Mermaid itself.
//
// Docusaurus renders Mermaid in the browser, so a diagram with a syntax error
// builds fine and shows up only as a red "Syntax error in text" box for the
// reader. This turns that into a failed build, with the file and line.
//
//   node scripts/check-mermaid.mjs        (npm run check:mermaid, make docs)

import {existsSync, readdirSync, readFileSync} from 'node:fs';
import {join, relative} from 'node:path';
import {fileURLToPath} from 'node:url';
import DOMPurify from 'dompurify';

// Mermaid sanitizes labels with DOMPurify, which needs a browser window and
// is inert in Node. Parsing does not depend on the sanitized text, so the
// same instance Mermaid imports is given pass-through methods.
for (const name of ['addHook', 'removeHook', 'removeHooks', 'removeAllHooks']) DOMPurify[name] ??= () => {};
DOMPurify.sanitize = (text) => String(text);
const {default: mermaid} = await import('mermaid');

// Every page under docs/: a diagram that stops parsing under a newer Mermaid
// would show as an error box.
const site = fileURLToPath(new URL('../', import.meta.url));
const roots = ['docs'].map((d) => join(site, d)).filter(existsSync);

function* markdownFiles(dir) {
  for (const entry of readdirSync(dir, {withFileTypes: true})) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) yield* markdownFiles(path);
    else if (/\.mdx?$/.test(entry.name)) yield path;
  }
}

// Yields {line, text} for each fenced mermaid block; line is the fence's.
function* mermaidBlocks(source) {
  const lines = source.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const open = lines[i].match(/^(\s*)(`{3,}|~{3,})mermaid\s*$/);
    if (!open) continue;
    const fence = open[2];
    const body = [];
    let j = i + 1;
    for (; j < lines.length && !lines[j].trimStart().startsWith(fence); j++) body.push(lines[j]);
    yield {line: i + 1, text: body.join('\n')};
    i = j;
  }
}

let blocks = 0;
const failures = [];
for (const file of roots.flatMap((root) => [...markdownFiles(root)])) {
  for (const {line, text} of mermaidBlocks(readFileSync(file, 'utf8'))) {
    blocks++;
    try {
      await mermaid.parse(text);
    } catch (err) {
      failures.push(`${relative(site, file)}:${line}: ${String(err.message ?? err).split('\n').slice(0, 3).join(' ')}`);
    }
  }
}

if (failures.length > 0) {
  console.error(`${failures.length} of ${blocks} Mermaid diagrams do not parse:`);
  for (const f of failures) console.error(`  ${f}`);
  process.exit(1);
}
console.log(`${blocks} Mermaid diagrams parse.`);
