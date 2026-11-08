// Copies the console tour's screenshots from the UI's Playwright baselines
// (ui/e2e/__screenshots__/screens.spec.ts, rendered from the MSW mocks of the
// API fixtures) into static/img/screens, where the docs reference them.
//
// Run by npm before `build` and `start` (the `prebuild` and `prestart`
// scripts), so the site always shows the baselines of the checkout it is built
// from, and nothing is committed twice. static/img/screens is git-ignored.
// Copies are not re-encoded (pngquant, the only optimiser on hand, is lossy).
//
// The curated subset is the list below: 1440 px, light and dark. A missing
// source fails the build, so a renamed or deleted baseline cannot leave the tour
// with a broken image. Docker builds copy ui/e2e/__screenshots__/screens.spec.ts
// into the docs stage for the same reason (Dockerfile, .dockerignore).
import {copyFileSync, existsSync, mkdirSync, readdirSync, rmSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

const src = fileURLToPath(new URL('../../../ui/e2e/__screenshots__/screens.spec.ts/', import.meta.url));
const dst = fileURLToPath(new URL('../static/img/screens/', import.meta.url));
const width = 1440;
const screens = [
  'clusters', 'overview-healthy', 'instances', 'replication', 'backups', 'storage',
  'diagnostics', 'events', 'timeline', 'revision', 'metrics', 'insight-blocking',
  'fleet-insight', 'operations', 'access', 'digest', 'admin',
];
const files = screens.flatMap((s) => ['light', 'dark'].map((t) => `${s}-${t}-${width}.png`));

const missing = files.filter((f) => !existsSync(src + f));
if (missing.length > 0) {
  console.error(`copy-screens: missing in ${src}:\n  ${missing.join('\n  ')}`);
  process.exit(1);
}
mkdirSync(dst, {recursive: true});
for (const f of readdirSync(dst)) if (f.endsWith('.png')) rmSync(dst + f);
for (const f of files) copyFileSync(src + f, dst + f);
console.log(`copy-screens: ${files.length} files in static/img/screens`);
