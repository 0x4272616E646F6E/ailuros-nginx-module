package repo

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	semverRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// semverLess reports whether a < b for plain MAJOR.MINOR.PATCH strings.
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

func TestVersionsFile(t *testing.T) {
	versions, err := LoadVersions("../../versions.json")
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("versions.json has no entries")
	}
	seen := map[string]bool{}
	for i, v := range versions {
		if !semverRE.MatchString(v.NGF) {
			t.Errorf("entry %d: NGF %q is not semver", i, v.NGF)
		}
		if !semverRE.MatchString(v.NGINX) {
			t.Errorf("entry %d: NGINX %q is not semver", i, v.NGINX)
		}
		if !sha256RE.MatchString(v.NGINXSHA256) {
			t.Errorf("entry %d: NGINXSHA256 %q is not 64 lowercase hex", i, v.NGINXSHA256)
		}
		if seen[v.NGF] {
			t.Errorf("entry %d: duplicate NGF %q", i, v.NGF)
		}
		seen[v.NGF] = true
		if i > 0 && semverRE.MatchString(v.NGF) && semverRE.MatchString(versions[i-1].NGF) &&
			!semverLess(v.NGF, versions[i-1].NGF) {
			t.Errorf("entry %d: NGF %q not older than previous %q (want newest first)", i, v.NGF, versions[i-1].NGF)
		}
	}
}
