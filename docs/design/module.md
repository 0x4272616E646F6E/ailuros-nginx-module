# Ailuros NGINX module

A dynamic NGINX module that asks [Ailuros](https://github.com/0x4272616E646F6E/ailuros) to allow or block each request on the routes it is attached to, alongside any `auth_request` check (such as tinyauth) on the same route. It is built for NGINX Gateway Fabric (NGF) and loaded into NGF's stock NGINX image from a ConfigMap: no custom images.

Status: approved design, not yet implemented. This note becomes the design record once it ships.

## Context

Ailuros already serves NGINX through HTTP forward-auth (`auth_request` to port 9002), headers only. On NGF that runs into two limits of stock NGINX:

- One `auth_request` per location. A route protected by tinyauth (`auth_request /auth`) cannot also call Ailuros, and putting `auth_request /_ailuros` inside the `/auth` location does not work: NGINX skips access-phase handlers in subrequests, so the nested check never runs. This was confirmed with a throwaway NGINX container: the inner check answered 403 and the request was still served.
- The checks are only as broad as the routes they are attached to. In the homelab that prompted this, no route used the plain Ailuros filter and the one route using the chained filter was never inspected.

Goals:

- Per route, opt in: a route that lists the filter gets every request checked by Ailuros; tinyauth may be on the same route and both are enforced.
- Headers only (Ailuros's lite mode). Bodies are out of scope; they would need a body-capable forward-auth in Ailuros as well.
- No custom NGINX image. NGF owns its data plane Deployment; the module is mounted into the stock image.
- Ailuros needs no changes: the module speaks Ailuros's existing forward-auth contract.

Constraints found on NGF 2.8.0's data plane image (`ghcr.io/nginx/nginx-gateway-fabric/nginx:2.8.0`):

- NGINX 1.31.7, built `--with-compat`, on Alpine, running as uid 101.
- `/etc/nginx/nginx.conf` includes `/etc/nginx/main-includes/*.conf` before `events` and `http`, so a main-context `load_module` works.
- A module built `--with-compat` against the exact NGINX version loads without matching every configure flag; the version must match exactly, so each NGF release needs its own build.
- The image ships `libai_guardrails.so`, F5's guardrail filter for LLM chat traffic (OpenAI/Ollama message bodies sent to a guardrails scanner). It is not a general request-inspection hook, so it does not replace this module.

## Decisions

### A module of its own, not `auth_request`, not chaining in Ailuros

The module registers its own phase handler and its own directives, so it is enforced next to `auth_request` the way `auth_basic` and `auth_request` coexist.

Rejected:

- **Chaining inside Ailuros** (Ailuros allows, then calls tinyauth and relays its 401 or redirect). It needs no NGINX code, but it makes Ailuros responsible for calling the authentication server: a bug or a fail-open path there would skip authentication.
- **Internal-redirect tricks in snippets** (a check in one location, `try_files` to a named location with the other). They depend on the location names NGF generates.
- **A self-contained module with its own HTTP client to Ailuros.** Much more C in a security component. Reusing NGINX's proxy module through a subrequest keeps the module close to `ngx_http_auth_request_module` in size.
- **Envoy Gateway instead of NGF.** It runs Ailuros (bodies and gRPC included) and a separate external auth side by side with no custom code, but it means replacing the ingress.

### Directives

All are valid in `http`, `server` and `location`, and each level inherits from the one above.

| Directive | Default | Meaning |
|---|---|---|
| `ailuros_request <uri> \| off;` | `off` | Send a subrequest to `<uri>`, an internal location that `proxy_pass`es to Ailuros, before the request continues. |
| `ailuros_request_set $variable <value>;` | none | After the subrequest, set `$variable` to `<value>` (which may use the subrequest's upstream variables, e.g. `$upstream_http_x_ailuros_request_id`). Like `auth_request_set`. |
| `ailuros_request_unavailable closed \| open;` | `closed` | What happens when Ailuros gives no decision (below). |

The module also provides a read-only variable, `$ailuros_request_status`: the status the subrequest returned, empty before the check runs. It doubles as the marker that the request has been checked (see "Once per client request").

### Applying Ailuros's answer

The subrequest's response body is discarded, as with `auth_request`; a route's `error_page` can render JSON bodies, as Ailuros's shipped NGINX example does.

| Subrequest result | Request gets |
|---|---|
| 2xx | continues |
| 4xx from Ailuros (403 block, 400 malformed, 413) | that status |
| 503 from Ailuros (its own fail-closed decision) | 503, whatever `ailuros_request_unavailable` says |
| no decision: connection refused or timeout (the proxy's 502 or 504), any other 5xx, an unexpected 1xx or 3xx | `closed`: 503. `open`: continues, with a warning logged for every request let through. |

`AILUROS_FAILURE_MODE` covers model failures inside Ailuros; `ailuros_request_unavailable` covers Ailuros itself being unreachable or broken, as `failure_mode_allow` does for Envoy.

### Where it runs

The handler runs in NGINX's preaccess phase, after `limit_req` and `limit_conn`. NGINX runs a phase's handlers in reverse registration order and a dynamic module registers after the built-in ones, so the module moves its handler to the front of the preaccess handler array at postconfiguration time to run last in that phase. Per request:

1. `limit_req`, `limit_conn`
2. Ailuros
3. `allow` / `deny`, then `auth_request` (tinyauth)
4. the upstream

Consequences:

- `satisfy any` cannot make Ailuros optional, and an Ailuros allow cannot stand in for tinyauth: `satisfy` combines only access-phase handlers.
- Rate limits protect Ailuros.
- Scanners hitting tinyauth-protected routes are inspected even though tinyauth then redirects them to login.
- Requests that `allow` / `deny` would reject are still sent to Ailuros first (a model call each, and visible in Ailuros's logs).

### Once per client request

- The handler does nothing in subrequests (`r != r->main`), so tinyauth's `/auth` and `/_ailuros` itself are never checked, even when `ailuros_request` is inherited from `server`.
- Internal redirects after the check (`error_page`, named locations such as `@error401`) reuse the result: the module stores the outcome in its indexed variable, and request variables survive internal redirects (module contexts do not).
- `rewrite … last` happens before preaccess, so the check runs once, in the rewritten location.

### Delivery: a ConfigMap, no image

Each release ships, for every supported NGF version and architecture:

- `ngx_http_ailuros_module-<version>-ngf<ngf>-linux-<arch>.so`
- `ailuros-nginx-module-ngf<ngf>-<arch>.yaml`: a ConfigMap named `ailuros-nginx-module-ngf<ngf>` holding the `.so` as `binaryData` (a module of this size is far under the 1 MiB ConfigMap limit)
- `SHA256SUMS` and a build-provenance attestation per `.so`

Releases are immutable, so a published asset cannot be replaced.

Installation on NGF (example manifests in `deploy/ngf/`):

1. The ConfigMap, in the namespace where NGF runs the NGINX pods, as a remote kustomize resource from the release URL (or the release's YAML committed, if a direct file URL is not accepted).
2. An `nginx.patches` entry in NGF's Helm values mounting it read-only at `/usr/lib/nginx/modules/ailuros/` in the stock NGINX container. The ConfigMap name carries the NGF version, so a bump changes the pod template and rolls the pods.
3. One Gateway-wide `SnippetsPolicy`:
   - `main`: `load_module /usr/lib/nginx/modules/ailuros/ngx_http_ailuros_module.so;`
   - `http.server`: the internal `location = /_ailuros` that proxies to Ailuros's forward-auth port, defined once per server (per-route definitions collide when one server has several rules)
   - `http.server`: `add_header X-Ailuros-Request-Id $ailuros_request_id always;`, at server level so routes keep server-level headers (a location-level `add_header` drops them), and empty, so not sent, where Ailuros is not attached
4. A per-route `SnippetsFilter` in `http.server.location`: `ailuros-check` (closed) and `ailuros-check-open`:
   ```nginx
   ailuros_request /_ailuros;
   ailuros_request_set $ailuros_request_id $upstream_http_x_ailuros_request_id;
   ailuros_request_unavailable open;   # ailuros-check-open only
   ```

Failure behaviour:

- A directive used without the module loaded fails `nginx -t`; NGF rejects the config and the previous one keeps serving.
- An NGF bump without a matching `.so`: new NGINX pods fail `load_module` and crash-loop while the rolling update keeps the old pods serving. The gateway stalls; it never runs without Ailuros.

### Versions and CI

- The module has its own semver.
- `versions.json` lists each supported NGF version with its NGINX version and the NGINX source SHA-256. It drives the CI and release matrices and the README table; a test checks they agree. The latest three NGF releases are supported.
- Every workflow: `permissions: {}` with per-job grants, `harden-runner`, actions pinned to commit SHAs, `persist-credentials: false`.

| Workflow | When | What |
|---|---|---|
| `ci.yml` | push, PR | builds every NGF version × {amd64, arm64} inside the stock NGF image; cppcheck; behaviour, sanitizer and Ailuros contract suites; actionlint and zizmor |
| `kind.yml` | weekly, on demand, from release | NGF-in-kind end-to-end test |
| `security.yml` | push, PR, weekly | CodeQL (C/C++, manual build), Gitleaks, dependency review |
| `scorecard.yml` | weekly | OpenSSF Scorecard |
| `release.yml` | `v*` tag | tagged commit on `main`; full suite and kind; build; ConfigMaps, `SHA256SUMS`, provenance; immutable GitHub release with notes from the CHANGELOG |
| `ngf-watch.yml` | weekly | opens a PR adding a new NGF release to `versions.json` (NGINX version from the image's `nginx -V`, source SHA-256 from nginx.org) |

Dependabot covers GitHub Actions, the Go test harness, and the test stack's base images. A deploying repo should group the NGF chart bump with the module asset URL in one update (the homelab's Renovate does this).

## Testing

Go tests driving Docker, as in Ailuros:

1. **Build.** The NGINX source for each version is checked against the pinned SHA-256. The module is compiled inside the stock NGF image with `--with-compat` and `-Wall -Wextra -Werror`, then loaded with `nginx -t`. cppcheck over the source.
2. **Behaviour suite.** A compose stack: the stock NGF NGINX image with the `.so` mounted read-only and loaded from main-includes, as in production; a stub Ailuros whose answers each test sets; a tinyauth stub (401 and redirect); an echo upstream. Every stub records its calls. Cases:
   - every row of the answer table, under `closed` and `open`, including the fail-open warning;
   - ordering and stacking: an Ailuros block means tinyauth is never called; an Ailuros allow followed by a tinyauth 401 still redirects; `satisfy any` with `allow` skips neither; `limit_req` rejects before Ailuros;
   - exactly one Ailuros call across `error_page`, named locations and `rewrite … last`, and with the directive at `server` level, where `/auth` and `/_ailuros` must not trigger checks; `$ailuros_request_id` survives internal redirects;
   - GET, HEAD and POST: the body never reaches Ailuros and reaches the upstream intact; Ailuros sees the original method and URI; large headers, keepalive and concurrency; a client hanging up mid-check.
3. **Sanitizers.** The same suite against the same NGINX version built on Debian with AddressSanitizer and UBSan (they do not work on the NGF image's Alpine).
4. **Real Ailuros and NGF.**
   - Contract test: the published Ailuros image, pinned, with a minimal OpenAI-compatible stub model. It confirms the `X-Original-*` headers are read, allow and block, the request ID, and the `X-Forwarded-*` blanking.
   - NGF in kind (weekly, on demand, before each release): real NGF with the Helm `nginx.patches` mount, the main-context `SnippetsPolicy`, per-route `SnippetsFilter`s, Ailuros and the tinyauth stub; allow, block and stacking end to end. It uses its own temporary kubeconfig.

## Verification before implementation

These are assumptions the design rests on; the first plan task checks each in a throwaway setup and records the result here.

- NGINX loads a `.so` from a ConfigMap volume mount (`dlopen` needs a readable file; the kubelet does not mount ConfigMaps `noexec`).
- NGF accepts `load_module` in a `main`-context `SnippetsPolicy` and places it before `http`.
- NGF rejects, and reports, a configuration that uses `ailuros_request` without the module loaded, and keeps serving the previous one.
- kustomize and Flux accept a ConfigMap as a remote file URL (a GitHub release asset).
- Moving the handler to the front of the preaccess array makes it run after `limit_req`, and request variables survive `error_page` and named-location redirects.

### Results (2026-10-21)

Checked on kind (v0.33.0) with NGF 2.8.0 (Helm chart 2.8.0, Gateway API CRDs v1.6.3), using the stub module (`ailuros on;` adds `X-Ailuros-Module: enabled`), built inside the stock image: 126,824 bytes.

- **`.so` from a ConfigMap mount: verified.** NGINX `dlopen`ed `/usr/lib/nginx/modules/ailuros/ngx_http_ailuros_module.so` from the ConfigMap volume (mounted `ro`, not `noexec`; the file is a symlink into the kubelet's `..data` directory) and a request through the Gateway returned the module's header. Ruling: the mount path in the global constraints stands.
- **`load_module` in a `main`-context `SnippetsPolicy`: verified.** NGF writes the snippet to `/etc/nginx/includes/SnippetsPolicy_main_default-<name>.conf` and includes it from `/etc/nginx/main-includes/main.conf`, which `nginx.conf` includes before `events` and `http` (after the stock `load_module modules/ngx_http_js_module.so;`). The policy reports `Accepted` and `Programmed`. Two corrections to the install steps, now applied above:
  - Snippets are off by default; the Helm value is `nginxGateway.snippets.enable: true` (it adds `--snippets` to the controller). Without it the route's `SnippetsFilter` is "Not found" and the route serves 500.
  - `nginx.patches` with `type: StrategicMerge` is merged by list index, not by name, and the Deployment is rejected (volumes and mounts overwrite NGF's own). Use `type: JSONPatch` with `add` operations on `/spec/template/spec/volumes/-` and `/spec/template/spec/containers/0/volumeMounts/-` (the `nginx` container is first). The working values are in `deploy/ngf/` once Task 8 lands them.
- **Directive without the module: verified (rejected, reported, old config serves).** With the `SnippetsPolicy` deleted and the route filter still using `ailuros on;`, NGF's `nginx -t` failed (`unknown directive "ailuros"`), the agent rolled back, and requests still returned 200 with the header. The failure shows as the Gateway's `Programmed=False` condition (reason `Invalid`, message carrying the `nginx -t` output) and in the NGF controller log (`Failed to update NGINX configuration`); the data plane log has "Config apply failed, rollback successful". The `HTTPRoute` and `SnippetsFilter` conditions stay green, so alert on the Gateway's `Programmed` condition. Re-applying the policy cleared it.
- **Remote file URL in kustomize: verified for `kubectl kustomize`; Flux not exercised.** kustomize v5.8.1 (bundled with kubectl v1.37.1) accepted an `https://` file as a `resources:` entry for both a `raw.githubusercontent.com` file and a GitHub release asset (`github.com/<org>/<repo>/releases/download/<tag>/<file>.yaml`, which redirects to a signed URL). A URL that does not exist (404) is reported as "URL is a git repository", which is misleading but harmless. Flux's `Kustomization` documentation says nothing about remote file resources; kustomize-controller (v1.9.6 in Flux v2.9.6) builds with the same library and only blocks remote bases when started with `--no-remote-bases`. Ruling: the remote asset URL is the documented option, with committing the release's ConfigMap YAML as the fallback for clusters that disallow remote bases or when Flux's build fails; the design's wording already says so.
- **Preaccess ordering and variables across redirects: not checked here.** Both need the real handler; they stay in the behaviour suite (Testing, item 2).

## Limitations and trade-offs

- Headers only. Bodies would need a body-capable forward-auth in Ailuros and body buffering at the gateway.
- One build per NGF version: an NGF upgrade needs the matching release first.
- Requests that `allow` / `deny` would reject still cost an Ailuros call.
- NGF only: a plain NGINX can load the module the same way, but only NGF's image is built and tested.

## History

- 2026-10-21: Design approved: own preaccess handler next to `auth_request`, configurable `ailuros_request_unavailable` (default closed), headers only, delivered by ConfigMap into NGF's stock image.
