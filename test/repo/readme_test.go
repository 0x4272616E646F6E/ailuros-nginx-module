package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const readmeVersionsHeading = "## Supported NGF versions"

// TestREADMEVersionsTable checks that the README's "Supported NGF versions"
// table lists exactly the rows of versions.json, in order.
func TestREADMEVersionsTable(t *testing.T) {
	versions, err := LoadVersions("../../versions.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	i := strings.Index(text, readmeVersionsHeading)
	if i < 0 {
		t.Fatalf("README.md has no %q section", readmeVersionsHeading)
	}
	section := text[i+len(readmeVersionsHeading):]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}

	var rows [][2]string
	header := false
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for k := range cells {
			cells[k] = strings.Trim(strings.TrimSpace(cells[k]), "`")
		}
		if len(cells) != 2 {
			t.Fatalf("table row %q: want 2 columns (NGF, NGINX)", line)
		}
		if !header {
			header = true
			if cells[0] != "NGF" || cells[1] != "NGINX" {
				t.Fatalf("table header %v: want NGF | NGINX", cells)
			}
			continue
		}
		if strings.Trim(cells[0], "-: ") == "" {
			continue // separator row
		}
		rows = append(rows, [2]string{cells[0], cells[1]})
	}

	if len(rows) != len(versions) {
		t.Fatalf("README table has %d rows, versions.json has %d: %v", len(rows), len(versions), rows)
	}
	for k, v := range versions {
		if rows[k][0] != v.NGF || rows[k][1] != v.NGINX {
			t.Errorf("row %d: README has NGF %s / NGINX %s, versions.json has %s / %s",
				k, rows[k][0], rows[k][1], v.NGF, v.NGINX)
		}
	}
}

var mdLinkRE = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestDocsRelativeLinks checks that every relative link in the repository's
// Markdown documents points at a file or directory that exists.
func TestDocsRelativeLinks(t *testing.T) {
	root := "../.."
	var files []string
	for _, f := range []string{"README.md", "CONTRIBUTING.md", "SECURITY.md", "CHANGELOG.md", "deploy/ngf/README.md"} {
		files = append(files, filepath.Join(root, f))
	}
	design, err := filepath.Glob(filepath.Join(root, "docs", "design", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, design...)

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		inFence := false
		for n, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range mdLinkRE.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
					continue
				}
				if k := strings.IndexAny(target, "#?"); k >= 0 {
					target = target[:k]
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(f), target)); err != nil {
					t.Errorf("%s:%d: broken relative link %q", f, n+1, m[1])
				}
			}
		}
	}
}
