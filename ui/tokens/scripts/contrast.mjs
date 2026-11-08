// Copyright © contributors to the pgtoolbox project.
// SPDX-License-Identifier: Apache-2.0
//
// WCAG 2.x contrast for the colours tokens.css and the vendored fyannk.css
// declare. Zero dependencies. Three value forms are understood, the ones the
// two files use: #rrggbb literals (the fyannk palette), oklch() literals
// (chart series), and color-mix(in srgb, A p%, B) of those (tints). oklch goes
// oklch → OKLab → linear sRGB (Björn Ottosson's matrices), clipped to the sRGB
// gamut the way a browser without wide-gamut output paints it. Everything is
// reduced to gamma-encoded sRGB, which is also the space color-mix(in srgb)
// interpolates in; then relative luminance and (L1 + 0.05) / (L2 + 0.05).

/**
 * Resolve a token to a value with every var() reference substituted.
 * @param {Record<string,string>} theme
 * @param {string} name
 */
export function resolve(theme, name, seen = new Set()) {
  if (seen.has(name)) throw new Error(`${name}: var() cycle`);
  const value = theme[name];
  if (value === undefined) throw new Error(`${name}: not declared`);
  const next = new Set(seen).add(name);
  return value.replace(/var\((--[\w-]+)\)/g, (_, ref) => resolve(theme, ref, next));
}

/** Parse "oklch(L% C H)" or "oklch(L% C H / A)". Alpha is rejected: a translucent colour has no fixed contrast. */
export function parseOklch(value) {
  const m = /^oklch\(\s*([\d.]+)%\s+([\d.]+)\s+([\d.]+)\s*(?:\/\s*([\d.]+))?\s*\)$/.exec(value);
  if (!m) throw new Error(`not an oklch() literal: ${value}`);
  if (m[4] !== undefined && Number(m[4]) !== 1) throw new Error(`translucent colour has no fixed contrast: ${value}`);
  return { l: Number(m[1]) / 100, c: Number(m[2]), h: Number(m[3]) };
}

const encode = (x) => (x <= 0.0031308 ? 12.92 * x : 1.055 * x ** (1 / 2.4) - 0.055);
const decode = (x) => (x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4);

/** Gamma-encoded sRGB [r, g, b] in 0..1 of an oklch colour, gamut-clipped. */
function oklchToSrgb({ l, c, h }) {
  const rad = (h * Math.PI) / 180;
  const a = c * Math.cos(rad);
  const b = c * Math.sin(rad);
  const l_ = (l + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m_ = (l - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s_ = (l - 0.0894841775 * a - 1.291485548 * b) ** 3;
  const clip = (x) => Math.min(1, Math.max(0, x));
  return [
    4.0767416621 * l_ - 3.3077115913 * m_ + 0.2309699292 * s_,
    -1.2684380046 * l_ + 2.6097574011 * m_ - 0.3413193965 * s_,
    -0.0041960863 * l_ - 0.7034186147 * m_ + 1.707614701 * s_,
  ].map((x) => encode(clip(x)));
}

/** Split the arguments of a function call at top-level commas. */
function args(inner) {
  const out = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < inner.length; i++) {
    if (inner[i] === "(") depth++;
    else if (inner[i] === ")") depth--;
    else if (inner[i] === "," && depth === 0) {
      out.push(inner.slice(start, i).trim());
      start = i + 1;
    }
  }
  out.push(inner.slice(start).trim());
  return out;
}

/** Gamma-encoded sRGB of a resolved colour value. */
export function toSrgb(value) {
  const v = value.trim();
  const hex = /^#([0-9a-f]{6})$/i.exec(v);
  if (hex) return [0, 2, 4].map((i) => Number.parseInt(hex[1].slice(i, i + 2), 16) / 255);
  if (v.startsWith("oklch(")) return oklchToSrgb(parseOklch(v));
  const mix = /^color-mix\((.*)\)$/.exec(v);
  if (mix) {
    const [space, a, b] = args(mix[1]);
    if (space !== "in srgb") throw new Error(`color-mix: only "in srgb" is understood: ${v}`);
    const part = (s) => {
      const m = /^(.*?)\s+([\d.]+)%$/.exec(s);
      return m ? { colour: m[1], p: Number(m[2]) / 100 } : { colour: s, p: undefined };
    };
    const pa = part(a);
    const pb = part(b);
    if (pa.colour === "transparent" || pb.colour === "transparent")
      throw new Error(`translucent colour has no fixed contrast: ${v}`);
    const wa = pa.p ?? (pb.p === undefined ? 0.5 : 1 - pb.p);
    const ca = toSrgb(pa.colour);
    const cb = toSrgb(pb.colour);
    return ca.map((x, i) => x * wa + cb[i] * (1 - wa));
  }
  throw new Error(`not a colour this checker reads: ${v}`);
}

/** Relative luminance (WCAG) of a resolved colour value. */
export function luminanceOf(value) {
  const [r, g, b] = toSrgb(value).map(decode);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** Relative luminance (WCAG) of an oklch colour, gamut-clipped to sRGB. */
export function luminance(lch) {
  const [r, g, b] = oklchToSrgb(lch).map(decode);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** WCAG contrast ratio of two token names in one theme. */
export function contrast(theme, fg, bg) {
  const y1 = luminanceOf(resolve(theme, fg));
  const y2 = luminanceOf(resolve(theme, bg));
  const [hi, lo] = y1 > y2 ? [y1, y2] : [y2, y1];
  return (hi + 0.05) / (lo + 0.05);
}
