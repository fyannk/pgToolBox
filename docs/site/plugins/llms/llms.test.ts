// Copyright 2026 The pgToolBox Authors.
// SPDX-License-Identifier: Apache-2.0
//
//   npm test   (node --test, no dependency)

import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';
import {cleanBody, splitFrontMatter} from './clean.ts';
import {mdFileFor, renderSite, type LoadedDoc, type LoadedVersion, type Site} from './pages.ts';

const site: Site = {trailingSlash: true, url: 'https://docs.example.org', baseUrl: '/', title: 'pgToolBox', tagline: 'A tagline'};

test('front matter is split off and unquoted', () => {
  const {data, body} = splitFrontMatter('---\ntitle: "A: b"\nsidebar_position: 2\n---\n\n# Hi\n');
  assert.equal(data.title, 'A: b');
  assert.equal(body, '\n# Hi\n');
});

test('mdx imports, exports, comments and components become text', () => {
  const mdx = readFileSync(new URL('../../docs/console/tour.mdx', import.meta.url), 'utf8');
  const out = cleanBody(splitFrontMatter(mdx).body, {mdx: true});
  assert.doesNotMatch(out, /^import |^export |<Screen|<ThemedImage|useBaseUrl|tabIndex/m);
  assert.match(out, /\*\[Image: The Clusters list:/);
  assert.match(out, /^# A tour of the console$/m);
});

test('fences, mermaid included, are untouched', () => {
  const src = '```mermaid\nflowchart LR\n  a --> b\n```\n\n<!-- gone -->\n\n```text\n:::note\n<Screen />\nimport x\n```\n';
  const out = cleanBody(src, {mdx: true});
  assert.match(out, /```mermaid\nflowchart LR\n {2}a --> b\n```/);
  assert.match(out, /```text\n:::note\n<Screen \/>\nimport x\n```/);
  assert.doesNotMatch(out, /gone/);
});

test('a comment that a removed comment splices together is removed too', () => {
  const out = cleanBody('a <!<!-- x -->-- hidden --> b\n', {mdx: false});
  assert.equal(out, 'a  b\n');
});

test('admonitions flatten to a bold label', () => {
  const out = cleanBody(':::warning A copy expires\nBody.\n:::\n\n:::note\nPlain.\n:::\n', {mdx: false});
  assert.equal(out, '**Warning: A copy expires.**\nBody.\n\n**Note.**\nPlain.\n');
});

test('plain .md keeps angle-bracket placeholders', () => {
  const out = cleanBody('Use <X.Y.Z> here\n', {mdx: false});
  assert.equal(out, 'Use <X.Y.Z> here\n');
});

test('relative links to pages become markdown URLs, anchors kept', () => {
  const out = cleanBody('See [c](../operator/conditions.md#reasons) and [e](https://x.org/a.md) and [s](#top).\n', {
    mdx: false,
    rewriteLink: (t) => (t === '../operator/conditions.md' ? '/operator/conditions.md' : undefined),
  });
  assert.equal(out, 'See [c](/operator/conditions.md#reasons) and [e](https://x.org/a.md) and [s](#top).\n');
});

function doc(source: string, permalink: string, title: string): LoadedDoc {
  return {id: title, title, source, sourceDirName: source.split('/').slice(2, -1).join('/') || '.', permalink, frontMatter: {}};
}

test('markdown paths follow permalinks', () => {
  assert.equal(mdFileFor(site, doc('@site/docs/console/tour.mdx', '/1.3/console/tour/', 't')), '1.3/console/tour.md');
  assert.equal(mdFileFor(site, doc('@site/docs/console/index.md', '/1.3/console/', 't')), '1.3/console/index.md');
  assert.equal(mdFileFor(site, doc('@site/docs/index.mdx', '/', 't')), 'index.md');
});

test('one md per page, an index and a full file per version', () => {
  const sources: Record<string, string> = {
    '@site/docs/a.md': '---\ntitle: A\n---\n\n# A\n\nSee [b](sub/b.md).\n',
    '@site/docs/sub/b.md': '# B\n\n```mermaid\nflowchart LR\n```\n',
  };
  const a = doc('@site/docs/a.md', '/a/', 'A');
  const b = doc('@site/docs/sub/b.md', '/sub/b/', 'B');
  b.frontMatter = {description: 'The B page'};
  const v: LoadedVersion = {versionName: 'current', label: '1.3.0', path: '/', isLast: true, docs: [a, b]};
  const run = () =>
    renderSite(site, [v], {read: (s) => sources[s], categoryLabel: (_v, d) => (d === 'sub' ? 'Sub pages' : undefined)});
  const files = run();
  const byPath = new Map(files.map((f) => [f.path, f.content]));
  assert.deepEqual([...byPath.keys()].sort(), ['a.md', 'docs-manifest.json', 'llms-full.txt', 'llms.txt', 'sub/b.md']);
  assert.match(byPath.get('a.md')!, /^---\ntitle: A\nversion: "1.3.0"\nurl: https:\/\/docs.example.org\/a\/\n---\n/);
  assert.match(byPath.get('a.md')!, /\[b\]\(\/sub\/b\.md\)/);
  assert.match(byPath.get('llms.txt')!, /## Sub pages\n\n- \[B\]\(https:\/\/docs.example.org\/sub\/b\.md\): The B page/);
  assert.match(byPath.get('llms-full.txt')!, /Source: https:\/\/docs.example.org\/sub\/b\//);
  assert.deepEqual(run(), files, 'same input, same output');
});

test('a versioned site writes under each version path', () => {
  const next: LoadedVersion = {versionName: 'current', label: 'next (main)', path: '/next/', isLast: false, docs: [doc('@site/docs/a.md', '/next/a/', 'A')]};
  const old: LoadedVersion = {versionName: '1.3', label: '1.3', path: '/', isLast: true, docs: [doc('@site/versioned_docs/version-1.3/a.md', '/a/', 'A')]};
  const files = renderSite(site, [next, old], {
    read: () => '# A\n',
    categoryLabel: () => undefined,
  });
  const paths = files.map((f) => f.path).sort();
  assert.deepEqual(paths, ['a.md', 'docs-manifest.json', 'llms-full.txt', 'llms.txt', 'next/a.md', 'next/llms-full.txt', 'next/llms.txt']);
  assert.match(files.find((f) => f.path === 'llms.txt')!.content, /## Other versions\n\n- \[next \(main\)\]\(https:\/\/docs.example.org\/next\/llms.txt\)/);
});
