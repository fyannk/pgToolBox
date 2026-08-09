import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

const config: Config = {
  title: 'pgToolBox',
  tagline: 'One declarative access stack per CloudNativePG cluster',
  favicon: 'img/favicon.ico',

  // GitHub Pages for fyannk/pgToolBox. Both are case-sensitive: served
  // under /pgtoolbox/ every stylesheet and script 404s, which renders as
  // an unstyled page rather than as an error.
  url: 'https://fyannk.github.io',
  baseUrl: '/pgToolBox/',
  trailingSlash: true,

  organizationName: 'fyannk',
  projectName: 'pgToolBox',

  onBrokenLinks: 'throw',

  markdown: {
    mermaid: true,
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
          includeCurrentVersion: true,
          versions: {
            // There is no versioned_docs snapshot yet, so "current" is the
            // released documentation. It must not carry the "unreleased"
            // banner, which would put that warning on every published page.
            current: {
              label: '0.1.0',
              badge: true,
              banner: 'none',
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
    // The card unfurled by chat clients and social previews: the same
    // lockup the navbar shows, so a link to the docs is recognisably the
    // project.
    image: 'img/social-card.png',
    metadata: [
      {
        name: 'description',
        content:
          'pgToolBox is a Kubernetes operator that gives one CloudNativePG cluster a complete access stack: authentication proxy, observation console, and embedded pgAdmin.',
      },
    ],
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'pgToolBox',
      logo: {
        alt: 'pgToolBox',
        src: 'img/logo.png',
        // The navbar is navy in both themes, so the mark is the same file.
        // Without srcDark, Docusaurus renders only a light-themed image and
        // the logo disappears entirely in dark mode.
        srcDark: 'img/logo.png',
      },
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
        {
          href: 'https://github.com/fyannk/pgToolBox',
          position: 'right',
          className: 'navbar-github',
          'aria-label': 'pgToolBox on GitHub',
        },
      ],
    },
    footer: {
      style: 'dark',
      logo: {
        alt: 'pgToolBox',
        src: 'img/logo.png',
        srcDark: 'img/logo.png',
        href: 'https://github.com/fyannk/pgToolBox',
        width: 84,
      },
      copyright: `Copyright © ${new Date().getFullYear()} pgToolBox contributors. Apache-2.0 licensed.`,
      links: [
        {
          title: 'Project',
          items: [
            {
              label: 'GitHub',
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
