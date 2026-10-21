# Security policy

The Ailuros NGINX module sits in the request path and enforces Ailuros's
decision, so a way to get a request past it — an answer it treats as an allow
that it should not, a failure that opens when it should close, a memory-safety
bug in the C code — is a security issue, not an ordinary bug.

## Reporting a vulnerability

Please report privately through GitHub's **[private vulnerability
reporting](https://github.com/0x4272616E646F6E/ailuros-nginx-module/security/advisories/new)**
(Security → Report a vulnerability). Do not open a public issue.

Include what you can: the module version, the NGF and NGINX versions, the
relevant NGINX configuration (the `ailuros_request` location and the route's
filters), and a request or script that reproduces it, with what the module
logged.

You should get an acknowledgement within a few days. Fixes are released as a
new version, with a GitHub security advisory once the fix is available.

## Supported versions

The module is pre-1.0: only the latest release gets security fixes, for the
NGF versions listed in the README.

## Verifying releases

Release assets have SHA-256 checksums and SLSA build provenance; see
"Verifying release assets" in the [README](README.md#verifying-release-assets).
