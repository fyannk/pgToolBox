// Copyright © contributors to the pgtoolbox project.
// SPDX-License-Identifier: Apache-2.0
//
// Styling rules Biome cannot express (CSP):
//  - no raw colour anywhere in components: no hex/rgb/hsl/oklch literal, no
//    arbitrary-value colour utility (`bg-[#fff]`, `text-[oklch(...)]`);
//  - no `style` prop and no <style> element: styling is semantic utilities on
//    tokens, and style-src is 'self' only;
//  - no dangerouslySetInnerHTML (operator text is rendered escaped).

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const SKIP = new Set(["generated", "paraglide"]);
const RULES = [
  [/#[0-9a-fA-F]{3,8}\b(?![\w-])/, "raw hex colour", (line) => /["'`]#[0-9a-fA-F]{3,8}["'`\s;]/.test(line)],
  [/\b(?:rgba?|hsla?|oklch|oklab|lab|lch|color-mix)\(/, "raw colour function"],
  [/-\[(?:#|rgb|hsl|oklch|oklab|color-mix|var\(--(?:health|ok|warn|bad))/, "arbitrary-value colour utility"],
  [/\bstyle=\{/, "style prop (use semantic utilities)"],
  [/<style[\s>]/, "<style> element (CSP style-src 'self')"],
  [/dangerouslySetInnerHTML/, "dangerouslySetInnerHTML"],
];

function* files(dir) {
  for (const name of readdirSync(dir)) {
    if (SKIP.has(name)) continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) yield* files(p);
    else if (/\.(tsx?|mdx)$/.test(name) && !name.endsWith(".gen.ts")) yield p;
  }
}

let bad = 0;
for (const dir of ["src", ".storybook"]) {
  for (const file of files(join(root, dir))) {
    const lines = readFileSync(file, "utf8").split("\n");
    lines.forEach((line, i) => {
      if (line.includes("check-semantic: allow")) return;
      for (const [re, what, guard] of RULES) {
        if (re.test(line) && (!guard || guard(line))) {
          process.stderr.write(`${relative(root, file)}:${i + 1}: ${what}\n  ${line.trim()}\n`);
          bad++;
        }
      }
    });
  }
}
if (bad) {
  process.stderr.write(`${bad} styling violation(s)\n`);
  process.exit(1);
}
