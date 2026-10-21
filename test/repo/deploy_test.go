package repo

import (
	"os"
	"regexp"
	"testing"
)

// deploy/ngf targets the newest versions.json row (the kind test substitutes
// the version for older rows). When ngf-watch adds a row, deploy/ngf and its
// README move with it; this test is what notices if they do not.
func TestDeployNGFTargetsNewestRow(t *testing.T) {
	versions, err := LoadVersions("../../versions.json")
	if err != nil || len(versions) == 0 {
		t.Fatalf("versions.json: %v (%d rows)", err, len(versions))
	}
	newest := versions[0].NGF

	// Every NGF version spelled in these places must be the newest row's.
	checks := []struct {
		file string
		re   *regexp.Regexp
	}{
		{"../../deploy/ngf/helm-values-patch.yaml", regexp.MustCompile(`ailuros-nginx-module-ngf(\d+\.\d+\.\d+)`)},
		{"../../deploy/ngf/helm-values-patch.yaml", regexp.MustCompile(`--version (\d+\.\d+\.\d+)`)},
		{"../../deploy/ngf/kustomization.yaml", regexp.MustCompile(`ngf(\d+\.\d+\.\d+)`)},
		{"../../deploy/ngf/README.md", regexp.MustCompile(`NGF (\d+\.\d+\.\d+)`)},
		{"../../deploy/ngf/README.md", regexp.MustCompile(`ngf(\d+\.\d+\.\d+)`)},
	}
	for _, c := range checks {
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		found := c.re.FindAllSubmatch(data, -1)
		if len(found) == 0 {
			t.Errorf("%s: no match for %s", c.file, c.re)
		}
		for _, m := range found {
			if string(m[1]) != newest {
				t.Errorf("%s: mentions NGF %s, but the newest versions.json row is %s", c.file, m[1], newest)
			}
		}
	}
}
