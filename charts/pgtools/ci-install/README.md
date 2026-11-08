# ct install cases

`hack/e2e-chart.sh` runs `ct install` once per `*-values.yaml` here, inside the
chart e2e's kind cluster. They differ from `../ci/`, which
`ct lint` renders and which names things a cluster does not have (Routes, a
ServiceMonitor CRD, a mirror registry, all-zero digests): these must install
and become Ready on a bare kind cluster with the CNPG CRDs.

| Case | What it proves |
|---|---|
| `console-default` | the default namespace console (one watched namespace) |
| `console-multi-namespace` | `watchNamespaces` with two namespaces: a Role and a token projection in each |
| `console-networkpolicy` | the console Ready with `networkPolicy.enabled` (run on the Calico cluster) |

The images are the ones the e2e built from the working tree and loaded; the script
passes their repository and tag with `--set`.
