// Writes one bare package.json per npm package the SPA can bundle (the
// production dependency closure), as <out>/<name>@<version>/package.json. The
// console embeds the SPA in its binary, where a scanner sees no package; the
// image ships this directory under /usr/share/sbom/npm, which syft and grype
// read. A package the bundler dropped may be listed; one it shipped is not
// missing. Run from ui/: node scripts/npm-sbom.mjs <out>
import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

const out = process.argv[2];
if (!out) throw new Error("usage: node scripts/npm-sbom.mjs <out>");

// Node's lookup: the nearest node_modules/<name> above the dependent. pnpm
// links into its store, so the dependent's real path is the one to walk up.
function locate(name, from) {
  for (let dir = from; ; dir = dirname(dir)) {
    const manifest = join(dir, "node_modules", name, "package.json");
    if (existsSync(manifest)) return realpathSync(manifest);
    if (dirname(dir) === dir) return null;
  }
}

const seen = new Set();
function visit(manifest) {
  const pkg = JSON.parse(readFileSync(manifest, "utf8"));
  const from = dirname(manifest);
  const required = Object.keys(pkg.dependencies ?? {});
  const optional = [...Object.keys(pkg.optionalDependencies ?? {}), ...Object.keys(pkg.peerDependencies ?? {})];
  for (const name of new Set([...required, ...optional])) {
    const found = locate(name, from);
    if (!found) {
      if (required.includes(name)) throw new Error(`${pkg.name} needs ${name}, which is not installed`);
      continue;
    }
    if (seen.has(found)) continue;
    seen.add(found);
    const dep = JSON.parse(readFileSync(found, "utf8"));
    if (!dep.private) {
      const dir = join(out, `${dep.name}@${dep.version}`);
      mkdirSync(dir, { recursive: true });
      const { name: n, version, license } = dep;
      writeFileSync(join(dir, "package.json"), `${JSON.stringify({ name: n, version, license })}\n`);
    }
    visit(found);
  }
}

visit(join(process.cwd(), "package.json"));
if (seen.size === 0) throw new Error("no dependency found");
