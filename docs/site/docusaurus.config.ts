import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';
import {readFileSync} from 'node:fs';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

// The version the site documents is the chart's: both charts are released
// together with the images.
const release = /^version:\s*(\S+)/m.exec(
  readFileSync(new URL('../../charts/pgtoolbox/Chart.yaml', import.meta.url), 'utf8'),
)?.[1] ?? 'unreleased';

const config: Config = {
  title: 'pgToolBox',
  tagline: 'A console, an operator and an evidence agent for CloudNativePG',
  favicon: 'img/favicon.ico',

  // Served by GitHub Pages at https://fyannk.github.io/pgToolBox/. DOCS_URL and
  // DOCS_BASE_URL serve the same build elsewhere (DOCS_BASE_URL=/ for a local
  // `docusaurus serve` at the root); baseUrl is case-sensitive and must end with
  // a slash, or every stylesheet and script 404s and the page renders unstyled.
  url: process.env.DOCS_URL ?? 'https://fyannk.github.io',
  baseUrl: process.env.DOCS_BASE_URL ?? '/pgToolBox/',
  trailingSlash: true,

  organizationName: 'fyannk',
  projectName: 'pgToolBox',

  // Every internal link, anchor and Markdown link that does not resolve fails the
  // build (`make docs`, the CI docs job). External links are not checked.
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',
  onDuplicateRoutes: 'throw',

  markdown: {
    mermaid: true,
    // .md is CommonMark, .mdx is MDX: the generated CRD reference (crd-ref-docs)
    // contains raw HTML and braces that MDX would reject.
    format: 'detect',
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          path: 'docs',
          sidebarPath: './sidebars.ts',
          // "Edit this page" opens the file in GitHub's web editor.
          editUrl: 'https://github.com/fyannk/pgToolBox/edit/main/docs/site/',
          // One version: the site documents the release it is built from, with
          // no "unreleased" banner and no badge on every page.
          versions: {
            current: {label: release, badge: false, banner: 'none'},
          },
        },
        blog: false,
        theme: {
          // fyannk.css (the design tokens) first: custom.css maps Infima onto them.
          customCss: ['./src/css/fyannk.css', './src/css/custom.css'],
        },
      } satisfies Preset.Options,
    ],
  ],
  // Writes the Markdown of every page, llms.txt and llms-full.txt after the
  // build (plugins/llms).
  plugins: ['./plugins/llms/index.ts'],
  clientModules: ['./src/client/mermaid-scale.ts'],
  themes: [
    '@docusaurus/theme-mermaid',
    [
      require.resolve('@easyops-cn/docusaurus-search-local'),
      {
        hashed: true,
        docsDir: ['docs'],
        searchResultLimits: 8,
        searchResultContextMaxLength: 50,
        language: ['en'],
        indexBlog: false,
        indexPages: false,
      },
    ],
  ],
  themeConfig: {
    // Mermaid does not scale diagrams; src/client/mermaid-scale.ts does,
    // between legible and fitting the column.
    mermaid: {
      options: {
        flowchart: {useMaxWidth: false},
        sequence: {useMaxWidth: false},
        state: {useMaxWidth: false},
      },
    },
    // The card unfurled by chat clients and social previews: the mark and
    // the wordmark, rendered from brand/social-card.html by brand/render.mjs.
    image: 'img/social-card.png',
    metadata: [
      {
        name: 'description',
        content:
          'pgToolBox is a per-team console, a small operator and an evidence agent for CloudNativePG clusters, on Kubernetes and OpenShift.',
      },
    ],
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'pgToolBox',
      logo: {
        alt: 'pgToolBox',
        src: 'img/logo.svg',
        // The mark sits on its own gradient tile, so one file serves both
        // themes. Without srcDark, Docusaurus renders only a light-themed
        // image and the logo disappears in dark mode.
        srcDark: 'img/logo.svg',
      },
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docs',
          position: 'left',
          label: 'Documentation',
        },
        {
          // src/pages/security.tsx: the latest security run, read from the
          // results.json the Pages workflow writes beside it.
          to: '/security/',
          label: 'Security',
          position: 'left',
        },
        {
          // The release these pages describe, linking to its notes.
          href: `https://github.com/fyannk/pgToolBox/releases/tag/v${release}`,
          label: `v${release}`,
          position: 'right',
        },
        {
          href: 'https://github.com/fyannk/pgToolBox',
          position: 'right',
          className: 'navbar-github',
          'aria-label': 'pgToolBox repository',
        },
      ],
    },
    footer: {
      style: 'dark',
      logo: {
        alt: 'pgToolBox',
        src: 'img/logo.svg',
        srcDark: 'img/logo.svg',
        href: 'https://github.com/fyannk/pgToolBox',
        width: 56,
      },
      copyright: `Copyright © 2026 The pgToolBox Authors. Apache-2.0 licensed.`,
      links: [
        {
          title: 'Project',
          items: [
            {
              label: 'Source',
              href: 'https://github.com/fyannk/pgToolBox',
            },
            {
              label: 'Releases',
              href: 'https://github.com/fyannk/pgToolBox/releases',
            },
            {
              label: 'Report a vulnerability',
              href: 'https://github.com/fyannk/pgToolBox/security/policy',
            },
            {
              // src/pages/security.tsx, fed by the Pages workflow's results.json.
              label: 'Security scan results',
              to: '/security/',
            },
          ],
        },
        {
          title: 'CloudNativePG',
          items: [
            {
              label: 'Website',
              href: 'https://cloudnative-pg.io',
            },
            {
              label: 'Slack',
              href: 'https://cloud-native.slack.com/messages/cloudnativepg-users',
            },
          ],
        },
        {
          title: 'pgAdmin',
          items: [
            {
              label: 'pgAdmin documentation',
              href: 'https://www.pgadmin.org/docs/',
            },
          ],
        },
      ],
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'yaml', 'go'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
