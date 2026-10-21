# Ailuros NGINX module

A dynamic NGINX module that asks [Ailuros](https://github.com/0x4272616E646F6E/ailuros)
to allow or block each request on the routes it is attached to, alongside any
`auth_request` check (such as tinyauth) on the same route. It is built for
[NGINX Gateway Fabric](https://github.com/nginx/nginx-gateway-fabric) (NGF)
and loaded into NGF's stock NGINX image from a ConfigMap: no custom image is
built or run.

> Status: early development. The first release is 0.1.0 (see
> [CHANGELOG.md](CHANGELOG.md)). Headers only: request bodies are not sent to
> Ailuros.

## Why a module

Ailuros already serves NGINX through HTTP forward-auth on port 9002. On NGF
that runs into two limits of stock NGINX:

- A location has one `auth_request`. A route protected by tinyauth cannot also
  call Ailuros, and nesting the Ailuros check inside tinyauth's `/auth`
  location does not work: NGINX skips access-phase handlers in subrequests.
- A check only covers the routes it is attached to.

The module has its own phase handler and directives, so it is enforced next to
`auth_request`, and a route opts in by listing one `SnippetsFilter`. Ailuros
needs no changes. The design, with the alternatives that were turned down, is
in [docs/design/module.md](docs/design/module.md).

## Supported NGF versions

Each NGF release needs its own build: the module must match the NGINX version
inside NGF's data plane image exactly.

| NGF | NGINX |
|---|---|
| 2.8.0 | 1.31.7 |

The table mirrors [versions.json](versions.json) and a test checks that they
agree. Release assets are built for `amd64` and `arm64`.

## Directives

All are valid in `http`, `server` and `location`, and inherit downwards.

| Directive | Default | Meaning |
|---|---|---|
| `ailuros_request <uri> \| off;` | `off` | Ask Ailuros through a subrequest to `<uri>` before the request continues |
| `ailuros_request_set $variable <value>;` | none | After the check, set `$variable` (the value may use `$upstream_http_*` of the subrequest) |
| `ailuros_request_unavailable closed \| open;` | `closed` | What happens when Ailuros gives no decision |

The read-only variable `$ailuros_request_status` is the status the subrequest
returned (empty before the check runs).

The `ailuros_request` location must be `internal`, not a named location, and
must answer itself. Do not use `rewrite`, `try_files`, `X-Accel-Redirect` or
any other internal redirect inside it: a redirected answer is treated as no
decision. `error_page` is not applied in it, so a proxy 502 or 504 stays one.
The shipped location sets `proxy_ignore_headers X-Accel-*`.

### How Ailuros's answer is applied

| Subrequest result | The request gets |
|---|---|
| 2xx | continues |
| 400, 403 or 413 from Ailuros | that status |
| 503 from Ailuros (its own fail-closed decision) | 503, whatever `ailuros_request_unavailable` says |
| no decision: connection refused or timeout (the proxy's 502 or 504), any other 4xx or 5xx, an answer from a location that is not internal, a redirect inside the subrequest | `closed`: 503. `open`: continues |

A fail-open pass logs a warning every time; a closed no-decision logs an
error. Ailuros's own 503 is never overridden. The full rules are in
[docs/design/module.md](docs/design/module.md#applying-ailuross-answer).

### Where it runs

In NGINX's preaccess phase, after `limit_req` and `limit_conn`, once per
client request (never in subrequests):

1. `limit_req`, `limit_conn`
2. Ailuros
3. `allow` / `deny`, then `auth_request` (tinyauth)
4. the upstream

`satisfy any` never skips Ailuros, because it runs in preaccess. `allow` /
`deny` and `auth_request` still combine as NGINX defines: with
`satisfy any; allow all;` tinyauth can be skipped. That is NGINX's behaviour,
not the module's.

## Install on NGF

The manifests are in [deploy/ngf/](deploy/ngf/README.md), which has the full
list of things to check. In short:

1. Run Ailuros with forward-auth on port 9002 behind a Service. The Service
   must exist before the policy is applied: `proxy_pass` has a literal host
   that NGINX resolves when it loads the configuration.
2. Create the module ConfigMap in the namespace where NGF runs the NGINX pods
   (the Gateway's namespace). Use the release asset
   `ailuros-nginx-module-ngf<ngf>-<arch>.yaml` for your CPU architecture. The
   manifest is 100-175 KB, close to the
   cap (256 KiB) on the `last-applied-configuration` annotation that
   client-side `kubectl apply` stores a copy of the object in, so use
   `kubectl apply --server-side` (Flux applies server-side already):
   ```sh
   kubectl apply --server-side -n <gateway-namespace> -f ailuros-nginx-module-ngf2.8.0-amd64.yaml
   ```
   A remote kustomize `resources:` entry pointing at the release asset works
   with `kubectl kustomize`; Flux remote file resources are unverified, so
   committing the release's ConfigMap YAML is the fallback.
3. Merge [helm-values-patch.yaml](deploy/ngf/helm-values-patch.yaml) into the
   NGF Helm values. It sets `nginxGateway.snippets.enable: true` and mounts
   the ConfigMap at `/usr/lib/nginx/modules/ailuros/` with a `JSONPatch` (not
   `StrategicMerge`) that addresses the nginx container as `containers/0`.
   Re-check that index on every NGF upgrade. The manifests in `deploy/ngf`
   target the newest supported NGF release (the first row of
   `versions.json`; 2.8.0 today). For an older supported release, use that
   release's chart version and ConfigMap asset and change the version in the
   ConfigMap name in `helm-values-patch.yaml` to match (`make test-kind NGF=<ngf>`
   does exactly that substitution); see [deploy/ngf/README.md](deploy/ngf/README.md).
4. Edit [snippetspolicy.yaml](deploy/ngf/snippetspolicy.yaml) (Gateway name and
   namespace, the Ailuros Service host) and
   [snippetsfilters.yaml](deploy/ngf/snippetsfilters.yaml) (the routes'
   namespace), then `kubectl apply -k deploy/ngf`. The policy loads the module,
   defines the internal `/_ailuros` location once per server, and sets
   `X-Ailuros-Request-Id` on responses. It also has
   `proxy_hide_header X-Ailuros-Request-Id;` at server level so an upstream
   cannot spoof the header.
5. List a filter on each route that should be checked:
   ```yaml
   filters:
     - type: ExtensionRef
       extensionRef: {group: gateway.nginx.org, kind: SnippetsFilter, name: ailuros-check}
   ```
   `ailuros-check` fails closed; `ailuros-check-open` fails open. A route can
   also list an `auth_request` filter such as tinyauth's: Ailuros runs first,
   then `auth_request`, and both are enforced. Routes without a filter are not
   checked.

The filters set `ailuros_request` at location level on purpose. With
`ailuros_request` at server level, an `error_page` target that reaches the
preaccess phase reuses the original decision, so a block stays a block, even
for the error page. If you do set it at server level, render block pages with
`return`, or set `ailuros_request off;` in the error page's location. The
per-route `SnippetsFilter` avoids this and is the supported way to choose
which routes are checked.

## Upgrade

Module release first, then the NGF bump, in one change:

1. Wait for (or build) a module release that lists the new NGF version in its
   assets. The weekly `ngf-watch` workflow proposes new versions.
2. In one pull request, update the NGF chart version and the module ConfigMap
   (its name carries the NGF version, so the pod template changes and the pods
   roll), and re-check `containers/0`.

An NGF bump without a matching module release makes new NGINX pods fail
`load_module` and crash-loop, while the old pods keep serving. The gateway
stalls; it never runs without Ailuros.

## Verifying release assets

Each release has `SHA256SUMS` and a build-provenance attestation per file.
Download the assets you use and the checksums, then:

```sh
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify ngx_http_ailuros_module-v0.1.0-ngf2.8.0-linux-amd64.so \
  --repo 0x4272616E646F6E/ailuros-nginx-module
```

On macOS use `shasum -a 256 -c SHA256SUMS --ignore-missing`. A release is
published with all its assets in one step and is meant to be immutable, so a
published asset is not replaced; see [CONTRIBUTING.md](CONTRIBUTING.md) for
the repository setting that enforces it.

## Troubleshooting

- **The module is not loaded** (wrong ConfigMap name, missing mount, policy
  removed): `nginx -t` fails, NGF rejects the configuration and keeps serving
  the old one. The failure is reported on the Gateway only
  (`Programmed=False`, reason `Invalid`, with the `nginx -t` output); the
  routes and filters stay green. Alert on the Gateway's `Programmed`
  condition.
- **New NGINX pods crash-loop after an NGF bump**: there is no module build
  for that NGF version, or the ConfigMap name still names the old one. Roll
  back the bump or install the matching release; the old pods keep serving.
- **A route serves 500 and its filter is "Not found"**: snippets are off.
  Set `nginxGateway.snippets.enable: true`.
- **The Deployment is rejected**: the Helm patch is `StrategicMerge`; it must
  be `JSONPatch`. Also check that `containers/0` is the nginx container:
  `kubectl get deploy <gateway>-nginx -o jsonpath='{.spec.template.spec.containers[0].name}'`.
- **`nginx -t` fails on `proxy_pass`**: the Ailuros Service does not exist yet.
- **`kubectl apply` of the ConfigMap fails or is rejected as too large**: use
  `--server-side`.
- **Fail-open warnings** in the NGINX error log (`gave no decision ... letting
  the request through`): Ailuros is unreachable, slow (the shipped timeouts are
  1s connect and 1500ms read) or answering something other than 2xx, 400, 403,
  413 or 503. Every pass logs one. Closed no-decisions log an error and the
  client gets 503.
- **An error log line saying the location "was redirected internally" or "is
  not internal"**: the `ailuros_request` location breaks the rules above; the
  answer is treated as no decision.

## Development

Requires Docker with buildx, GNU Make and `jq`; Go for the test harness.
See [CONTRIBUTING.md](CONTRIBUTING.md) for the test layers.

`ARCH` defaults to `amd64`. The test suites run the module in containers of
the host's architecture (`make test` loads the host-architecture `.so`), so on
an arm64 host (Apple silicon, Linux aarch64) build with `ARCH=arm64` before
testing. A shell-portable choice: `ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')`.

| Command | What it does |
|---|---|
| `make build NGF=2.8.0 ARCH=<host arch> VERSION=dev` | builds the `.so` inside NGF's stock image into `out/` (build this before `make test`) |
| `make verify NGF=2.8.0` | compiles with `-Werror` and loads it with `nginx -t` |
| `make test` | repo, behaviour and contract suites |
| `make test-asan` | the behaviour suite against an AddressSanitizer build |
| `make test-contract` | the module against the published Ailuros |
| `make test-kind` | NGF in a throwaway kind cluster |
| `make cppcheck` | static analysis of the C source |
| `make release-assets VERSION=v0.1.0` | every `.so`, ConfigMap and `SHA256SUMS` |

## Security

Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## Licence

[Apache License, Version 2.0](LICENSE).
