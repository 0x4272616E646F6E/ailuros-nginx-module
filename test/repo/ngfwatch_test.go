package repo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ngf-watch.sh with docker, curl and the signature check stubbed out: only the
// version selection, versions.json rewrite and tarball sanity check run.
func TestNGFWatch(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// docker run ... <image>:<ngf> -V  -> nginx version derived from the NGF tag: 2.X.Y -> 1.X.Y
	write(filepath.Join(bin, "docker"), `#!/bin/sh
for a; do [ "$a" = inspect ] && { echo "img@sha256:abc"; exit 0; }; done
for a; do case $a in *:[0-9]*) tag=${a##*:} ;; esac; done
echo "nginx version: nginx/1.${tag#*.}"
`)
	// curl -o <file> <url>: a tarball of the version in the URL (or a wrong one if BAD_TARBALL is set).
	write(filepath.Join(bin, "curl"), `#!/bin/sh
while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift ;; http*) url=$1 ;; esac; shift; done
case $url in
*.asc) echo sig > "$out" ;;
*) v=${url##*/nginx-}; v=${v%.tar.gz}
   w=$(mktemp -d); real=${BAD_TARBALL:-$v}
   mkdir -p "$w/nginx-$real/src/core"
   printf '#define NGINX_VERSION      "%s"\n' "$real" > "$w/nginx-$real/src/core/nginx.h"
   tar -czf "$out" -C "$w" "nginx-$real" ;;
esac
`)
	verify := filepath.Join(dir, "verify.sh")
	write(verify, "#!/bin/sh\necho "+strings.Repeat("a", 64)+"\n")

	start := `[
  {
    "ngf": "2.8.0",
    "nginx": "1.31.7",
    "nginx_sha256": "89ea8a3dc171511f7325043b50e4c036248d9fad5feb7ec809c0b82d5df42c65"
  },
  {
    "ngf": "2.7.0",
    "nginx": "1.7.0",
    "nginx_sha256": "` + strings.Repeat("b", 64) + `"
  }
]
`

	run1 := func(tags string, extra ...string) (string, string, error) {
		f := filepath.Join(dir, "versions.json")
		if err := os.WriteFile(f, []byte(start), 0o644); err != nil {
			t.Fatal(err)
		}
		notes := filepath.Join(dir, "notes.md")
		os.Remove(notes)
		env := append([]string{"PATH=" + bin + ":" + os.Getenv("PATH"), "VERSIONS_FILE=" + f,
			"NGF_TAGS=" + tags, "VERIFY_SCRIPT=" + verify, "NOTES_FILE=" + notes}, extra...)
		out, err := run(t, env, "../../scripts/ngf-watch.sh")
		got, _ := os.ReadFile(f)
		n, _ := os.ReadFile(notes)
		return out + "\x00" + string(n), string(got), err
	}

	t.Run("adds new, keeps newest three, ignores prereleases and junk", func(t *testing.T) {
		out, got, err := run1("v2.9.0\nv2.8.0\nv2.7.0\nv2.6.0\nv3.0.0-rc1\nlatest\nv2.10.1")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		var vs []Version
		if err := json.Unmarshal([]byte(got), &vs); err != nil {
			t.Fatal(err)
		}
		var order []string
		for _, v := range vs {
			order = append(order, v.NGF)
		}
		if strings.Join(order, ",") != "2.10.1,2.9.0,2.8.0" {
			t.Errorf("rows %v, want newest three numerically sorted 2.10.1,2.9.0,2.8.0\n%s", order, got)
		}
		for _, w := range []string{"NGF 2.10.1: NGINX 1.10.1", "img@sha256:abc"} {
			if !strings.Contains(out, w) {
				t.Errorf("notes lack %q:\n%s", w, out)
			}
		}
	})

	t.Run("no change leaves the file alone", func(t *testing.T) {
		out, got, err := run1("v2.8.0\nv2.7.0")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got != start {
			t.Errorf("file rewritten:\n%s", got)
		}
		if strings.Split(out, "\x00")[0] != "" {
			t.Errorf("printed additions: %q", out)
		}
	})

	t.Run("tarball of another version is refused", func(t *testing.T) {
		out, got, err := run1("v2.9.0", "BAD_TARBALL=1.2.3")
		if err == nil || !strings.Contains(out, "is not NGINX") {
			t.Errorf("accepted: %v\n%s", err, out)
		}
		if got != start {
			t.Errorf("file modified despite failure:\n%s", got)
		}
	})
}
