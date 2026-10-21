package repo

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const configmapScript = "../../scripts/configmap.sh"

func runConfigMap(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(configmapScript, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		t.Logf("stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func TestConfigMapScript(t *testing.T) {
	dir := t.TempDir()
	so := filepath.Join(dir, "module.so")
	payload := []byte("\x7fELF fake module \x00\x01\xff bytes")
	if err := os.WriteFile(so, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("fields", func(t *testing.T) {
		out, err := runConfigMap(t, []string{"AILUROS_VERSION=v1.2.3"}, so, "2.8.0", "amd64")
		if err != nil {
			t.Fatalf("configmap.sh: %v", err)
		}
		want := []string{
			"kind: ConfigMap\n",
			"  name: ailuros-nginx-module-ngf2.8.0\n",
			"    app.kubernetes.io/name: ailuros-nginx-module\n",
			"    app.kubernetes.io/version: v1.2.3\n",
			"    ailuros.dev/arch: amd64\n",
			"binaryData:\n",
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("output lacks %q:\n%s", w, out)
			}
		}
		const key = "  ngx_http_ailuros_module.so: "
		i := strings.Index(out, key)
		if i < 0 {
			t.Fatalf("no binaryData key:\n%s", out)
		}
		enc := strings.TrimSpace(out[i+len(key):])
		got, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("binaryData is not base64: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("binaryData decodes to %q, want %q", got, payload)
		}
	})

	t.Run("namespace", func(t *testing.T) {
		out, err := runConfigMap(t, nil, so, "2.8.0", "arm64", "gw")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "  namespace: gw\n") || !strings.Contains(out, "ailuros.dev/arch: arm64\n") {
			t.Errorf("namespace or arch missing:\n%s", out)
		}
	})

	t.Run("version from file name", func(t *testing.T) {
		named := filepath.Join(dir, "ngx_http_ailuros_module-v0.4.0-ngf2.8.0-linux-amd64.so")
		if err := os.WriteFile(named, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := runConfigMap(t, []string{"AILUROS_VERSION="}, named, "2.8.0", "amd64")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "app.kubernetes.io/version: v0.4.0\n") {
			t.Errorf("version not taken from the file name:\n%s", out)
		}
	})

	// The limit is on the generated YAML (Kubernetes caps a ConfigMap at 1 MiB,
	// and base64 adds a third), not on the .so alone.
	t.Run("refuses output over 1 MiB", func(t *testing.T) {
		big := filepath.Join(dir, "big.so")
		if err := os.WriteFile(big, make([]byte, 800<<10), 0o644); err != nil { // 800 KiB -> ~1.04 MiB of base64
			t.Fatal(err)
		}
		out, err := runConfigMap(t, nil, big, "2.8.0", "amd64")
		if err == nil {
			t.Fatal("configmap.sh accepted output over 1 MiB")
		}
		if out != "" {
			t.Errorf("wrote output despite refusing: %.80q", out)
		}
	})

	t.Run("accepts output just under 1 MiB", func(t *testing.T) {
		edge := filepath.Join(dir, "edge.so")
		if err := os.WriteFile(edge, make([]byte, 760<<10), 0o644); err != nil { // 760 KiB -> ~0.99 MiB of base64
			t.Fatal(err)
		}
		out, err := runConfigMap(t, nil, edge, "2.8.0", "amd64")
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		if len(out) > 1<<20 {
			t.Errorf("output is %d bytes, over 1 MiB", len(out))
		}
	})

	t.Run("bad arguments", func(t *testing.T) {
		for _, args := range [][]string{
			{so, "2.8.0", "riscv"},
			{so, "x", "amd64"},
			{filepath.Join(dir, "missing.so"), "2.8.0", "amd64"},
			{so},
		} {
			if _, err := runConfigMap(t, nil, args...); err == nil {
				t.Errorf("%v: expected failure", args)
			}
		}
	})
}
