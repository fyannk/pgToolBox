# Development

Build and test:

```bash
make generate manifests   # after any api/ change
make build
make test
make lint
```

The full contribution guide — repository invariants, layout, git workflow —
lives in [`CONTRIBUTING.md`](https://github.com/fyannk/pgtoolbox/blob/main/CONTRIBUTING.md)
in the repository.

Build this site locally:

```bash
cd web
npm install
npm start
```
