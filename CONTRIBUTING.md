# Contributing to the decision-waf NGINX module

Bug reports, fixes, tests and documentation are welcome. For larger changes,
open an issue first. Report security issues privately as described in
[SECURITY.md](SECURITY.md).

## Development setup

You need Docker with buildx, GNU Make, `jq` and Go (the version in
`test/go.mod`). `make test-kind` also needs kind, kubectl and Helm, and
network access for the NGF chart and images. The C module is compiled inside
NGF's stock NGINX image with `--with-compat`; no host NGINX is needed.

## Tests

`NGF` selects the row of [versions.json](versions.json) under test (unset, the
first row).

| Command | What it does |
|---|---|
| `make test NGF=2.8.0` | all Go suites: behaviour, contract and repo (below) |
| `make test-asan NGF=2.8.0` | the behaviour suite against the module and NGINX built with AddressSanitizer and UBSan on Debian |
| `make test-contract NGF=2.8.0` | the module against the published decision-waf (pinned by digest) and a stub model |
| `make test-kind NGF=2.8.0` | NGF in a throwaway kind cluster (`decision-waf-module-it`) with `deploy/ngf/` applied unmodified; it uses a temporary kubeconfig and never yours |
| `make cppcheck` | static analysis of `module/src` |
| `make verify NGF=2.8.0` | compiles with `-Wall -Wextra -Werror` and loads the module with `nginx -t` |
| `make build-host NGF=2.8.0` | the host-compiler build CodeQL traces |

The suites under `test/`:

- `repo/`: the repository itself: `versions.json`, the README's versions table
  and relative links, workflows, scripts, ConfigMap generation.
- `behaviour/`: a compose stack with NGF's NGINX image, the `.so` mounted as
  in production, a stub decision-waf, a tinyauth stub and an echo upstream.
- `contract/`: the real decision-waf image with a stub model.
- `kind/` (build tag `kind`): NGF end to end.

`make test` and `make test-contract` need the module built first: `make build
NGF=2.8.0 ARCH=<host arch> VERSION=dev`, or set `DECISION_WAF_MODULE_SO`. Run
`make test`, `make cppcheck` and `make verify` before opening a pull request,
and `make test-asan` when changing the C code. Check afterwards that no
compose projects or kind clusters are left behind (`docker compose ls`,
`kind get clusters`).

## Pull requests

- Keep a pull request to one change, with tests for new behavior and for
  every bug fixed (a test that fails without the fix).
- Write commit messages that say what changed and why, in plain sentences.
  Do not add `Co-Authored-By` trailers for AI tools.
- Add a line under `Unreleased` in [CHANGELOG.md](CHANGELOG.md) for anything
  a user would notice. The release workflow takes a release's notes from its
  `## [X.Y.Z]` section.
- Update the docs in the same pull request when behavior or configuration
  changes, including the README's versions table. If a design decision
  changes, add a dated line to the History of
  [docs/design/module.md](docs/design/module.md).

CI must pass before a pull request is merged.

## Releasing

Move the `Unreleased` entries under a new `## [X.Y.Z] - date` heading, merge
to `main`, then tag `vX.Y.Z` on that commit. The release workflow checks the
tag is on `main`, that the changelog has a section for it, runs the full suite
and the kind test, builds every NGF version and architecture, and publishes an
immutable release.

## Repository settings

For maintainers; these are GitHub settings, not files:

- Enable **immutable releases**.
- Add a **tag ruleset** restricting creation of `v*` tags.
- Enable **Allow GitHub Actions to create pull requests**, which `ngf-watch`
  needs to open its pull requests.
- Pull requests opened by `ngf-watch` do not trigger CI automatically (they
  are created with the workflow token). Run CI and kind on the branch with
  `workflow_dispatch` (or close and reopen the pull request) and require them
  to pass before merging.
- The release's on-main check fetches `main` without credentials, so it
  assumes the repository is public.

## Licence of contributions

The module is licensed under the [Apache License, Version 2.0](LICENSE). Under
section 5 of that licence, anything you submit for inclusion is licensed under
the same terms, with no separate agreement or sign-off needed. Only submit work
you have the right to license this way.
