# Changelog

All notable changes to the decision-waf NGINX module are listed here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0,
a minor version may change behavior or configuration; the entry says how.

## [Unreleased]

## [0.2.0] - 2026-10-22

The project is renamed from Ailuros to decision-waf. This is a clean break:
the old names are not accepted as aliases, so configuration that uses them
fails `nginx -t` until it is updated. Behavior is otherwise unchanged.

### Changed (breaking)

| What | Old | New |
|---|---|---|
| Repository | `0x4272616E646F6E/ailuros-nginx-module` | `0x4272616E646F6E/decision-waf-nginx-module` |
| Module name | `ngx_http_ailuros_module` | `ngx_http_decision_waf_module` |
| Module file | `ngx_http_ailuros_module.so` | `ngx_http_decision_waf_module.so` |
| Directive | `ailuros_request` | `decision_waf_request` |
| Directive | `ailuros_request_set` | `decision_waf_request_set` |
| Directive | `ailuros_request_unavailable` | `decision_waf_request_unavailable` |
| Variable | `$ailuros_request_status` | `$decision_waf_request_status` |
| Variable (shipped policy) | `$ailuros_request_id` | `$decision_waf_request_id` |
| Variable (shipped policy) | `$ailuros_upstream` | `$decision_waf_upstream` |
| Reserved internal variable | `ailuros-request-outcome` | `decision-waf-request-outcome` |
| Internal location | `/_ailuros` | `/_decision_waf` |
| Response header | `X-Ailuros-Request-Id` | `X-Decision-Waf-Request-Id` |
| Upstream header read by the filters | `$upstream_http_x_ailuros_request_id` | `$upstream_http_x_decision_waf_request_id` |
| Mount directory | `/usr/lib/nginx/modules/ailuros/` | `/usr/lib/nginx/modules/decision-waf/` |
| ConfigMap name | `ailuros-nginx-module-ngf<ngf>` | `decision-waf-nginx-module-ngf<ngf>` |
| ConfigMap label key | `ailuros.dev/arch` | `decision-waf.dev/arch` |
| ConfigMap `app.kubernetes.io/name` | `ailuros-nginx-module` | `decision-waf-nginx-module` |
| Helm patch volume name | `ailuros-module` | `decision-waf-module` |
| Release asset (module) | `ngx_http_ailuros_module-<v>-ngf<ngf>-linux-<arch>.so` | `ngx_http_decision_waf_module-<v>-ngf<ngf>-linux-<arch>.so` |
| Release asset (ConfigMap) | `ailuros-nginx-module-ngf<ngf>-<arch>.yaml` | `decision-waf-nginx-module-ngf<ngf>-<arch>.yaml` |
| SnippetsPolicy | `ailuros` | `decision-waf` |
| SnippetsFilter (fail closed) | `ailuros-check` | `decision-waf-check` |
| SnippetsFilter (fail open) | `ailuros-check-open` | `decision-waf-check-open` |
| Backend Service host in the policy | `ailuros.<namespace>.svc.cluster.local` | `decision-waf.<namespace>.svc.cluster.local` |
| Build and test variables | `AILUROS_NGF`, `AILUROS_MODULE_SO`, `AILUROS_MODULE_IMAGE`, `AILUROS_VERSION` | `DECISION_WAF_NGF`, `DECISION_WAF_MODULE_SO`, `DECISION_WAF_MODULE_IMAGE`, `DECISION_WAF_VERSION` |

To migrate, apply the new release's ConfigMap and the updated Helm values
patch, SnippetsPolicy and SnippetsFilters together, point HTTPRoute filters at
the new SnippetsFilter names, and delete the old ConfigMap, policy and
filters. The upstream request ID header follows decision-waf: the filters
read `X-Decision-Waf-Request-Id`, which decision-waf 0.2.0 sends.

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
  trusts from the real request, so clients cannot forge them, and resolves the
  Ailuros Service per request, so NGF reloads never depend on DNS.
- CI (build, cppcheck, behaviour, AddressSanitizer and Ailuros contract
  suites), an NGF-in-kind end-to-end test, CodeQL, Scorecard, and a weekly
  watch for new NGF releases.
