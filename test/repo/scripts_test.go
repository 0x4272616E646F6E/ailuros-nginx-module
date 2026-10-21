package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, env []string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestChangelogSection(t *testing.T) {
	dir := t.TempDir()
	cl := filepath.Join(dir, "CHANGELOG.md")
	body := "# Changelog\n\n## [Unreleased]\n\n- later\n\n## [0.2.0] - 2026-11-01\n\n- two\n- more\n\n## v0.1.0\n\n- one\n\n## [0.0.9] - 2026-10-01\n\n## [0.0.8]\n- eight\n"
	if err := os.WriteFile(cl, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	const script = "../../scripts/changelog-section.sh"

	for tag, want := range map[string]string{"v0.2.0": "- two\n- more", "v0.1.0": "- one", "v0.0.8": "- eight"} {
		out, err := run(t, nil, script, tag, cl)
		if err != nil {
			t.Errorf("%s: %v\n%s", tag, err, out)
			continue
		}
		if strings.TrimSpace(out) != want {
			t.Errorf("%s: got %q, want %q", tag, out, want)
		}
	}
	for _, tag := range []string{"v0.3.0", "v0.0.9", "v0.2", "v0.2.0-rc1"} { // missing, empty, partial
		if out, err := run(t, nil, script, tag, cl); err == nil {
			t.Errorf("%s: expected failure, got %q", tag, out)
		} else if !strings.Contains(out, "no section for "+tag) {
			t.Errorf("%s: message not clear: %s", tag, out)
		}
	}
}

func TestVerifyNginxSource(t *testing.T) {
	for _, tool := range []string{"gpg"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	const script = "../../scripts/verify-nginx-source.sh"
	dir := t.TempDir()
	// Short path: gpg-agent's socket path is length-limited.
	gh, err := os.MkdirTemp("/tmp", "ailgpg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = run(t, []string{"GNUPGHOME=" + gh}, "gpgconf", "--kill", "gpg-agent")
		_ = os.RemoveAll(gh)
	})
	genv := []string{"GNUPGHOME=" + gh}
	gen := func(name string) string { // returns the primary fingerprint
		t.Helper()
		if out, err := run(t, genv, "gpg", "--batch", "--passphrase", "", "--quick-gen-key", name+"@test", "rsa2048", "sign", "1d"); err != nil {
			t.Fatalf("gen key: %v\n%s", err, out)
		}
		out, err := run(t, genv, "gpg", "--batch", "--with-colons", "--fingerprint", name+"@test")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "fpr:") {
				return strings.Split(l, ":")[9]
			}
		}
		t.Fatal("no fingerprint")
		return ""
	}
	export := func(name, path string) {
		t.Helper()
		out, err := run(t, genv, "gpg", "--batch", "--armor", "--export", name+"@test")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pinnedFPR, otherFPR := gen("pinned"), gen("other")

	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o755); err != nil {
		t.Fatal(err)
	}
	export("pinned", filepath.Join(keys, "pinned.key"))
	if err := os.WriteFile(filepath.Join(keys, "FINGERPRINTS"), []byte("# test\n"+pinnedFPR+"  test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "nginx.tar.gz")
	if err := os.WriteFile(tarball, []byte("not really a tarball"), 0o644); err != nil {
		t.Fatal(err)
	}
	sign := func(name, out string) {
		t.Helper()
		if o, err := run(t, genv, "gpg", "--batch", "--yes", "--local-user", name+"@test", "--output", out, "--detach-sign", tarball); err != nil {
			t.Fatalf("sign: %v\n%s", err, o)
		}
	}
	good, bad := filepath.Join(dir, "good.asc"), filepath.Join(dir, "other.asc")
	sign("pinned", good)
	sign("other", bad)
	env := []string{"NGINX_KEYS_DIR=" + keys, "GNUPGHOME="} // the script uses its own home

	out, err := run(t, env, script, tarball, good)
	if err != nil || len(lastLine(out)) != 64 {
		t.Fatalf("valid signature rejected or no digest: %v\n%s", err, out)
	}
	if out, err := run(t, env, script, tarball, bad); err == nil || !strings.Contains(out, "not signed by a pinned") && !strings.Contains(out, "failed") {
		t.Errorf("signature by an unpinned key accepted (%s): %v\n%s", otherFPR, err, out)
	}
	tampered := filepath.Join(dir, "tampered.tar.gz")
	if err := os.WriteFile(tampered, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, env, script, tampered, good); err == nil {
		t.Errorf("tampered tarball accepted:\n%s", out)
	}
	// A key file not in FINGERPRINTS is refused even with a valid signature.
	export("other", filepath.Join(keys, "other.key"))
	if out, err := run(t, env, script, tarball, good); err == nil || !strings.Contains(out, "pinned fingerprints") {
		t.Errorf("unpinned key file accepted: %v\n%s", err, out)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// The pinned production keys must match the fingerprint list.
func TestPinnedNginxKeys(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	sig := t.TempDir()
	tar := filepath.Join(sig, "t")
	if err := os.WriteFile(tar, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An invalid signature must fail on the signature, not on key mismatch.
	if err := os.WriteFile(tar+".asc", []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, nil, "../../scripts/verify-nginx-source.sh", tar, tar+".asc")
	if err == nil || strings.Contains(out, "do not match the pinned fingerprints") {
		t.Errorf("pinned keys do not match FINGERPRINTS (or bad signature accepted): %v\n%s", err, out)
	}
}
