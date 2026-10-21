package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const workflowsDir = "../../.github/workflows"

var (
	usesRE    = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(\S+)(?:\s+#\s*(\S+))?`)
	pinnedRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)
	commentRE = regexp.MustCompile(`^v\d+(\.\d+){0,2}$`)
)

func workflowFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(workflowsDir, pat))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatalf("no workflows in %s", workflowsDir)
	}
	return files
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(data), "\n")
}

// stripComment drops a trailing YAML comment (good enough for workflow files,
// which do not put '#' inside values).
func stripComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

// jobBlock returns the lines of the top-level job with the given id, or nil.
func jobBlock(lines []string, id string) []string {
	in, start := false, -1
	for i, l := range lines {
		if l == "jobs:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") {
			if start >= 0 {
				return lines[start:i]
			}
			in = false
			continue
		}
		if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.TrimSpace(l) != "" && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			if start >= 0 {
				return lines[start:i]
			}
			if strings.TrimSpace(l) == id+":" {
				start = i
			}
		}
	}
	if start >= 0 {
		return lines[start:]
	}
	return nil
}

func TestActionsPinned(t *testing.T) {
	for _, f := range workflowFiles(t) {
		for n, line := range readLines(t, f) {
			m := usesRE.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue
			}
			where := filepath.Base(f) + ":" + strconv.Itoa(n+1)
			if !pinnedRE.MatchString(m[1]) {
				t.Errorf("%s: %q is not owner/repo@<40 hex>", where, m[1])
			}
			if !commentRE.MatchString(m[2]) {
				t.Errorf("%s: %q needs a trailing '# vX.Y.Z' comment", where, m[1])
			}
		}
	}
}

// The build matrices must come from versions.json via a "versions" job, and
// no workflow may hard-code an NGF or NGINX version.
func TestCIMatrixMatchesVersions(t *testing.T) {
	versions, err := LoadVersions("../../versions.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci.yml", "kind.yml", "release.yml"} {
		path := filepath.Join(workflowsDir, name)
		if _, err := os.Stat(path); err != nil {
			if name == "release.yml" {
				continue // written by a later task
			}
			t.Fatalf("%s: %v", name, err)
		}
		lines := readLines(t, path)
		job := jobBlock(lines, "versions")
		if job == nil {
			t.Errorf("%s: no versions job", name)
			continue
		}
		if !strings.Contains(strings.Join(job, "\n"), "versions.json") {
			t.Errorf("%s: versions job does not read versions.json", name)
		}
		if !strings.Contains(strings.Join(lines, "\n"), "fromJSON(needs.versions.outputs.") {
			t.Errorf("%s: no matrix built from needs.versions.outputs", name)
		}
	}
	for _, f := range workflowFiles(t) {
		for n, line := range readLines(t, f) {
			code := stripComment(line)
			for _, v := range versions {
				for _, lit := range []string{v.NGF, v.NGINX, v.NGINXSHA256} {
					if lit != "" && strings.Contains(code, lit) {
						t.Errorf("%s:%d hard-codes %q from versions.json", filepath.Base(f), n+1, lit)
					}
				}
			}
		}
	}
}
