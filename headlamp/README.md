# pgToolBox Headlamp plugin (`pgtoolbox-console`)

A thin [Headlamp](https://headlamp.dev) plugin that links CloudNativePG clusters to
the [pgToolBox](https://github.com/fyannk/pgToolBox) console. The console stays the main
UI; this plugin only helps people who already browse their clusters in Headlamp get
there. Documentation: <https://fyannk.github.io/pgToolBox/>.

## What it does

On the details page of a CloudNativePG `Cluster` (`clusters.postgresql.cnpg.io`, any
API version), Headlamp's generic custom-resource view gets:

- **An "Open in the pgToolBox console" header action.** It is a plain link to
  `<pgToolBox console URL>/c/<namespace>/<cluster>` (each segment percent-encoded), opened in a
  new tab with `rel="noopener noreferrer"`. When the URL is not set or not a valid
  http(s) URL, the button is disabled and the reason is written next to it, for example
  "pgToolBox console URL is not configured in the plugin settings". The plugin never guesses a URL.
- **A "CloudNativePG phase" section.** It shows `status.phase` exactly as the operator
  wrote it ("Phase: Cluster in healthy state"), or "Phase: not reported" when absent, and
  `status.readyInstances`/`status.instances` ("2/3 instances ready"). The data comes
  from the object Headlamp already fetched; the plugin makes no extra API calls.
- **An "Operator phase" column** on Headlamp's list of `clusters` custom resources. Rows
  from other CRDs whose plural is also `clusters` (Cluster API, for example) are left empty.

The phase is shown as words with a neutral glyph; no colour is used to carry meaning.

## What it does not do

- **No health verdict.** The phase is what the operator reports, not a judgment. The
  verdict (replication, backups, freshness of the status, diagnostics) is computed by
  the console's Go backend; the section links there for it.
- No actions on the cluster (no switchover, restart, backup), no writes to Kubernetes.
- No authentication bridging: the pgToolBox console asks for its own sign-in as usual.
- No per-namespace console URL: one base URL per Headlamp user. A team that runs several
  consoles sees links to the configured one only.

## Setting

Headlamp → Settings → Plugins → `pgtoolbox-console` → **pgToolBox console URL**, for example
`https://console.team-a.example.com` or `https://example.com/pgtoolbox`. It must be
an absolute `http://` or `https://` URL without credentials, query or fragment; a
trailing slash is ignored. Headlamp keeps plugin settings in the browser's local storage,
so each user sets it once per browser.

## Install

Build first (see below), then either:

- **Package and extract** (desktop or in-cluster):

  ```sh
  npm run package          # pgtoolbox-console-0.1.0.tar.gz
  tar xzf pgtoolbox-console-0.1.0.tar.gz -C ~/.config/Headlamp/plugins/   # desktop, Linux/macOS
  tar xzf pgtoolbox-console-0.1.0.tar.gz -C /headlamp/plugins/            # in-cluster -plugins-dir
  ```

- **Extract the build directly**: `npx headlamp-plugin extract . <plugins-dir>`, which
  writes `<plugins-dir>/pgtoolbox-console/{main.js,package.json}`.

For an in-cluster Headlamp, put the extracted directory in the volume mounted at the
`-plugins-dir` path (an init container copying from an image, or the Headlamp chart's
plugin options). The plugin is not published to a plugin catalog yet.

## Develop and check

Toolchain: `@kinvolk/headlamp-plugin` (exact pin in `package.json`), npm with the
committed `package-lock.json`. Node 24 works (one engine warning from `i18next-parser`).

```sh
npm run check   # npm ci, then lint, tsc, test, build
npm start       # rebuild on change while Headlamp loads the plugin
npm run format  # prettier
```

`npm run check` is what CI runs (Makefile target `headlamp-check`). Tests are Vitest via
`headlamp-plugin test`: `src/cluster.test.ts` covers the URL builder and the badge text,
`src/components.test.tsx` renders the action and the section.

Layout: `src/cluster.ts` pure helpers (no React), `src/components.tsx` UI,
`src/settings.tsx` settings page, `src/config.ts` settings store, `src/index.tsx`
registrations.
