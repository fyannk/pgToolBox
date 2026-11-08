// The Content-Security-Policy of the console. The Go console serves the real
// header; this copy is what the Playwright harness runs the build under, so a
// change here without the same change in Go is a test that lies.
export function cspHeader({ worker }: { worker: boolean }): string {
  return [
    "default-src 'none'",
    "script-src 'self'",
    "style-src 'self'",
    "connect-src 'self'",
    "img-src 'self' data:",
    "font-src 'self'",
    "base-uri 'none'",
    "form-action 'self'",
    "frame-ancestors 'none'",
    ...(worker ? ["worker-src 'self'"] : []),
  ].join("; ");
}
