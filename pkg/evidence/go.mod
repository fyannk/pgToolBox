module github.com/fyannk/pgToolBox/pkg/evidence

// The module graph derives no more than 1.26.0. The floor is held at 1.26.6,
// above the standard-library vulnerabilities fixed there (net/url,
// html/template, crypto/tls, net/http, encoding/asn1), but a floor cannot
// keep builds clear of advisories published since. The pinned toolchain
// does that: CI builds and runs govulncheck at it, never at the floor.
go 1.26.6

toolchain go1.27.4
