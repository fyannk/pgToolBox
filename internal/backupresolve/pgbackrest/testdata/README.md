# Plugin example manifests at the pinned tags

Copied unchanged from the plugins' repositories; `types_test.go` decodes each
into the subset types of this package. A pin bump replaces these files with the
new tag's and reruns the test.

| Directory | Source | Tag | Commit |
|---|---|---|---|
| `dalibo-v0.0.3/` | `github.com/dalibo/cnpg-i-pgbackrest`, `examples/` | `v0.0.3` | `f0c7bc34a3fd591aebce7f517dc822da20a2e59e` |
| `opera-v0.9.0/` | `github.com/operasoftware/cnpg-plugin-pgbackrest`, `docs/examples/` | `v0.9.0` | `588e2786837ab4941148c53540263d7793764c63` |

`dalibo-v0.0.3/stanza_custom.yaml` is not valid YAML at that tag (its
`CustomEnvVar` key is indented as a sibling of a sequence item, and spelled
unlike the field's `customEnvVar` tag); the test asserts that it still does not
parse, so a fixed upstream copy is noticed. Both projects are Apache-2.0.
