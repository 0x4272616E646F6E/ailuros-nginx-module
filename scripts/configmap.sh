#!/bin/sh
# Write the module's ConfigMap to stdout.
#   scripts/configmap.sh <so> <ngf> <arch> [namespace]
# The ConfigMap is named decision-waf-nginx-module-ngf<ngf> and holds the .so as
# binaryData key ngx_http_decision_waf_module.so. <arch> is recorded in the
# decision-waf.dev/arch label. The module version for the app.kubernetes.io/version
# label is $DECISION_WAF_VERSION, else taken from a release-named <so>
# (ngx_http_decision_waf_module-<version>-ngf<ngf>-linux-<arch>.so), else "dev".
# Exits non-zero, writing nothing, if the YAML would exceed 1 MiB (the
# Kubernetes limit on a ConfigMap).
set -eu

if [ $# -lt 3 ] || [ $# -gt 4 ]; then
	echo "usage: $0 <so> <ngf> <arch> [namespace]" >&2
	exit 2
fi
so=$1 ngf=$2 arch=$3 ns=${4:-}

[ -r "$so" ] || { echo "$0: cannot read $so" >&2; exit 1; }
case $arch in amd64 | arm64) ;; *) echo "$0: unsupported architecture $arch" >&2; exit 1 ;; esac
case $ngf in *[!0-9.]* | '') echo "$0: invalid NGF version $ngf" >&2; exit 1 ;; esac

version=${DECISION_WAF_VERSION:-}
if [ -z "$version" ]; then
	version=$(basename "$so" | sed -n 's/^ngx_http_decision_waf_module-\(.*\)-ngf[0-9.]*-linux-[a-z0-9]*\.so$/\1/p')
fi
version=${version:-dev}
case $version in *[!A-Za-z0-9._-]* | [!A-Za-z0-9]* | *[!A-Za-z0-9]) echo "$0: $version is not a valid label value" >&2; exit 1 ;; esac
[ ${#version} -le 63 ] || { echo "$0: version label too long" >&2; exit 1; }

# base64 differs between GNU (wraps at 76) and BSD; strip newlines either way.
data=$(base64 <"$so" | tr -d '\n')

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
{
	printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: decision-waf-nginx-module-ngf%s\n' "$ngf"
	[ -z "$ns" ] || printf '  namespace: %s\n' "$ns"
	printf '  labels:\n    app.kubernetes.io/name: decision-waf-nginx-module\n    app.kubernetes.io/version: %s\n    decision-waf.dev/arch: %s\n' "$version" "$arch"
	printf 'binaryData:\n  ngx_http_decision_waf_module.so: %s\n' "$data"
} >"$tmp"
size=$(wc -c <"$tmp" | tr -d ' ')
if [ "$size" -gt 1048576 ]; then
	echo "$0: ConfigMap would be $size bytes, over the 1 MiB Kubernetes limit" >&2
	exit 1
fi
cat "$tmp"
