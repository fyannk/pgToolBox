// Copyright 2026 The pgToolBox Authors.
// SPDX-License-Identifier: Apache-2.0
//
// The pure half of the llms plugin: from loaded documentation versions to the
// files written next to the built site. No Docusaurus runtime, so node --test
// can exercise it.

import {cleanBody, splitFrontMatter} from './clean.ts';

/** The part of a loaded doc this module reads. */
export interface LoadedDoc {
  id: string;
  title: string;
  description?: string;
  /** `@site/docs/console/tour.mdx` */
  source: string;
  sourceDirName: string;
  /** With the base URL: `/1.3/console/tour/`. */
  permalink: string;
  frontMatter: {sidebar_position?: unknown; [key: string]: unknown};
  unlisted?: boolean;
}

export interface LoadedVersion {
  versionName: string;
  label: string;
  /** The version's URL path with the base URL: `/1.3/`, `/next/` or `/`. */
  path: string;
  isLast: boolean;
  docs: LoadedDoc[];
}

export interface Site {
  /** Origin, without a trailing slash. */
  url: string;
  /** Always ends with a slash. */
  baseUrl: string;
  /** The site's trailingSlash setting: pages are served at `/x/` when true. */
  trailingSlash?: boolean;
  title: string;
  tagline: string;
}

export interface SourceReader {
  /** The text of a docs source (`@site/…`), or undefined if it is missing. */
  read(source: string): string | undefined;
  /** The label of a top-level docs directory from its `_category_.json`. */
  categoryLabel(version: LoadedVersion, dirName: string): string | undefined;
}

export interface OutputFile {
  /** Relative to the build directory. */
  path: string;
  content: string;
}

function trimBase(site: Site, path: string): string {
  return path.startsWith(site.baseUrl) ? path.slice(site.baseUrl.length) : path.replace(/^\//, '');
}

/**
 * Where a page's Markdown is written, relative to the build directory:
 * `/1.3/console/tour/` is `1.3/console/tour.md`, a directory's own page
 * (`/1.3/console/`) is `1.3/console/index.md`.
 */
export function mdFileFor(site: Site, doc: LoadedDoc): string {
  const rel = trimBase(site, doc.permalink).replace(/\/$/, '');
  // Docusaurus gives a directory's own page (index, README or a file named
  // like its folder) the permalink of the directory.
  const base = (doc.source.split('/').pop() ?? '').replace(/\.mdx?$/, '');
  const folder = doc.source.split('/').slice(-2, -1)[0];
  const isIndex = base === 'index' || base === 'README' || base === folder;
  if (rel === '') return 'index.md';
  return isIndex ? `${rel}/index.md` : `${rel}.md`;
}

/** The path a page is served at, as readers and search engines see it. */
export function canonicalPath(site: Site, doc: LoadedDoc): string {
  const p = doc.permalink;
  return site.trailingSlash && !p.endsWith('/') ? `${p}/` : p;
}

/** A version's path, always with a trailing slash. */
function versionDir(version: LoadedVersion): string {
  return version.path.endsWith('/') ? version.path : `${version.path}/`;
}

function absolute(site: Site, path: string): string {
  return `${site.url.replace(/\/$/, '')}${path}`;
}

/** The absolute URL of a page's Markdown file. */
function mdUrl(site: Site, doc: LoadedDoc): string {
  return absolute(site, `${site.baseUrl}${mdFileFor(site, doc)}`);
}

function yamlScalar(value: string): string {
  return /^[A-Za-z][\w .,()/@+-]*$/.test(value) && !/^(true|false|null|yes|no)$/i.test(value)
    ? value
    : JSON.stringify(value);
}

function resolve(dir: string, target: string): string {
  const parts = target.startsWith('/') ? [] : dir.split('/');
  for (const seg of target.split('/')) {
    if (seg === '..') parts.pop();
    else if (seg !== '.' && seg !== '') parts.push(seg);
  }
  return parts.join('/');
}

function listed(version: LoadedVersion): LoadedDoc[] {
  return version.docs
    .filter((d) => !d.unlisted)
    .sort((a, b) => {
      const pa = typeof a.frontMatter.sidebar_position === 'number' ? a.frontMatter.sidebar_position : 1e9;
      const pb = typeof b.frontMatter.sidebar_position === 'number' ? b.frontMatter.sidebar_position : 1e9;
      return pa - pb || a.title.localeCompare(b.title) || a.permalink.localeCompare(b.permalink);
    });
}

/** One page: front matter (title, version, canonical URL) and the clean body. */
export function renderPage(
  site: Site,
  version: LoadedVersion,
  doc: LoadedDoc,
  bySource: Map<string, LoadedDoc>,
  reader: SourceReader,
): string | undefined {
  const raw = reader.read(doc.source);
  if (raw === undefined) return undefined;
  const dir = doc.source.replace(/\/[^/]*$/, '');
  const clean = cleanBody(splitFrontMatter(raw).body, {
    mdx: /\.mdx$/.test(doc.source),
    rewriteLink: (target) => {
      if (!/\.mdx?$/.test(target)) return undefined;
      const linked = bySource.get(resolve(dir, target));
      return linked ? `${site.baseUrl}${mdFileFor(site, linked)}` : undefined;
    },
  });
  return [
    '---',
    `title: ${yamlScalar(doc.title)}`,
    `version: ${yamlScalar(version.label)}`,
    `url: ${absolute(site, canonicalPath(site, doc))}`,
    '---',
    '',
    clean,
  ].join('\n');
}

function sectionName(dirName: string, version: LoadedVersion, reader: SourceReader): string {
  if (dirName === '') return 'Overview';
  return reader.categoryLabel(version, dirName) ?? dirName.charAt(0).toUpperCase() + dirName.slice(1).replace(/[-_]/g, ' ');
}

/** `llms.txt`: the index of a version, grouped by top-level section. */
export function renderIndex(site: Site, version: LoadedVersion, versions: LoadedVersion[], reader: SourceReader): string {
  const groups = new Map<string, LoadedDoc[]>();
  for (const doc of listed(version)) {
    const top = doc.sourceDirName === '.' ? '' : doc.sourceDirName.split('/')[0];
    groups.set(top, [...(groups.get(top) ?? []), doc]);
  }
  const names = [...groups.keys()].sort((a, b) => (a === '' ? -1 : b === '' ? 1 : a.localeCompare(b)));
  const out = [
    `# ${site.title}`,
    '',
    `> ${site.tagline}. Documentation version ${version.label}.`,
    '',
    'Every page below is plain Markdown. `llms-full.txt` in the same directory holds them all in one file.',
    '',
  ];
  for (const name of names) {
    out.push(`## ${sectionName(name, version, reader)}`, '');
    for (const doc of groups.get(name)!) {
      // Only a description written in the front matter: Docusaurus otherwise
      // derives one by cutting the first lines of the page mid-sentence.
      const written = typeof doc.frontMatter.description === 'string' ? doc.frontMatter.description : '';
      const desc = written.trim() ? `: ${written.replace(/\s+/g, ' ').trim()}` : '';
      out.push(`- [${doc.title}](${mdUrl(site, doc)})${desc}`);
    }
    out.push('');
  }
  const others = versions.filter((v) => v !== version);
  if (others.length > 0) {
    out.push('## Other versions', '');
    for (const v of others) out.push(`- [${v.label}](${absolute(site, `${versionDir(v)}llms.txt`)})`);
    out.push('');
  }
  return out.join('\n');
}

/** `llms-full.txt`: every page of a version in one file, in index order. */
export function renderFull(site: Site, version: LoadedVersion, pages: Map<LoadedDoc, string>): string {
  const out: string[] = [`# ${site.title} documentation, version ${version.label}`, ''];
  for (const doc of listed(version)) {
    const page = pages.get(doc);
    if (page === undefined) continue;
    const {body} = splitFrontMatter(page);
    out.push('---', '', `Source: ${absolute(site, canonicalPath(site, doc))}`, '');
    out.push(/^#\s/m.test(body) ? body.trimEnd() : `# ${doc.title}\n\n${body.trimEnd()}`, '');
  }
  return out.join('\n');
}

/**
 * Every file the plugin writes for the loaded versions. `docs-manifest.json`
 * lists the pages for tools that consume the output (`make docs-bundle`).
 */
export function renderSite(site: Site, versions: LoadedVersion[], reader: SourceReader): OutputFile[] {
  const files: OutputFile[] = [];
  const manifest: unknown[] = [];
  for (const version of versions) {
    const bySource = new Map(version.docs.map((d) => [d.source, d]));
    const pages = new Map<LoadedDoc, string>();
    const entries: unknown[] = [];
    for (const doc of listed(version)) {
      const page = renderPage(site, version, doc, bySource, reader);
      if (page === undefined) continue;
      pages.set(doc, page);
      files.push({path: mdFileFor(site, doc), content: page});
      entries.push({title: doc.title, path: canonicalPath(site, doc).slice(site.baseUrl.length - 1), file: mdFileFor(site, doc)});
    }
    const dir = trimBase(site, versionDir(version));
    const index = renderIndex(site, version, versions, reader);
    const full = renderFull(site, version, pages);
    files.push({path: `${dir}llms.txt`, content: index}, {path: `${dir}llms-full.txt`, content: full});
    // The conventional place is the site root: it serves the default version.
    if (version.isLast && dir !== '') {
      files.push({path: 'llms.txt', content: index}, {path: 'llms-full.txt', content: full});
    }
    manifest.push({
      version: version.versionName,
      label: version.label,
      path: versionDir(version).slice(site.baseUrl.length - 1),
      isLast: version.isLast,
      pages: entries,
    });
  }
  files.push({path: 'docs-manifest.json', content: `${JSON.stringify({versions: manifest}, null, 2)}\n`});
  return files;
}
