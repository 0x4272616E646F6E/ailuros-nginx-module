#!/bin/sh
# Compile the module with the host compiler (no container), so a code scanner
# that traces the build (CodeQL) sees the C compile. Not a release build: the
# shipped .so always comes from `make build` inside the stock NGF image.
#   scripts/build-host.sh <ngf-version>      (WORKDIR defaults to out/host)
# Needs: jq, curl, tar, sha256sum, a C compiler, make, libpcre2-dev,
# libssl-dev, zlib1g-dev.
set -eu
ngf="${1:?usage: build-host.sh <ngf-version>}"
root="$(cd "$(dirname "$0")/.." && pwd)"
work="${WORKDIR:-$root/out/host}"
row="$(jq -c --arg ngf "$ngf" '.[] | select(.ngf == $ngf)' "$root/versions.json")"
[ -n "$row" ] || { echo "NGF $ngf not found in versions.json" >&2; exit 1; }
nginx="$(printf '%s' "$row" | jq -r .nginx)"
want="$(printf '%s' "$row" | jq -r .nginx_sha256)"

rm -rf "$work"; mkdir -p "$work"
curl -fsSLo "$work/nginx.tar.gz" "https://nginx.org/download/nginx-$nginx.tar.gz"
got="$(sha256sum "$work/nginx.tar.gz" | cut -d' ' -f1)"
if [ "$got" != "$want" ]; then
    echo "ERROR: SHA-256 mismatch for nginx-$nginx.tar.gz: expected $want, got $got" >&2
    exit 1
fi
mkdir "$work/src"
tar -xzf "$work/nginx.tar.gz" -C "$work/src" --strip-components=1
cd "$work/src"
./configure --with-compat --add-dynamic-module="$root/module" \
    --with-cc-opt='-Wall -Wextra -Werror'
make modules
echo "wrote $work/src/objs/ngx_http_decision_waf_module.so"
