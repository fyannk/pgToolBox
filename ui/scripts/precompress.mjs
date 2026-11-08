// Copyright © contributors to the pgtoolbox project.
// SPDX-License-Identifier: Apache-2.0
//
// Writes .br and .gz siblings for the text assets of a build, so
// the Go server serves pre-compressed files without compressing per request.
// Dependency-free: node:zlib only. Deterministic: fixed mtime-free gzip header
// and maximum quality, so two builds of one tree produce identical bytes.

import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { brotliCompressSync, constants, gzipSync } from "node:zlib";

const dir = process.argv[2] ?? "dist";
const TEXT = /\.(?:js|css|html|svg|json|txt|map)$/;
const MIN = 1024;

function* walk(d) {
  for (const name of readdirSync(d)) {
    const p = join(d, name);
    if (statSync(p).isDirectory()) yield* walk(p);
    else if (TEXT.test(name)) yield p;
  }
}

let n = 0;
for (const file of walk(dir)) {
  const body = readFileSync(file);
  if (body.length < MIN) continue;
  writeFileSync(`${file}.gz`, gzipSync(body, { level: 9 }));
  writeFileSync(
    `${file}.br`,
    brotliCompressSync(body, {
      params: { [constants.BROTLI_PARAM_QUALITY]: 11, [constants.BROTLI_PARAM_SIZE_HINT]: body.length },
    }),
  );
  n++;
}
process.stdout.write(`precompressed ${n} file(s) in ${dir}\n`);
