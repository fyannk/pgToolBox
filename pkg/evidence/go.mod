module github.com/fyannk/pgToolBox/pkg/evidence

// The module graph derives no more than 1.26.0. The floor is held above
// that deliberately: 1.26.0 through 1.26.5 carry the standard-library
// vulnerabilities fixed in 1.26.6 — net/url, html/template, crypto/tls,
// net/http, encoding/asn1 — and a GOTOOLCHAIN=local build against them
// would produce a binary govulncheck rejects. CI never sees that, because
// CI builds at the pinned toolchain and never at the floor.
go 1.26.6

toolchain go1.27.4
