import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

const config: Config = {
  title: 'pgToolBox',
  tagline: 'One declarative access stack per CloudNativePG cluster',
  favicon: 'img/favicon.svg',

  // GitHub Pages for fyannk/pgtoolbox.
  url: 'https://fyannk.github.io',
  baseUrl: '/pgtoolbox/',
  trailingSlash: true,

  organizationName: 'fyannk',
  projectName: 'pgtoolbox',

  onBrokenLinks: 'throw',
  onBrokenMarkdownLinks: 'throw',

  markdown: {
    mermaid: true,
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
          includeCurrentVersion: true,
          versions: {
            current: {
              label: 'Dev',
              badge: true,
              banner: 'unreleased',
            },
          },
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],
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
    navbar: {
      title: 'pgToolBox',
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docs',
          position: 'left',
          label: 'Documentation',
        },
        {
          type: 'docsVersionDropdown',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
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
