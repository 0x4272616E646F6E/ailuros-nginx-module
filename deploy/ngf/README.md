# Installing the module on NGINX Gateway Fabric

Example manifests for NGF 2.8.0, applied unmodified by `make test-kind`
(`test/kind/kind_test.go`). The module is a ConfigMap mounted into NGF's stock
NGINX image; no custom image is run.

| File | What it does |
|---|---|
| `helm-values-patch.yaml` | Helm values: enables snippets and mounts the module ConfigMap into the NGINX container at `/usr/lib/nginx/modules/ailuros/` |
| `snippetspolicy.yaml` | One Gateway `SnippetsPolicy`: `load_module`, the internal `/_ailuros` location that proxies to Ailuros, and the `X-Ailuros-Request-Id` response header |
| `snippetsfilters.yaml` | `ailuros-check` (fail closed) and `ailuros-check-open` (fail open), attached per route |
| `kustomization.yaml` | `kubectl apply -k` for the policy and the filters |

## Install

1. **Gateway API CRDs and Ailuros.** Install the Gateway API CRDs NGF 2.8.0
   documents, and run Ailuros with forward-auth on port 9002 behind a Service.
   The Service must exist before you apply the policy: `proxy_pass` has a
   literal host, which NGINX resolves when it loads the configuration, so
   without the Service `nginx -t` fails and NGF rejects the Gateway's config.
2. **The module ConfigMap**, in the namespace where NGF runs the NGINX pods (the
   Gateway's namespace), before the pods start. Use the release asset
   `ailuros-nginx-module-ngf2.8.0-<arch>.yaml` (match the cluster's CPU
   architecture), as a remote kustomize resource or committed next to these
   files. To make one from a local build:
   `scripts/configmap.sh out/ngx_http_ailuros_module-<version>-ngf2.8.0-linux-<arch>.so 2.8.0 <arch> <namespace> | kubectl apply -f -`.
   The ConfigMap is named `ailuros-nginx-module-ngf2.8.0`; the name carries the
   NGF version, so an NGF bump changes the pod template and rolls the pods.
3. **NGF**, with `helm-values-patch.yaml` merged into your values.
4. **Edit `snippetspolicy.yaml`**: `metadata.namespace` and `targetRefs[0].name`
   (the Gateway), and the `proxy_pass` host `ailuros.<namespace>.svc.cluster.local`
   (the Ailuros Service). Edit the namespace in `snippetsfilters.yaml` to match
   the routes' namespace (SnippetsFilters are referenced from the route's own
   namespace). Then `kubectl apply -k deploy/ngf`.
5. **List a filter on each route** that should be checked:
   ```yaml
   filters:
     - type: ExtensionRef
       extensionRef: {group: gateway.nginx.org, kind: SnippetsFilter, name: ailuros-check}
   ```
   A route can also list an `auth_request` filter such as tinyauth's: Ailuros
   runs first (preaccess phase), then `auth_request`.

Routes without a filter are never sent to Ailuros and carry no
`X-Ailuros-Request-Id`.

## Things to check

- **`containers/0` must be the nginx container.** The Helm patch adds the
  mount to `/spec/template/spec/containers/0/volumeMounts/-`, by index. On NGF
  2.8.0 container 0 is `nginx` (the kind test asserts it). Re-check on every
  NGF upgrade: `kubectl get deploy <gateway>-nginx -o jsonpath='{.spec.template.spec.containers[0].name}'`.
- **The patch must be `type: JSONPatch`.** `StrategicMerge` merges lists by
  index on NGF 2.8.0 and the Deployment is rejected.
- **Snippets are off by default**: `nginxGateway.snippets.enable: true`.
- **One build per NGF version.** Use the ConfigMap for the NGF version you run;
  an NGF upgrade needs the matching release first.
- **Alert on the Gateway.** If the module is not loaded (wrong ConfigMap name,
  missing mount, policy removed), `nginx -t` fails, NGF keeps the previous
  configuration and reports it on the Gateway only: `Programmed=False`, reason
  `Invalid`, with the `nginx -t` output. Routes and filters stay green.
- **Why `ailuros_request_set` is in the policy.** `$ailuros_request_id` exists
  only once some `ailuros_request_set` declares it, and `add_header` with an
  unknown variable fails `nginx -t`. The policy declares it at server level
  (it does nothing where `ailuros_request` is off), so the policy is valid
  before any route lists a filter.
- **`/_ailuros` stays minimal.** It must be `internal` and answer itself: the
  module refuses answers from non-internal or named locations and from any
  internal redirect inside the subrequest, and `error_page` does not apply
  there, so do not add `rewrite` or `try_files`. The timeouts (1s connect,
  1500ms read) are Ailuros's default 1s classifier timeout plus 500ms.
- **`add_header X-Ailuros-Request-Id` is at server level, once.** A
  location-level `add_header` would drop the server-level headers of every
  route.

- **Upstream cannot spoof the header.** The policy has a server-level
  `proxy_hide_header X-Ailuros-Request-Id;`, so an upstream app that sends the
  header is not passed on; only the module-set value is. A location-level
  `proxy_hide_header` or `add_header` (for example from a
  `ResponseHeaderModifier` on a checked route, which NGF renders as a
  location-level `add_header`) replaces the server-level ones: the request-id
  header would be dropped and a spoofed one would no longer be hidden.

## `ailuros_request` at server level

The filters use `ailuros_request` at location level on purpose. If you set it
at server level instead (all routes of a server checked), `error_page` targets
that reach the preaccess phase reuse the original decision rather than asking
again. A per-route `SnippetsFilter` (location context) avoids that, and it is
the supported way to choose which routes are checked.
