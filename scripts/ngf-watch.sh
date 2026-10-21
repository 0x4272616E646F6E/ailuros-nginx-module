#!/bin/sh
# Bring versions.json up to the newest three NGF releases (read-only on
# GitHub; the workflow opens the PR). For each NGF release not yet listed it
# reads the NGINX version from the stock data plane image's `nginx -V`, fetches
# that NGINX source tarball and its signature from nginx.org, verifies the
# signature against the pinned keys (scripts/verify-nginx-source.sh) and
# records the tarball's SHA-256. Rewrites $VERSIONS_FILE (default versions.json)
# only if something changed, and prints the added NGF versions, one per line.
#
# NGF_TAGS (optional): newline-separated release tags to use instead of
# querying `gh api repos/nginx/nginx-gateway-fabric/releases`.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
file=${VERSIONS_FILE:-versions.json}
image=${NGF_IMAGE:-ghcr.io/nginx/nginx-gateway-fabric/nginx}
keep=3
semver='^[0-9]+\.[0-9]+\.[0-9]+$'

if [ -n "${NGF_TAGS:-}" ]; then
	tags=$NGF_TAGS
else
	tags=$(gh api --paginate repos/nginx/nginx-gateway-fabric/releases \
		--jq '.[] | select(.draft | not) | select(.prerelease | not) | .tag_name')
fi
# Only plain vX.Y.Z tags; the strings are used in image names and URLs.
listed=$(printf '%s\n' "$tags" | sed -n 's/^v\{0,1\}\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)$/\1/p')
[ -n "$listed" ] || { echo "$0: no NGF releases found" >&2; exit 1; }

existing=$(jq -r '.[].ngf' "$file")
target=$(printf '%s\n%s\n' "$listed" "$existing" | sed '/^$/d' | sort -u -t. -k1,1nr -k2,2nr -k3,3nr | head -n "$keep")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp "$file" "$work/rows.json"
added=""

for ngf in $target; do
	printf '%s\n' "$ngf" | grep -Eq "$semver"
	if printf '%s\n' "$existing" | grep -qx "$ngf"; then continue; fi

	echo "== NGF $ngf is new" >&2
	out=$(docker run --rm --entrypoint nginx "$image:$ngf" -V 2>&1) || {
		echo "$0: cannot run $image:$ngf (image not published yet?); skipping" >&2
		continue
	}
	nginx=$(printf '%s\n' "$out" | sed -n 's|^nginx version: nginx/\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)$|\1|p' | head -n 1)
	[ -n "$nginx" ] || { echo "$0: no NGINX version in nginx -V output of $image:$ngf" >&2; exit 1; }

	dir="$work/nginx-$nginx"
	mkdir -p "$dir"
	curl -fsSL --retry 3 -o "$dir/nginx.tar.gz" "https://nginx.org/download/nginx-$nginx.tar.gz"
	curl -fsSL --retry 3 -o "$dir/nginx.tar.gz.asc" "https://nginx.org/download/nginx-$nginx.tar.gz.asc"
	sha=$("$here/verify-nginx-source.sh" "$dir/nginx.tar.gz" "$dir/nginx.tar.gz.asc")
	printf '%s\n' "$sha" | grep -Eq '^[0-9a-f]{64}$'

	jq --arg ngf "$ngf" --arg nginx "$nginx" --arg sha "$sha" \
		'. + [{ngf: $ngf, nginx: $nginx, nginx_sha256: $sha}]' "$work/rows.json" >"$work/next.json"
	mv "$work/next.json" "$work/rows.json"
	added="$added$ngf
"
done

# Newest three only, newest first.
jq --argjson keep "$keep" 'sort_by(.ngf | split(".") | map(tonumber)) | reverse | .[:$keep]' "$work/rows.json" >"$work/final.json"
if ! cmp -s "$work/final.json" "$file"; then
	cp "$work/final.json" "$file"
fi
printf '%s' "$added"
