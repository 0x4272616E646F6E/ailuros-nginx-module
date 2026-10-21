#!/bin/sh
# Check an NGINX source tarball against its detached signature using only the
# keys pinned in .github/nginx-keys/ (override the directory with
# NGINX_KEYS_DIR). Prints the tarball's SHA-256 on success; exits non-zero if
# the pinned keys do not match FINGERPRINTS or the signature is missing, bad,
# expired, revoked or made by any other key.
#   scripts/verify-nginx-source.sh <nginx-X.Y.Z.tar.gz> <nginx-X.Y.Z.tar.gz.asc>
set -eu

[ $# -eq 2 ] || { echo "usage: $0 <tarball> <signature>" >&2; exit 2; }
tar=$1 sig=$2
here=$(cd "$(dirname "$0")" && pwd)
keys=${NGINX_KEYS_DIR:-$here/../.github/nginx-keys}
[ -r "$tar" ] && [ -r "$sig" ] || { echo "$0: cannot read $tar or $sig" >&2; exit 1; }
command -v gpg >/dev/null || { echo "$0: gpg is required" >&2; exit 1; }

pinned=$(awk '!/^#/ && NF { print $1 }' "$keys/FINGERPRINTS" | sort)
[ -n "$pinned" ] || { echo "$0: no pinned fingerprints in $keys/FINGERPRINTS" >&2; exit 1; }

GNUPGHOME=$(mktemp -d)
export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT
chmod 700 "$GNUPGHOME"

for k in "$keys"/*.key; do gpg --batch --quiet --import "$k" 2>/dev/null; done
have=$(gpg --batch --with-colons --list-keys | awk -F: '$1 == "pub" { p = 1; next } $1 == "fpr" && p { print $10; p = 0 }' | sort)
if [ "$have" != "$pinned" ]; then
	echo "$0: the keys in $keys do not match the pinned fingerprints" >&2
	exit 1
fi

status=$(gpg --batch --status-fd 1 --verify "$sig" "$tar" 2>/dev/null) || {
	echo "$0: signature check failed for $tar" >&2
	exit 1
}
if printf '%s\n' "$status" | grep -Eq '^\[GNUPG:\] (EXPKEYSIG|EXPSIG|REVKEYSIG|BADSIG|ERRSIG)'; then
	echo "$0: signature is expired, revoked or bad" >&2
	exit 1
fi
# VALIDSIG <signing fpr> <date> <ts> <expire> <ver> <rsvd> <pk-algo> <hash-algo> <class> <primary fpr>
primary=$(printf '%s\n' "$status" | awk '$2 == "VALIDSIG" { print $NF }')
if [ -z "$primary" ] || ! printf '%s\n' "$pinned" | grep -qx "$primary"; then
	echo "$0: $tar is not signed by a pinned NGINX key (signer: ${primary:-none})" >&2
	exit 1
fi

echo "signed by $primary" >&2
if command -v sha256sum >/dev/null; then sha256sum "$tar"; else shasum -a 256 "$tar"; fi | awk '{ print $1 }'
