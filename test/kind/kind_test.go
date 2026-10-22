//go:build kind

// Package kind installs the module into a real NGINX Gateway Fabric in a kind
// cluster, from deploy/ngf/ as documented, and checks allow, block, stacking
// with tinyauth and the fail-closed/open behaviour end to end.
//
// It never touches the user's kubeconfig: it writes a temporary one, passes it
// to every kind, kubectl and helm call, and refuses to apply anything unless
// the current context is the kind cluster it created.
package kind

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/harness"
)

const (
	cluster  = "ailuros-module-it"
	context_ = "kind-" + cluster
	// ailurosImage is ghcr.io/0x4272616e646f6e/ailuros:0.1.0, pinned by digest
	// (as in the contract test). The kind node pulls it.
	ailurosImage = "ghcr.io/0x4272616e646f6e/ailuros@sha256:b7540bacf0a1729bba473f728119157cc584f28449aa52b763d5e502f41db273"
	ngfChart     = "oci://ghcr.io/nginx/charts/nginx-gateway-fabric"
	// configMapPrefix starts the module ConfigMap's name in
	// deploy/ngf/helm-values-patch.yaml; the NGF version follows it.
	configMapPrefix = "ailuros-nginx-module-ngf"
)

var configMapRE = regexp.MustCompile(regexp.QuoteMeta(configMapPrefix) + `(\d+\.\d+\.\d+)`)

// crdKustomization is the Gateway API CRD reference NGF documents for an
// NGF release: its repository's config/crd/gateway-api/standard at the
// release tag. NGF 2.x documents it that way; any other major version is
// refused rather than guessed.
func crdKustomization(ngf string) (string, error) {
	if !strings.HasPrefix(ngf, "2.") {
		return "", fmt.Errorf("no Gateway API CRD reference known for NGF %s: add one to crdKustomization in test/kind/kind_test.go", ngf)
	}
	return "https://github.com/nginx/nginx-gateway-fabric/config/crd/gateway-api/standard?ref=v" + ngf + "&timeout=120s", nil
}

// valuesPatchFor returns deploy/ngf/helm-values-patch.yaml with the module
// ConfigMap's name carrying ngf instead of the NGF version deploy/ngf targets
// (the newest versions.json row), and that deploy version. Nothing else changes.
func valuesPatchFor(patch []byte, ngf string) (out []byte, deployNGF string, err error) {
	m := configMapRE.FindSubmatch(patch)
	if m == nil {
		return nil, "", fmt.Errorf("helm-values-patch.yaml has no %s<version> ConfigMap name", configMapPrefix)
	}
	deployNGF = string(m[1])
	return bytes.ReplaceAll(patch, []byte(configMapPrefix+deployNGF), []byte(configMapPrefix+ngf)), deployNGF, nil
}

type env struct {
	kubeconfig string
	root       string
	stubImage  string
}

// cmd runs a command with the temporary kubeconfig and nothing else.
func (e *env) cmd(t *testing.T, timeout time.Duration, stdin string, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Env = e.environ()
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	out, err := c.CombinedOutput()
	return string(out), err
}

func (e *env) environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KUBECONFIG=") {
			out = append(out, kv)
		}
	}
	return append(out, "KUBECONFIG="+e.kubeconfig)
}

// kubectl always names the kubeconfig and the kind context explicitly.
func (e *env) kubectl(t *testing.T, args ...string) string {
	t.Helper()
	return e.kubectlIn(t, "", args...)
}

func (e *env) kubectlIn(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	full := append([]string{"--kubeconfig", e.kubeconfig, "--context", context_}, args...)
	out, err := e.cmd(t, 10*time.Minute, stdin, "kubectl", full...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *env) kubectlTry(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--kubeconfig", e.kubeconfig, "--context", context_}, args...)
	return e.cmd(t, 2*time.Minute, "", "kubectl", full...)
}

func TestNGF(t *testing.T) {
	for _, tool := range []string{"docker", "kind", "kubectl", "helm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required for the kind test", tool)
		}
	}
	// The versions.json row under test ($AILUROS_NGF). deploy/ngf targets the
	// newest row; for any other row the NGF version in the ConfigMap name of
	// the values patch is substituted, nothing else is changed.
	ngf := harness.NGFVersion(t)
	root := harness.RepoRoot(t)
	crdURL, err := crdKustomization(ngf)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(root, "deploy", "ngf", "helm-values-patch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	patch, deployNGF, err := valuesPatchFor(patch, ngf)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NGF %s (deploy/ngf targets %s)", ngf, deployNGF)
	valuesFile := filepath.Join(t.TempDir(), "helm-values-patch.yaml")
	if err := os.WriteFile(valuesFile, patch, 0o600); err != nil {
		t.Fatal(err)
	}
	so := harness.ModuleSO(t)
	arch := harness.DockerArch(t)

	dir := t.TempDir()
	e := &env{kubeconfig: filepath.Join(dir, "kubeconfig"), root: root, stubImage: harness.BuildStubImage(t)}

	// Delete the cluster at the end, even on failure or a half-created cluster.
	t.Cleanup(func() {
		if t.Failed() {
			e.diagnostics(t)
		}
		out, err := e.cmd(t, 5*time.Minute, "", "kind", "delete", "cluster", "--name", cluster, "--kubeconfig", e.kubeconfig)
		t.Logf("kind delete cluster: err=%v\n%s", err, strings.TrimSpace(out))
	})
	// A cluster of this name left by an earlier run is ours: replace it.
	if existing, _ := e.cmd(t, time.Minute, "", "kind", "get", "clusters"); strings.Contains(existing, cluster) {
		t.Logf("deleting a pre-existing kind cluster named %s", cluster)
		_, _ = e.cmd(t, 5*time.Minute, "", "kind", "delete", "cluster", "--name", cluster, "--kubeconfig", e.kubeconfig)
	}

	if out, err := e.cmd(t, 10*time.Minute, "", "kind", "create", "cluster", "--name", cluster,
		"--kubeconfig", e.kubeconfig, "--wait", "180s"); err != nil {
		t.Fatalf("kind create cluster: %v\n%s", err, out)
	}
	// Refuse to go on unless the temporary kubeconfig points at our cluster.
	if got := strings.TrimSpace(e.kubectl(t, "config", "current-context")); got != context_ {
		t.Fatalf("current context is %q, want %q", got, context_)
	}

	if out, err := e.cmd(t, 10*time.Minute, "", "kind", "load", "docker-image", e.stubImage,
		"--name", cluster); err != nil {
		t.Fatalf("kind load docker-image: %v\n%s", err, out)
	}

	// Gateway API CRDs, as NGF's documentation installs them.
	crds, err := e.cmd(t, 5*time.Minute, "", "kubectl", "--kubeconfig", e.kubeconfig, "--context", context_, "kustomize", crdURL)
	if err != nil {
		t.Fatalf("fetch Gateway API CRDs: %v\n%s", err, crds)
	}
	e.kubectlIn(t, crds, "apply", "--server-side", "-f", "-")

	// The module ConfigMap, generated from the freshly built .so, must exist
	// before the data plane pods that mount it.
	cm, err := e.cmd(t, time.Minute, "", filepath.Join(root, "scripts", "configmap.sh"), so, ngf, arch)
	if err != nil {
		t.Fatalf("configmap.sh: %v\n%s", err, cm)
	}
	e.kubectlIn(t, cm, "apply", "-f", "-")

	// NGF itself, with the documented values patch (the ConfigMap name carries
	// this row's NGF version).
	if out, err := e.cmd(t, 10*time.Minute, "", "helm", "install", "ngf", ngfChart,
		"--version", ngf, "-n", "nginx-gateway", "--create-namespace",
		"-f", valuesFile,
		"--kubeconfig", e.kubeconfig, "--kube-context", context_, "--wait", "--timeout", "8m"); err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	} else {
		t.Logf("helm install ok")
	}

	// Workloads: the real Ailuros on the stub model, tinyauth and echo stubs.
	e.kubectlIn(t, workloads(e.stubImage), "apply", "-f", "-")
	for _, d := range []string{"model", "ailuros", "tinyauth", "echo", "spoof"} {
		e.kubectl(t, "rollout", "status", "deploy/"+d, "--timeout=180s")
	}

	// deploy/ngf as documented (policy and filters), then Gateway and routes.
	e.kubectl(t, "apply", "-k", filepath.Join(root, "deploy", "ngf"))
	e.kubectlIn(t, gatewayAndRoutes, "apply", "-f", "-")
	e.kubectl(t, "wait", "--for=condition=Programmed", "gateway/gateway", "--timeout=240s")
	e.kubectl(t, "rollout", "status", "deploy/gateway-nginx", "--timeout=240s")

	t.Run("nginx container is container 0 and mounts the module", func(t *testing.T) {
		name := strings.TrimSpace(e.kubectl(t, "get", "deploy/gateway-nginx", "-o", "jsonpath={.spec.template.spec.containers[0].name}"))
		if name != "nginx" {
			t.Fatalf("containers[0] is %q, want nginx: the JSONPatch in deploy/ngf/helm-values-patch.yaml would mount the module into the wrong container", name)
		}
		mounts := e.kubectl(t, "get", "deploy/gateway-nginx", "-o", "jsonpath={.spec.template.spec.containers[0].volumeMounts[*].mountPath}")
		if !strings.Contains(mounts, "/usr/lib/nginx/modules/ailuros") {
			t.Fatalf("nginx container mounts: %s", mounts)
		}
		image := strings.TrimSpace(e.kubectl(t, "get", "deploy/gateway-nginx", "-o", "jsonpath={.spec.template.spec.containers[0].image}"))
		if !strings.HasSuffix(image, ":"+ngf) {
			t.Fatalf("data plane image %q, want tag %s (the chart pins the image to its own version)", image, ngf)
		}
		t.Logf("containers[0].name = %s, image %s; mounts: %s", name, image, mounts)
	})

	base := e.portForward(t, "svc/gateway-nginx", "80")
	client := &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(t *testing.T, path string, headers map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// Wait until NGF has programmed every route.
	waitFor(t, 3*time.Minute, func() error {
		for path, want := range map[string]int{"/plain/ready": 200, "/checked/ready": 200, "/open/ready": 200, "/stacked/ready": 302, "/spoof/ready": 200} {
			req, _ := http.NewRequest(http.MethodGet, base+path, nil)
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != want {
				return fmt.Errorf("%s: status %d, want %d", path, resp.StatusCode, want)
			}
		}
		return nil
	})

	reqID := regexp.MustCompile(`^[0-9a-f]{16}$`)
	echoHas := func(t *testing.T, svc, needle string) bool { return strings.Contains(e.calls(t, svc), needle) }
	decisions := func(t *testing.T) int {
		return strings.Count(e.kubectl(t, "logs", "deploy/ailuros"), `"msg":"decision"`)
	}

	t.Run("plain route never calls Ailuros", func(t *testing.T) {
		// Positive control: a checked request does reach Ailuros and is logged,
		// so the zero below cannot be a log we fail to read.
		c0 := decisions(t)
		if r, _ := do(t, "/checked/control", nil); r.StatusCode != 200 {
			t.Fatalf("control request: status %d, want 200", r.StatusCode)
		}
		waitFor(t, 15*time.Second, func() error {
			if decisions(t) <= c0 {
				return fmt.Errorf("no new Ailuros decision after a checked request")
			}
			return nil
		})
		d0, m0 := decisions(t), e.callCount(t, "model")
		resp, _ := do(t, "/plain/hello?q=attack", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d, want 200 (an attack query on an unfiltered route)", resp.StatusCode)
		}
		if id := resp.Header.Get("X-Ailuros-Request-Id"); id != "" {
			t.Errorf("X-Ailuros-Request-Id = %q on a route without the filter", id)
		}
		time.Sleep(time.Second) // Ailuros logs its decision after answering
		if d := decisions(t); d != d0 {
			t.Errorf("Ailuros logged %d new decisions", d-d0)
		}
		if m := e.callCount(t, "model"); m != m0 {
			t.Errorf("the model got %d new calls", m-m0)
		}
		if !echoHas(t, "echo", "/plain/hello?q=attack") {
			t.Errorf("echo did not receive the request")
		}
		// An upstream that sets the header itself must not reach the client.
		spoof, _ := do(t, "/spoof/hello", nil)
		if spoof.StatusCode != 200 {
			t.Fatalf("/spoof/hello: status %d, want 200", spoof.StatusCode)
		}
		if v := spoof.Header.Values("X-Ailuros-Request-Id"); len(v) != 0 {
			t.Errorf("upstream-set X-Ailuros-Request-Id leaked to the client: %q", v)
		}
	})

	t.Run("checked route allows and blocks", func(t *testing.T) {
		resp, _ := do(t, "/checked/hello", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("/checked/hello: status %d, want 200", resp.StatusCode)
		}
		if id := resp.Header.Get("X-Ailuros-Request-Id"); !reqID.MatchString(id) {
			t.Errorf("X-Ailuros-Request-Id = %q, want 16 hex characters", id)
		}
		blocked, _ := do(t, "/checked/blockedpath?q=attack", nil)
		if blocked.StatusCode != 403 {
			t.Fatalf("?q=attack: status %d, want 403", blocked.StatusCode)
		}
		if id := blocked.Header.Get("X-Ailuros-Request-Id"); !reqID.MatchString(id) {
			t.Errorf("blocked X-Ailuros-Request-Id = %q, want 16 hex characters", id)
		}
		if echoHas(t, "echo", "/checked/blockedpath") {
			t.Errorf("a blocked request reached the upstream")
		}
		t.Logf("allow -> 200, block -> 403 (request id %s)", blocked.Header.Get("X-Ailuros-Request-Id"))
	})

	t.Run("stacked route: Ailuros before tinyauth", func(t *testing.T) {
		t0 := e.callCount(t, "tinyauth")
		blocked, _ := do(t, "/stacked/blocked?q=attack", nil)
		if blocked.StatusCode != 403 {
			t.Fatalf("attack: status %d, want 403", blocked.StatusCode)
		}
		if n := e.callCount(t, "tinyauth"); n != t0 {
			t.Errorf("tinyauth was called %d times for a blocked request", n-t0)
		}
		login, _ := do(t, "/stacked/hello", nil)
		if login.StatusCode != 302 || !strings.Contains(login.Header.Get("Location"), "login.example.test") {
			t.Fatalf("allowed request without credentials: status %d, Location %q, want a 302 to login", login.StatusCode, login.Header.Get("Location"))
		}
		if n := e.callCount(t, "tinyauth"); n != t0+1 {
			t.Errorf("tinyauth calls = %d, want %d", n-t0, 1)
		}
		if id := login.Header.Get("X-Ailuros-Request-Id"); !reqID.MatchString(id) {
			t.Errorf("302 X-Ailuros-Request-Id = %q, want 16 hex characters", id)
		}
		ok, _ := do(t, "/stacked/hello", map[string]string{"Authorization": "ok"})
		if ok.StatusCode != 200 {
			t.Errorf("with credentials: status %d, want 200", ok.StatusCode)
		}
		if id := ok.Header.Get("X-Ailuros-Request-Id"); !reqID.MatchString(id) {
			t.Errorf("200 X-Ailuros-Request-Id = %q, want 16 hex characters", id)
		}
	})

	t.Run("Ailuros down: closed 503, open 200", func(t *testing.T) {
		e.kubectl(t, "scale", "deploy/ailuros", "--replicas=0")
		e.kubectl(t, "wait", "--for=delete", "pod", "-l", "app=ailuros", "--timeout=120s")
		before, _ := e.kubectlTry(t, "logs", "deploy/gateway-nginx", "-c", "nginx")

		closed, _ := do(t, "/checked/down-closed", nil)
		if closed.StatusCode != 503 {
			t.Errorf("checked, Ailuros down: status %d, want 503", closed.StatusCode)
		}
		open, _ := do(t, "/open/down-open", nil)
		if open.StatusCode != 200 {
			t.Errorf("open, Ailuros down: status %d, want 200", open.StatusCode)
		}
		if echoHas(t, "echo", "/checked/down-closed") {
			t.Errorf("a fail-closed request reached the upstream")
		}
		if !echoHas(t, "echo", "/open/down-open") {
			t.Errorf("the fail-open request did not reach the upstream")
		}
		after, _ := e.kubectlTry(t, "logs", "deploy/gateway-nginx", "-c", "nginx")
		var rejected, warned bool
		for _, line := range strings.Split(strings.TrimPrefix(after, before), "\n") {
			if strings.Contains(line, "[error]") && strings.Contains(line, "request: \"GET /checked/down-closed") &&
				strings.Contains(line, "rejecting the request with 503") && strings.Contains(line, "ailuros_request_unavailable closed") {
				rejected = true
				t.Logf("error: %s", line)
			}
			if strings.Contains(line, "[warn]") && strings.Contains(line, "request: \"GET /open/down-open") &&
				strings.Contains(line, "letting the request through") && strings.Contains(line, "ailuros_request_unavailable open") {
				warned = true
				t.Logf("warning: %s", line)
			}
		}
		if !rejected {
			t.Errorf("no fail-closed [error] line for /checked/down-closed in the nginx log")
		}
		if !warned {
			t.Errorf("no fail-open [warn] line for /open/down-open in the nginx log")
		}
	})
}

// calls returns the JSON list of requests a stub received, through the API
// server's service proxy to its control port (named ctl).
func (e *env) calls(t *testing.T, svc string) string {
	t.Helper()
	return e.kubectl(t, "get", "--raw", "/api/v1/namespaces/default/services/"+svc+":ctl/proxy/calls")
}

// callCount is the number of requests a stub has received.
func (e *env) callCount(t *testing.T, svc string) int {
	t.Helper()
	var calls []json.RawMessage
	if err := json.Unmarshal([]byte(e.calls(t, svc)), &calls); err != nil {
		t.Fatalf("parse %s calls: %v", svc, err)
	}
	return len(calls)
}

// portForward starts kubectl port-forward and returns the base URL.
func (e *env) portForward(t *testing.T, target, port string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := exec.CommandContext(ctx, "kubectl", "--kubeconfig", e.kubeconfig, "--context", context_,
		"port-forward", "--address", "127.0.0.1", target, fmt.Sprintf("%d:%s", local, port))
	c.Env = e.environ()
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Start(); err != nil {
		cancel()
		t.Fatalf("port-forward: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = c.Wait() })
	waitFor(t, time.Minute, func() error {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), time.Second)
		if err != nil {
			return fmt.Errorf("%v: %s", err, buf.String())
		}
		conn.Close()
		return nil
	})
	return fmt.Sprintf("http://127.0.0.1:%d", local)
}

func waitFor(t *testing.T, d time.Duration, f func() error) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if err = f(); err == nil {
			return
		}
	}
	t.Fatalf("not ready after %s: %v", d, err)
}

// diagnostics logs what is needed to see why a run failed.
func (e *env) diagnostics(t *testing.T) {
	for _, args := range [][]string{
		{"get", "gateway,httproute,snippetspolicy,snippetsfilter,pods", "-A", "-o", "wide"},
		{"describe", "gateway", "gateway"},
		{"logs", "-n", "nginx-gateway", "deploy/ngf-nginx-gateway-fabric", "--tail=80"},
		{"logs", "deploy/gateway-nginx", "-c", "nginx", "--tail=80"},
		{"logs", "deploy/ailuros", "--tail=40"},
	} {
		out, _ := e.kubectlTry(t, args...)
		t.Logf("kubectl %s:\n%s", strings.Join(args, " "), out)
	}
}

func workloads(stub string) string {
	stubDeploy := func(name, role, listen string, extra ...string) string {
		args := `["-role", "` + role + `", "-listen", ":` + listen + `"`
		for _, x := range extra {
			args += `, "` + x + `"`
		}
		args += "]"
		return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: %[1]s}}
  template:
    metadata: {labels: {app: %[1]s}}
    spec:
      containers:
        - name: stub
          image: %[2]s
          imagePullPolicy: IfNotPresent
          args: %[3]s
          ports: [{containerPort: %[4]s}, {containerPort: 9999}]
          readinessProbe: {httpGet: {path: /ready, port: 9999}}
---
apiVersion: v1
kind: Service
metadata: {name: %[1]s}
spec:
  selector: {app: %[1]s}
  ports:
    - {name: http, port: %[5]s, targetPort: %[4]s}
    - {name: ctl, port: 9999, targetPort: 9999}
---
`, name, stub, args, listen, map[string]string{"model": "8000", "tinyauth": "3000", "echo": "80", "spoof": "80"}[name])
	}
	return stubDeploy("model", "model", "8000") +
		stubDeploy("tinyauth", "tinyauth", "3000") +
		stubDeploy("echo", "echo", "8080") +
		stubDeploy("spoof", "echo", "8080", "-resp-header", "X-Ailuros-Request-Id: spoofed0123456789") + `apiVersion: apps/v1
kind: Deployment
metadata: {name: ailuros}
spec:
  replicas: 1
  selector: {matchLabels: {app: ailuros}}
  template:
    metadata: {labels: {app: ailuros}}
    spec:
      containers:
        - name: ailuros
          image: ` + ailurosImage + `
          env:
            - {name: AILUROS_INSPECTION_MODE, value: lite}
            - {name: AILUROS_CLASSIFIER_BACKEND, value: openai}
            - {name: AILUROS_CLASSIFIER_BASE_URL, value: "http://model.default.svc.cluster.local:8000/v1"}
            - {name: AILUROS_CLASSIFIER_MODEL, value: stub}
            - {name: AILUROS_FORWARDAUTH_ADDR, value: ":9002"}
            - {name: AILUROS_EXTPROC_ADDR, value: ""}
            - {name: AILUROS_EXTAUTHZ_ADDR, value: ""}
          ports: [{containerPort: 9002}, {containerPort: 9090}]
          readinessProbe: {httpGet: {path: /readyz, port: 9090}}
---
apiVersion: v1
kind: Service
metadata: {name: ailuros}
spec:
  selector: {app: ailuros}
  ports:
    - {name: forwardauth, port: 9002, targetPort: 9002}
`
}

// gatewayAndRoutes are the test's own objects: the Gateway the policy
// targets, a tinyauth auth_request filter, and the four routes.
var gatewayAndRoutes = `apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: gateway, namespace: default}
spec:
  gatewayClassName: nginx
  listeners:
    - {name: http, port: 80, protocol: HTTP}
---
apiVersion: gateway.nginx.org/v1alpha1
kind: SnippetsFilter
metadata: {name: tinyauth, namespace: default}
spec:
  snippets:
    - context: http.server
      value: |
        location = /_tinyauth {
            internal;
            resolver 10.96.0.10 valid=10s ipv6=off;
            set $tinyauth_upstream tinyauth.default.svc.cluster.local:3000;
            proxy_pass http://$tinyauth_upstream/;
            proxy_pass_request_body off;
            proxy_set_header Content-Length "";
        }
    - context: http.server.location
      value: |
        auth_request /_tinyauth;
        error_page 401 =302 http://login.example.test/login;
` + routes("plain", "echo", nil) + routes("spoof", "spoof", nil) + routes("checked", "echo", []string{"ailuros-check"}) +
	routes("stacked", "echo", []string{"ailuros-check", "tinyauth"}) + routes("open", "echo", []string{"ailuros-check-open"})

func routes(name, backend string, filters []string) string {
	var f string
	for _, n := range filters {
		f += fmt.Sprintf("        - type: ExtensionRef\n          extensionRef: {group: gateway.nginx.org, kind: SnippetsFilter, name: %s}\n", n)
	}
	if f != "" {
		f = "      filters:\n" + f
	}
	return fmt.Sprintf(`---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: %[1]s, namespace: default}
spec:
  parentRefs: [{name: gateway}]
  rules:
    - matches: [{path: {type: PathPrefix, value: /%[1]s}}]
%[2]s      backendRefs: [{name: %[3]s, port: 80}]
`, name, f, backend)
}
