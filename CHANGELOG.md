# Changelog

All notable changes to the Ailuros NGINX module are listed here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0,
a minor version may change behavior or configuration; the entry says how.

## [Unreleased]

## [0.1.0] - 2026-10-22

The first release.

### Added

- The `ngx_http_ailuros_module` dynamic NGINX module: `ailuros_request`,
  `ailuros_request_set` and `ailuros_request_unavailable` (`closed`, the
  default, or `open`), and the read-only variable `$ailuros_request_status`.
  Each request on a route that enables it is checked by Ailuros through its
  HTTP forward-auth contract, headers only.
- The check runs in the preaccess phase, after `limit_req` and `limit_conn`
  and before `allow` / `deny` and `auth_request`, once per client request, so
  it stacks with tinyauth on the same route.
- Ailuros's answer: 2xx continues; 400, 403 and 413 are passed on; 503 is
  Ailuros's own fail-closed decision and is never overridden; anything else is
  no decision (503 when closed, continue with a logged warning when open).
- The `ailuros_request` location must be `internal` and answer itself;
  answers from other locations and from internal redirects are no decision.
- Builds for NGF 2.8.0 (NGINX 1.31.7), `amd64` and `arm64`, as `.so` files
  and ConfigMap manifests with `SHA256SUMS` and build-provenance attestations,
  loaded into NGF's stock NGINX image: no custom image.
- Example NGF installation in `deploy/ngf/`: Helm values patch, a
  `SnippetsPolicy` and per-route `SnippetsFilter`s (fail closed and fail
  open), with an `X-Ailuros-Request-Id` response header that upstreams cannot
  spoof. Its `/_ailuros` location sets the host and client IP headers Ailuros
  trusts from the real request, so clients cannot forge them.
- CI (build, cppcheck, behaviour, AddressSanitizer and Ailuros contract
  suites), an NGF-in-kind end-to-end test, CodeQL, Scorecard, and a weekly
  watch for new NGF releases.
