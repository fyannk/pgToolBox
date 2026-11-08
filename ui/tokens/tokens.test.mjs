// Copyright © contributors to the pgtoolbox project.
// SPDX-License-Identifier: Apache-2.0
//
// The invariants of tokens.css that the server-rendered auth pages and the
// SPA all rely on. WCAG AA contrast of every pair is contrast.test.mjs.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { render } from "./scripts/generate.mjs";
import { readTokens } from "./scripts/parse.mjs";

const css = readFileSync(new URL("tokens.css", import.meta.url), "utf8");
const fyCss = readFileSync(new URL("fyannk.css", import.meta.url), "utf8");
const t = readTokens(css, fyCss);

test("the committed tokens.ts is generated from tokens.css", () => {
  assert.equal(readFileSync(new URL("tokens.ts", import.meta.url), "utf8"), render());
});

test("tokens.ts loads and agrees with the parser", async () => {
  const mod = await import("./tokens.ts");
  assert.deepEqual([...mod.tokenNames], Object.keys(t.light).sort());
  assert.equal(mod.cssVar("--accent"), "var(--accent)");
  assert.equal(mod.tokens.dark["--bg"], t.dark["--bg"]);
});

test("the OS-preference light rule and the pinned light rule declare the same palette", () => {
  // The prefers-color-scheme block is what an unpinned reader gets; the
  // attribute block is what a pinned one gets. They must not drift apart, in
  // the console's tokens nor in the vendored fyannk palette.
  assert.deepEqual([...t.lightMedia].sort(), [...t.lightAttr].sort());
  assert.deepEqual([...t.fy.lightMedia].sort(), [...t.fy.lightAttr].sort());
});

test("a light override never invents a token the dark base does not declare", () => {
  for (const name of t.lightAttr.keys())
    assert.ok(t.base.has(name), `${name} is overridden in light but has no base value`);
  for (const name of t.fy.lightAttr.keys()) {
    if (name === "color-scheme") continue;
    assert.ok(t.fy.base.has(name), `${name} is overridden in light but has no base value in fyannk.css`);
  }
});

test("the console's tokens never redefine the vendored --fy-* palette", () => {
  for (const block of [t.base, t.lightAttr, t.lightMedia]) {
    for (const name of block.keys())
      assert.ok(!name.startsWith("--fy-"), `${name} is fyannk's; change fyannk/design instead`);
  }
});

test("every var() reference resolves in both themes", () => {
  for (const theme of [t.light, t.dark]) {
    for (const value of Object.values(theme)) {
      for (const [, ref] of value.matchAll(/var\((--[\w-]+)\)/g))
        assert.ok(theme[ref] !== undefined, `${ref} is not declared`);
    }
  }
});

test("status colours exist in both themes with their weak variants", () => {
  for (const theme of [t.light, t.dark]) {
    for (const n of [
      "--ok",
      "--warn",
      "--bad",
      "--warn-weak",
      "--bad-weak",
      "--accent",
      "--text",
      "--bg",
      "--surface",
    ]) {
      assert.ok(theme[n], `${n} missing`);
    }
  }
});
