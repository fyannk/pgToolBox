// Copyright 2026 The pgToolBox Authors.
// SPDX-License-Identifier: Apache-2.0
//
// Local Docusaurus plugin: after the build, writes for every
// loaded documentation version one clean Markdown file per page
// (`<version>/<page>.md`), an `llms.txt` index and an `llms-full.txt` per
// version, and `docs-manifest.json` for `make docs-bundle`. The version that
// is served at the site root gets the root `llms.txt`.

import {existsSync, mkdirSync, readFileSync, writeFileSync} from 'node:fs';
import {dirname, join} from 'node:path';
import type {LoadContext, Plugin} from '@docusaurus/types';
import {renderSite, type LoadedVersion, type SourceReader} from './pages.ts';

interface DocsContent {
  loadedVersions: LoadedVersion[];
}

export default function pluginLlms(context: LoadContext): Plugin {
  const {siteDir, siteConfig} = context;
  const reader: SourceReader = {
    read(source) {
      const file = join(siteDir, source.replace(/^@site\//, ''));
      return existsSync(file) ? readFileSync(file, 'utf8') : undefined;
    },
    categoryLabel(version, dirName) {
      // The docs directory of a version: `docs` for current, `versioned_docs/version-X` otherwise.
      const sample = version.docs.find((d) => d.sourceDirName.split('/')[0] === dirName);
      const root = sample?.source.slice(0, sample.source.lastIndexOf(sample.sourceDirName) - 1);
      if (!root) return undefined;
      const file = join(siteDir, root.replace(/^@site\//, ''), dirName, '_category_.json');
      try {
        return (JSON.parse(readFileSync(file, 'utf8')) as {label?: string}).label;
      } catch {
        return undefined;
      }
    },
  };

  return {
    name: 'pgtoolbox-plugin-llms',
    async postBuild({outDir, plugins}) {
      const docs = plugins.find((p) => p.name === 'docusaurus-plugin-content-docs');
      const versions = (docs?.content as DocsContent | undefined)?.loadedVersions ?? [];
      if (versions.length === 0) throw new Error('llms plugin: the docs plugin loaded no version');
      const files = renderSite(
        {
          url: siteConfig.url,
          baseUrl: siteConfig.baseUrl,
          trailingSlash: siteConfig.trailingSlash,
          title: siteConfig.title,
          tagline: siteConfig.tagline,
        },
        versions,
        reader,
      );
      for (const file of files) {
        const target = join(outDir, file.path);
        mkdirSync(dirname(target), {recursive: true});
        writeFileSync(target, file.content);
      }
      console.log(`[llms] ${files.length} files for ${versions.length} version(s)`);
    },
  };
}
