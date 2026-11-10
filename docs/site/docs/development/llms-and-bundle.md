---
sidebar_position: 3
title: Documentation for assistants
description: llms.txt, one Markdown file per page, and the documentation bundle embedded in the console.
---

# Documentation for assistants

An assistant answering a question about pgToolBox should read the pages of the
version the person runs, as plain text, and find the right section by its exact
name. Three things on this site and in the console serve that.

## Version

The site documents one version: the release named in `charts/pgtoolbox/Chart.yaml`,
built from `main`. The navbar links that release's notes.

## `llms.txt` and one Markdown file per page

The build writes the clean Markdown of each page next to its
HTML (`/docs/console/tour.md` beside `/docs/console/tour/`; a section's own page
is `/docs/console/index.md`), an `llms.txt` index and an `llms-full.txt` with every
page in one file. The two files are also served at the site root (`/llms.txt`,
`/llms-full.txt`).

A page file starts with front matter naming its `title`, `version` and canonical
`url`. The body is the page's source with what only a browser needs removed: MDX
imports, exports and comments, components rendered to the text they carry (a
screenshot becomes its alt text), admonitions turned into a bold label, links to
other pages pointing at their `.md` files. Code fences, Mermaid diagrams included,
are kept as they are. A local plugin does this (`docs/site/plugins/llms`,
TypeScript, no dependency, tested by `npm test` in `docs/site`).

## The console's own documentation

The console binary embeds the pages of the version it was built for and a
prebuilt keyword index over them, so it can answer from the documentation that
matches it. The index is BM25 over sections (a `##` or `###` block is one chunk);
there are no embeddings: the pages are exact technical names, which keyword ranking
finds better than a model would, and nothing needs a model in the cluster.

The package is `internal/console/docsbundle`, standard library only:

| Call | Returns |
|---|---|
| `Search(query string, limit int) []Hit` | the best sections: `ChunkID`, `PageTitle`, `Section`, `Version`, `Path`, `Excerpt`, `Score`. `limit` defaults to 10 and is capped at 50 |
| `Page(path string) (Document, bool)` | one page (`Path`, `Title`, `Version`, `Markdown`), by the path it is served at, with or without a trailing slash or `.md` |
| `Pages() []Document` | every page, in path order |
| `Version() string` | the documentation version, the chart version the console is built with |

The tokenizer keeps identifiers findable: `history.persistent`,
`InstanceCABundleComplete` and `cluster-ref` each match as a whole and by their
parts, and a plural matches its singular. A query that writes an identifier whole
weighs it double.

The console's [MCP endpoint](../console/mcp.md) serves it through its
documentation tools and `docs://` resources.

`make docs-bundle` builds the site and writes `internal/console/docsbundle/data`:
the pages of `docs/` under `pages/` (except this development section, which documents building pgToolBox rather
than running it) and the index as `index.bin`, under 1 MiB in
all, byte-for-byte reproducible from the same pages.

The files are generated and not committed (only `data/.keep` is, so that
`go:embed` matches on a fresh clone): a committed binary index would make any two
pull requests that touch the docs conflict. `make docs`, part of CI, runs the
generator and then loads what it wrote and searches it (`make test-docs-bundle`),
so a site build the console cannot embed fails the pull request. The console
image builds the site and the bundle in its own Dockerfile stages, so every
image, `make images` and the release workflow included, carries the
documentation of its release. A console built from source without
`make docs-bundle` starts and logs a warning; its MCP endpoint serves everything
but the docs tools. The same holds for the tour's screenshots: the site build
copies them from `ui/e2e/__screenshots__/screens.spec.ts/`.
