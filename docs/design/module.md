# decision-waf NGINX module

A dynamic NGINX module that asks [decision-waf](https://github.com/0x4272616E646F6E/decision-waf) to allow or block each request on the routes it is attached to, alongside any `auth_request` check (such as tinyauth) on the same route. It is built for NGINX Gateway Fabric (NGF) and loaded into NGF's stock NGINX image from a ConfigMap: no custom images.

Status: implemented; this note is the design record (first release 0.1.0).

## Context

decision-waf already serves NGINX through HTTP forward-auth (`auth_request` to port 9002), headers only. On NGF that runs into two limits of stock NGINX:

- One `auth_request` per location. A route protected by tinyauth (`auth_request /auth`) cannot also call decision-waf, and putting `auth_request /_decision_waf` inside the `/auth` location does not work: NGINX skips access-phase handlers in subrequests, so the nested check never runs. This was confirmed with a throwaway NGINX container: the inner check answered 403 and the request was still served.
- The checks are only as broad as the routes they are attached to. In the homelab that prompted this, no route used the plain decision-waf filter and the one route using the chained filter was never inspected.

Goals:

- Per route, opt in: a route that lists the filter gets every request checked by decision-waf; tinyauth may be on the same route and both are enforced.
- Headers only (decision-waf's lite mode). Bodies are out of scope; they would need a body-capable forward-auth in decision-waf as well.
- No custom NGINX image. NGF owns its data plane Deployment; the module is mounted into the stock image.
- decision-waf needs no changes: the module speaks decision-waf's existing forward-auth contract.

Constraints found on NGF 2.8.0's data plane image (`ghcr.io/nginx/nginx-gateway-fabric/nginx:2.8.0`):

- NGINX 1.31.7, built `--with-compat`, on Alpine, running as uid 101.
- `/etc/nginx/nginx.conf` includes `/etc/nginx/main-includes/*.conf` before `events` and `http`, so a main-context `load_module` works.
- A module built `--with-compat` against the exact NGINX version loads without matching every configure flag; the version must match exactly, so each NGF release needs its own build.
- The image ships `libai_guardrails.so`, F5's guardrail filter for LLM chat traffic (OpenAI/Ollama message bodies sent to a guardrails scanner). It is not a general request-inspection hook, so it does not replace this module.

## Decisions

### A module of its own, not `auth_request`, not chaining in decision-waf

The module registers its own phase handler and its own directives, so it is enforced next to `auth_request` the way `auth_basic` and `auth_request` coexist.

Rejected:

- **Chaining inside decision-waf** (decision-waf allows, then calls tinyauth and relays its 401 or redirect). It needs no NGINX code, but it makes decision-waf responsible for calling the authentication server: a bug or a fail-open path there would skip authentication.
- **Internal-redirect tricks in snippets** (a check in one location, `try_files` to a named location with the other). They depend on the location names NGF generates.
- **A self-contained module with its own HTTP client to decision-waf.** Much more C in a security component. Reusing NGINX's proxy module through a subrequest keeps the module close to `ngx_http_auth_request_module` in size.
- **Envoy Gateway instead of NGF.** It runs decision-waf (bodies and gRPC included) and a separate external auth side by side with no custom code, but it means replacing the ingress.

### Directives

All are valid in `http`, `server` and `location`, and each level inherits from the one above.

| Directive | Default | Meaning |
|---|---|---|
| `decision_waf_request <uri> \| off;` | `off` | Send a subrequest to `<uri>`, an internal location that `proxy_pass`es to decision-waf, before the request continues. The location must be `internal` and not named, and must answer itself: no `rewrite`, `try_files`, `X-Accel-Redirect` or other internal redirect inside it. Any such redirect, and any answer from a location that is not internal, is no decision. `error_page` is not applied in the subrequest, so the proxy's 502/504 stays a 502/504. The shipped location sets `proxy_ignore_headers X-Accel-*`. |
| `decision_waf_request_set $variable <value>;` | none | After the subrequest, set `$variable` to `<value>` (which may use the subrequest's upstream variables, e.g. `$upstream_http_x_decision_waf_request_id`). Like `auth_request_set`. |
| `decision_waf_request_unavailable closed \| open;` | `closed` | What happens when decision-waf gives no decision (below). |

The module also provides a read-only variable, `$decision_waf_request_status`: the status the subrequest returned, empty before the check runs. It is informational only. The decided outcome, which marks the request as checked (see "Once per client request"), is stored in a separate module-owned variable that configuration can neither read nor write: its name cannot be written in a variable reference and it is not changeable.

### Applying decision-waf's answer

The 4xx passthrough is exactly 400, 403 and 413. The subrequest's response body is discarded, as with `auth_request`; a route's `error_page` can render JSON bodies, as decision-waf's shipped NGINX example does.

| Subrequest result | Request gets |
|---|---|
| 2xx | continues |
| 400, 403 (block) or 413 from decision-waf | that status |
| 503 from decision-waf (its own fail-closed decision) | 503, whatever `decision_waf_request_unavailable` says |
| no decision: connection refused or timeout (the proxy's 502 or 504), any other 5xx, any other 4xx (such as the 404 of a missing internal location), an unexpected 1xx or 3xx, an answer from a location that is not internal or after an internal redirect | `closed`: 503, logged as an error. `open`: continues, with a warning logged for every request let through. |

`DECISION_WAF_FAILURE_MODE` covers model failures inside decision-waf; `decision_waf_request_unavailable` covers decision-waf itself being unreachable or broken, as `failure_mode_allow` does for Envoy.

### Where it runs

The handler runs in NGINX's preaccess phase, after `limit_req` and `limit_conn`. NGINX runs a phase's handlers in reverse registration order and a dynamic module registers after the built-in ones, so the module moves its handler to the front of the preaccess handler array at postconfiguration time to run last in that phase. Per request:

1. `limit_req`, `limit_conn`
2. decision-waf
3. `allow` / `deny`, then `auth_request` (tinyauth)
4. the upstream

Consequences:

- `satisfy any` never skips decision-waf, because it runs in preaccess, and a decision-waf allow cannot stand in for tinyauth: `satisfy` combines only access-phase handlers. `allow` / `deny` and `auth_request` still combine as NGINX defines, so `satisfy any; allow all;` lets tinyauth be skipped: that is NGINX's behaviour, not the module's.
- Rate limits protect decision-waf.
- Scanners hitting tinyauth-protected routes are inspected even though tinyauth then redirects them to login.
- Requests that `allow` / `deny` would reject are still sent to decision-waf first (a model call each, and visible in decision-waf's logs).

### Once per client request

- The handler does nothing in subrequests (`r != r->main`), so tinyauth's `/auth` and `/_decision_waf` itself are never checked, even when `decision_waf_request` is inherited from `server`.
- Internal redirects after the check (`error_page`, named locations such as `@error401`) reuse the result: the module stores the outcome in its indexed variable, and request variables survive internal redirects (module contexts do not). The stored outcome is the decision, not the status, so a block stays a block and a fail-open pass is not logged twice.
- With `decision_waf_request` at server level, an `error_page` target that reaches preaccess therefore reuses the original decision: a block page is blocked again. Render block pages with `return`, or set `decision_waf_request off;` there. The per-route `SnippetsFilter` (location context) avoids this and is the supported way to choose which routes are checked.
- `rewrite … last` happens before preaccess, so the check runs once, in the rewritten location.

### Delivery: a ConfigMap, no image

Each release ships, for every supported NGF version and architecture:

- `ngx_http_decision_waf_module-<version>-ngf<ngf>-linux-<arch>.so`
- `decision-waf-nginx-module-ngf<ngf>-<arch>.yaml`: a ConfigMap named `decision-waf-nginx-module-ngf<ngf>` holding the `.so` as `binaryData` (a module of this size is far under the 1 MiB ConfigMap limit)
- `SHA256SUMS` and a build-provenance attestation per `.so`

Releases are immutable, so a published asset cannot be replaced.

Installation on NGF (example manifests in `deploy/ngf/`):

1. The ConfigMap, in the namespace where NGF runs the NGINX pods, as a remote kustomize resource from the release URL (or the release's YAML committed, if a direct file URL is not accepted). Apply it server-side (`kubectl apply --server-side`, or Flux, which does): client-side apply stores the object in the `last-applied-configuration` annotation, which is capped at 256 KiB, and the manifest is close to that.
2. NGF's Helm values: `nginxGateway.snippets.enable: true` (snippets are off by default), and an `nginx.patches` entry mounting the ConfigMap read-only at `/usr/lib/nginx/modules/decision-waf/` in the stock NGINX container. The patch must be `type: JSONPatch`, with an `add` to `/spec/template/spec/volumes/-` and an `add` to `/spec/template/spec/containers/0/volumeMounts/-` (`StrategicMerge` fails: NGF merges lists by index, so the patch overwrites NGF's own volumes and the Deployment is rejected). It addresses the nginx container by index (`containers/0`), which must be re-checked on each NGF upgrade. The ConfigMap name carries the NGF version, so a bump changes the pod template and rolls the pods.
3. One Gateway-wide `SnippetsPolicy`:
   - `main`: `load_module /usr/lib/nginx/modules/decision-waf/ngx_http_decision_waf_module.so;`
   - `http.server`: the internal `location = /_decision_waf` that proxies to decision-waf's forward-auth port, defined once per server (per-route definitions collide when one server has several rules)
   - `http.server`: `proxy_hide_header X-Decision-Waf-Request-Id;`, so an upstream cannot spoof the header
   - `http.server`: `add_header X-Decision-Waf-Request-Id $decision_waf_request_id always;`, at server level so routes keep server-level headers (a location-level `add_header` drops them), and empty, so not sent, where decision-waf is not attached
4. A per-route `SnippetsFilter` in `http.server.location`: `decision-waf-check` (closed) and `decision-waf-check-open`:
   ```nginx
   decision_waf_request /_decision_waf;
   decision_waf_request_set $decision_waf_request_id $upstream_http_x_decision_waf_request_id;
   decision_waf_request_unavailable open;   # decision-waf-check-open only
   ```

Failure behaviour:

- A directive used without the module loaded fails `nginx -t`; NGF rejects the config and the previous one keeps serving. The failure is reported on the Gateway (`Programmed=False`, reason `Invalid`, with the `nginx -t` output) and in the controller and data plane logs; the route and `SnippetsFilter` conditions stay green, so alert on the Gateway condition.
- An NGF bump without a matching `.so`: new NGINX pods fail `load_module` and crash-loop while the rolling update keeps the old pods serving. The gateway stalls; it never runs without decision-waf.

### Versions and CI

- The module has its own semver.
- `versions.json` lists each supported NGF version with its NGINX version and the NGINX source SHA-256. It drives the CI and release matrices and the README table; a test checks they agree. The latest three NGF releases are supported.
- Every workflow: `permissions: {}` with per-job grants, `harden-runner`, actions pinned to commit SHAs, `persist-credentials: false`.

| Workflow | When | What |
|---|---|---|
| `ci.yml` | push, PR | builds every NGF version × {amd64, arm64} inside the stock NGF image; cppcheck; behaviour, sanitizer and decision-waf contract suites; actionlint and zizmor |
| `kind.yml` | weekly, on demand, from release | NGF-in-kind end-to-end test |
| `security.yml` | push, PR, weekly | CodeQL (C/C++, manual build), Gitleaks, dependency review |
| `scorecard.yml` | weekly | OpenSSF Scorecard |
| `release.yml` | `v*` tag | tagged commit on `main`; full suite and kind; build; ConfigMaps, `SHA256SUMS`, provenance; immutable GitHub release with notes from the CHANGELOG |
| `ngf-watch.yml` | weekly | opens a PR adding a new NGF release to `versions.json` (NGINX version from the image's `nginx -V`, source SHA-256 from nginx.org) |

Dependabot covers GitHub Actions, the Go test harness, and the test stack's base images. A deploying repo should group the NGF chart bump with the module asset URL in one update (the homelab's Renovate does this).

## Testing

Go tests driving Docker, as in decision-waf:

1. **Build.** The NGINX source for each version is checked against the pinned SHA-256. The module is compiled inside the stock NGF image with `--with-compat` and `-Wall -Wextra -Werror`, then loaded with `nginx -t`. cppcheck over the source.
2. **Behaviour suite.** A compose stack: the stock NGF NGINX image with the `.so` mounted read-only and loaded from main-includes, as in production; a stub decision-waf whose answers each test sets; a tinyauth stub (401 and redirect); an echo upstream. Every stub records its calls. Cases:
   - every row of the answer table, under `closed` and `open`, including the fail-open warning;
   - ordering and stacking: a decision-waf block means tinyauth is never called; a decision-waf allow followed by a tinyauth 401 still redirects; `satisfy any` never skips decision-waf (`satisfy any; allow all;` still skips tinyauth, as NGINX defines); `limit_req` rejects before decision-waf;
   - exactly one decision-waf call across `error_page`, named locations and `rewrite … last`, and with the directive at `server` level, where `/auth` and `/_decision_waf` must not trigger checks; `$decision_waf_request_id` survives internal redirects;
   - GET, HEAD and POST: the body never reaches decision-waf and reaches the upstream intact; decision-waf sees the original method and URI; large headers, keepalive and concurrency; a client hanging up mid-check.
3. **Sanitizers.** The same suite against the same NGINX version built on Debian with AddressSanitizer and UBSan (they do not work on the NGF image's Alpine).
4. **Real decision-waf and NGF.**
   - Contract test: the published decision-waf image, pinned, with a minimal OpenAI-compatible stub model. It confirms the `X-Original-*` headers are read, allow and block, the request ID, the `X-Forwarded-*` blanking, and that decision-waf logs the client's Host and NGINX's `$remote_addr` even when a client forges `X-Forwarded-Host`, `X-Real-IP` or `X-Forwarded-For`.
   - NGF in kind (weekly, on demand, before each release): real NGF with `nginxGateway.snippets.enable` and the Helm `nginx.patches` JSONPatch mount, the main-context `SnippetsPolicy`, per-route `SnippetsFilter`s, decision-waf and the tinyauth stub; allow, block and stacking end to end. It uses its own temporary kubeconfig.

## Verification before implementation

These are assumptions the design rests on; the first plan task checks each in a throwaway setup and records the result here.

- NGINX loads a `.so` from a ConfigMap volume mount (`dlopen` needs a readable file; the kubelet does not mount ConfigMaps `noexec`).
- NGF accepts `load_module` in a `main`-context `SnippetsPolicy` and places it before `http`.
- NGF rejects, and reports, a configuration that uses `decision_waf_request` without the module loaded, and keeps serving the previous one.
- kustomize and Flux accept a ConfigMap as a remote file URL (a GitHub release asset).
- Moving the handler to the front of the preaccess array makes it run after `limit_req`, and request variables survive `error_page` and named-location redirects.

### Results (2026-10-21)

Checked on kind (v0.33.0) with NGF 2.8.0 (Helm chart 2.8.0, Gateway API CRDs v1.6.3), using the stub module (`decision_waf on;` adds `X-Decision-Waf-Module: enabled`), built inside the stock image: 126,824 bytes.

- **`.so` from a ConfigMap mount: verified.** NGINX `dlopen`ed `/usr/lib/nginx/modules/decision-waf/ngx_http_decision_waf_module.so` from the ConfigMap volume (mounted `ro`, not `noexec`; the file is a symlink into the kubelet's `..data` directory) and a request through the Gateway returned the module's header. Ruling: the mount path in the global constraints stands.
- **`load_module` in a `main`-context `SnippetsPolicy`: verified.** NGF writes the snippet to `/etc/nginx/includes/SnippetsPolicy_main_default-<name>.conf` and includes it from `/etc/nginx/main-includes/main.conf`, which `nginx.conf` includes before `events` and `http` (after the stock `load_module modules/ngx_http_js_module.so;`). The policy reports `Accepted` and `Programmed`. Two corrections to the install steps, applied in Delivery step 2:
  - Snippets are off by default; the Helm value is `nginxGateway.snippets.enable: true` (it adds `--snippets` to the controller). Without it the route's `SnippetsFilter` is "Not found" and the route serves 500.
  - `nginx.patches` with `type: StrategicMerge` is merged by list index, not by name, and the Deployment is rejected (volumes and mounts overwrite NGF's own). Use `type: JSONPatch` with `add` operations on `/spec/template/spec/volumes/-` and `/spec/template/spec/containers/0/volumeMounts/-` (the `nginx` container is first). The working values are in `deploy/ngf/` once Task 8 lands them.
- **Directive without the module: verified (rejected, reported, old config serves).** With the `SnippetsPolicy` deleted and the route filter still using `decision_waf on;`, NGF's `nginx -t` failed (`unknown directive "decision_waf"`), the agent rolled back, and requests still returned 200 with the header. The failure shows as the Gateway's `Programmed=False` condition (reason `Invalid`, message carrying the `nginx -t` output) and in the NGF controller log (`Failed to update NGINX configuration`); the data plane log has "Config apply failed, rollback successful". The `HTTPRoute` and `SnippetsFilter` conditions stay green, so alert on the Gateway's `Programmed` condition. Re-applying the policy cleared it.
- **Remote file URL in kustomize: verified for `kubectl kustomize`; Flux not exercised.** kustomize v5.8.1 (bundled with kubectl v1.37.1) accepted an `https://` file as a `resources:` entry for both a `raw.githubusercontent.com` file and a GitHub release asset (`github.com/<org>/<repo>/releases/download/<tag>/<file>.yaml`, which redirects to a signed URL). A URL that does not exist (404) is reported as "URL is a git repository", which is misleading but harmless. Flux's `Kustomization` documentation says nothing about remote file resources; kustomize-controller (v1.9.6 in Flux v2.9.6) builds with the same library and only blocks remote bases when started with `--no-remote-bases`. Ruling: the remote asset URL is the documented option, with committing the release's ConfigMap YAML as the fallback for clusters that disallow remote bases or when Flux's build fails; the design's wording already says so.
- **Preaccess ordering and variables across redirects: not checked here.** Both need the real handler; they stay in the behaviour suite (Testing, item 2).

## Limitations and trade-offs

- Headers only. Bodies would need a body-capable forward-auth in decision-waf and body buffering at the gateway.
- One build per NGF version: an NGF upgrade needs the matching release first.
- Requests that `allow` / `deny` would reject still cost a decision-waf call.
- NGF only: a plain NGINX can load the module the same way, but only NGF's image is built and tested.

## History

- 2026-10-21: Design approved: own preaccess handler next to `auth_request`, configurable `ailuros_request_unavailable` (default closed), headers only, delivered by ConfigMap into NGF's stock image.
- 2026-10-21: Implemented as 0.1.0. Review-driven changes to the design as first written: the `ailuros_request` location must be `internal`, not named, and answer itself, and no `error_page` or internal redirect applies in the subrequest (any redirect is no decision); 4xx passthrough narrowed to exactly 400, 403 and 413; the decided outcome is stored in a separate module-owned variable instead of `$ailuros_request_status`; the handler is placed at index 0 of the preaccess array to run last in the phase; the policy hides `X-Ailuros-Request-Id` from upstreams (`proxy_hide_header`) and the shipped location ignores `X-Accel-*` headers; the `satisfy any` and server-level `error_page` consequences are documented.
- 2026-10-22: Renamed from Ailuros to decision-waf as 0.2.0, a clean break with no aliases: module `ngx_http_decision_waf_module`, directives `decision_waf_request*`, variables `$decision_waf_request_*`, location `/_decision_waf`, header `X-Decision-Waf-Request-Id`, mount dir `/usr/lib/nginx/modules/decision-waf/`. Nothing else in the design changed.
