#!/bin/sh
# Write the module's ConfigMap to stdout.
#   scripts/configmap.sh <so> <ngf> <arch> [namespace]
# The ConfigMap is named ailuros-nginx-module-ngf<ngf> and holds the .so as
# binaryData key ngx_http_ailuros_module.so. <arch> is recorded in a label.
set -eu

if [ $# -lt 3 ] || [ $# -gt 4 ]; then
	echo "usage: $0 <so> <ngf> <arch> [namespace]" >&2
	exit 2
fi
so=$1 ngf=$2 arch=$3 ns=${4:-}

[ -r "$so" ] || { echo "$0: cannot read $so" >&2; exit 1; }
case $arch in amd64 | arm64) ;; *) echo "$0: unsupported architecture $arch" >&2; exit 1 ;; esac
case $ngf in *[!0-9.]* | '') echo "$0: invalid NGF version $ngf" >&2; exit 1 ;; esac

# base64 differs between GNU (wraps at 76) and BSD; strip newlines either way.
data=$(base64 <"$so" | tr -d '\n')

printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ailuros-nginx-module-ngf%s\n' "$ngf"
[ -z "$ns" ] || printf '  namespace: %s\n' "$ns"
printf '  labels:\n    app.kubernetes.io/name: ailuros-nginx-module\n    ailuros-nginx-module/arch: %s\n' "$arch"
printf 'binaryData:\n  ngx_http_ailuros_module.so: %s\n' "$data"
