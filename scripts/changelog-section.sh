#!/bin/sh
# Print the CHANGELOG.md section for a release tag (the lines under its
# "## [X.Y.Z]", "## X.Y.Z" or "## vX.Y.Z" heading, optionally followed by
# " - date"), up to the next "## " heading. Exits non-zero, with a message,
# if the section is missing or empty.
#   scripts/changelog-section.sh <tag> [CHANGELOG.md]
set -eu

[ $# -ge 1 ] && [ $# -le 2 ] || { echo "usage: $0 <tag> [changelog]" >&2; exit 2; }
tag=$1 file=${2:-CHANGELOG.md}
[ -r "$file" ] || { echo "$0: cannot read $file" >&2; exit 1; }
ver=${tag#v}

body=$(awk -v ver="$ver" '
	/^## / {
		if (on) exit
		h = $0
		sub(/^## +/, "", h)
		gsub(/[][]/, "", h)
		sub(/^v/, "", h)
		split(h, f, /[ \t]/)
		if (f[1] == ver) { on = 1; next }
	}
	on { print }
' "$file")

if [ -z "$(printf '%s' "$body" | tr -d '[:space:]')" ]; then
	echo "$0: $file has no section for $tag (expected a heading like \"## [$ver] - YYYY-MM-DD\" with content)" >&2
	exit 1
fi
printf '%s\n' "$body"
