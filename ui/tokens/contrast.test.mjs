// Copyright © contributors to the pgtoolbox project.
// SPDX-License-Identifier: Apache-2.0
//
// WCAG AA contrast of every text/background and glyph/background pair the
// design system draws, in both themes ("WCAG AA contrast of every
// pair becomes a token unit test"). Text needs 4.5:1; a glyph or a component
// boundary that carries meaning needs 3:1 (WCAG 1.4.11).

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { contrast, luminance, luminanceOf, parseOklch } from "./scripts/contrast.mjs";
import { readTokens } from "./scripts/parse.mjs";

const t = readTokens(
  readFileSync(new URL("tokens.css", import.meta.url), "utf8"),
  readFileSync(new URL("fyannk.css", import.meta.url), "utf8"),
);
const themes = { light: t.light, dark: t.dark };

const HEALTH = ["ok", "progressing", "warning", "critical", "inactive", "unknown", "info"];
const SURFACES = ["--bg", "--surface", "--surface-2"];
const TEXT = 4.5;
const GRAPHIC = 3;

test("the converter agrees with known sRGB anchors", () => {
  assert.ok(Math.abs(luminance(parseOklch("oklch(100% 0 0)")) - 1) < 1e-3);
  assert.ok(luminance(parseOklch("oklch(0% 0 0)")) < 1e-6);
  // Black on white is 21:1.
  const bw = contrast({ "--a": "oklch(0% 0 0)", "--b": "oklch(100% 0 0)" }, "--a", "--b");
  assert.ok(Math.abs(bw - 21) < 0.05, `black on white is ${bw}`);
  // Hex and color-mix(in srgb) agree with the same colours as oklch.
  assert.ok(Math.abs(luminanceOf("#ffffff") - 1) < 1e-9);
  assert.ok(Math.abs(luminanceOf("color-mix(in srgb, #ffffff 50%, #000000)") - luminanceOf("#808080")) < 2e-3);
  assert.throws(() => luminanceOf("color-mix(in srgb, #ffffff 14%, transparent)"), /translucent/);
});

test("every health state declares fg, bg, border and icon in both themes", () => {
  for (const [name, theme] of Object.entries(themes)) {
    for (const s of HEALTH) {
      for (const part of ["fg", "bg", "border", "icon"]) {
        assert.ok(theme[`--health-${s}-${part}`], `${name}: --health-${s}-${part} missing`);
      }
    }
  }
});

const pairs = [];
for (const s of HEALTH) {
  // The chip: word on its tint, and on the stale (untinted) background.
  pairs.push([`--health-${s}-fg`, `--health-${s}-bg`, TEXT]);
  pairs.push([`--health-${s}-fg`, "--stale-bg", TEXT]);
  // Inline state words on plain surfaces (tables, banners' secondary lines).
  for (const bg of SURFACES) pairs.push([`--health-${s}-fg`, bg, TEXT]);
  // The glyph is meaningful graphics.
  pairs.push([`--health-${s}-icon`, `--health-${s}-bg`, GRAPHIC]);
  pairs.push([`--health-${s}-icon`, "--surface", GRAPHIC]);
}
// unknown's dashed border is the shape cue that distinguishes it; it must be seen.
pairs.push(["--health-unknown-border", "--surface", GRAPHIC]);
pairs.push(["--stale-border", "--surface", GRAPHIC]);
for (const bg of SURFACES) {
  pairs.push(["--text", bg, TEXT]);
  pairs.push(["--text-muted", bg, TEXT]);
}
// Text on the accent tint (palette highlight, added diff lines).
pairs.push(["--text", "--accent-weak", TEXT]);
pairs.push(["--accent", "--surface", TEXT]);
pairs.push(["--accent", "--bg", TEXT]);
pairs.push(["--focus-ring", "--surface", GRAPHIC]);
pairs.push(["--topbar-text", "--topbar-bg", TEXT]);
pairs.push(["--topbar-muted", "--topbar-bg", TEXT]);
// Text on the accent fill (primary buttons) and the storage domain colour,
// which labels the wiring drawing's storage frames.
pairs.push(["--accent-ink", "--accent", TEXT]);
pairs.push(["--storage", "--surface", TEXT]);
pairs.push(["--storage", "--bg", TEXT]);
// The wiring drawing's flow strokes are meaningful graphics.
for (const f of ["--flow-write", "--flow-read", "--flow-replicate", "--flow-archive", "--flow-volume"]) {
  pairs.push([f, "--surface", GRAPHIC]);
  pairs.push([f, "--surface-2", GRAPHIC]);
}
// Plain status words on the surfaces and their weak tints (banners).
for (const s of ["--ok", "--warn", "--bad", "--unknown"]) pairs.push([s, "--surface", TEXT]);
pairs.push(["--warn", "--warn-weak", TEXT]);
pairs.push(["--bad", "--bad-weak", TEXT]);
// Tinted rows: the body text, the muted text and the state word (labels and
// links take it) on the row tint; the leading bar against the tint.
for (const s of ["warning", "critical", "progressing", "unknown"]) {
  for (const fg of ["--text", "--text-muted", `--health-${s}-fg`]) pairs.push([fg, `--row-${s}-bg`, TEXT]);
  pairs.push([`--health-${s}-icon`, `--row-${s}-bg`, GRAPHIC]);
}
// Finding count squares: the number on its severity tint.
for (const s of ["--sev-critical", "--sev-note"]) {
  pairs.push([s, `${s}-bg`, TEXT]);
  pairs.push([s, "--surface", TEXT]);
}

for (const [name, theme] of Object.entries(themes)) {
  for (const [fg, bg, min] of pairs) {
    test(`${name}: ${fg} on ${bg} ≥ ${min}:1`, () => {
      const ratio = contrast(theme, fg, bg);
      assert.ok(ratio >= min, `${name}: ${fg} on ${bg} is ${ratio.toFixed(2)}:1, needs ${min}:1`);
    });
  }
}

// The 8-series chart palette. A series mark is meaningful
// graphics: on the dark surface every slot clears 3:1. On the light surface the
// reference palette keeps slots 3–5 (aqua, yellow, magenta) under 3:1 by design;
// the relief rule applies (MetricChart always ships a legend with direct labels
// and a table view), and the other five must still clear it. The colour-blind
// separation of adjacent slots is validated where the palette was chosen
// (dataviz validator, recorded in tokens.css) and pinned here by value.
const SERIES = [1, 2, 3, 4, 5, 6, 7, 8].map((i) => `--series-${i}`);
const LIGHT_RELIEF = new Set(["--series-3", "--series-4", "--series-5"]);

test("eight series slots exist in both themes", () => {
  for (const [name, theme] of Object.entries(themes))
    for (const s of SERIES) assert.ok(theme[s], `${name}: ${s} missing`);
});

for (const s of SERIES) {
  test(`dark: ${s} on --surface ≥ ${GRAPHIC}:1`, () => {
    const ratio = contrast(themes.dark, s, "--surface");
    assert.ok(ratio >= GRAPHIC, `dark: ${s} is ${ratio.toFixed(2)}:1`);
  });
  if (!LIGHT_RELIEF.has(s))
    test(`light: ${s} on --surface ≥ ${GRAPHIC}:1`, () => {
      const ratio = contrast(themes.light, s, "--surface");
      assert.ok(ratio >= GRAPHIC, `light: ${s} is ${ratio.toFixed(2)}:1`);
    });
}

test("reduced motion zeroes every motion token and nothing else", () => {
  const motion = Object.keys(t.light).filter((n) => n.startsWith("--motion-"));
  assert.ok(motion.length > 0);
  assert.deepEqual([...t.reducedMotion.keys()].sort(), motion.sort());
  for (const v of t.reducedMotion.values()) assert.equal(v, "0ms");
});
