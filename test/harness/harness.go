// Package harness runs the behaviour suite's Docker Compose stack: the stock
// NGINX Gateway Fabric data plane image with the module mounted read-only at
// the production path and loaded from main-includes, running as uid 101, plus
// stubs for Ailuros (ailuros:9002), tinyauth (tinyauth:3000) and an echo
// upstream (echo:8080). Every stub records the requests it receives.
//
// The module is taken from $AILUROS_MODULE_SO, or else from
// out/ngx_http_ailuros_module-dev-ngf<ngf>-linux-<arch>.so for the first NGF
// version in versions.json and the Docker host's architecture (build it with
// make build NGF=<ngf> ARCH=<arch>).
package harness

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/repo"
)

// ModulePath is where NGF mounts the module, and where the stack mounts it.
const ModulePath = "/usr/lib/nginx/modules/ailuros/ngx_http_ailuros_module.so"

const stubImage = "ailuros-mod-it-stub:local"

// Config describes one stack.
type Config struct {
	// NGINXConf is included in the http block of the stock nginx.conf (it
	// replaces /etc/nginx/conf.d/default.conf). Servers should listen on 8080.
	NGINXConf string
	// ModuleImage replaces the stock NGF image (e.g. a sanitizer build).
	ModuleImage string
}

// Call is one request a stub received.
type Call struct {
	Method string      `json:"method"`
	URI    string      `json:"uri"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
}

// Stack is a running compose project.
type Stack struct {
	project string
	file    string
	conf    string // host path of the http-level config NGINX includes
	base    string // http://127.0.0.1:<port> of NGINX's 8080
	tlsAddr string // 127.0.0.1:<port> of NGINX's 8443
	control map[string]string
	client  *http.Client
}

var (
	stubOnce sync.Once
	stubErr  error
)

// Up starts a stack for cfg and tears it down when t finishes.
func Up(t *testing.T, cfg Config) *Stack {
	t.Helper()
	requireDocker(t)
	buildStub(t)

	so := ModuleSO(t)
	image := cfg.ModuleImage
	if image == "" {
		image = StockImage(t)
	}

	dir := confDir(t, cfg.NGINXConf)
	compose := fmt.Sprintf(composeTemplate, image, so, ModulePath,
		filepath.Join(dir, "ailuros.conf"), filepath.Join(dir, "harness.conf"),
		filepath.Join(dir, "default.conf"), filepath.Join(dir, "tls"), stubImage, stubImage, stubImage)
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, []byte(compose), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	s := &Stack{
		project: "ailuros-mod-it-" + randomHex(t),
		file:    file,
		conf:    filepath.Join(dir, "default.conf"),
		control: map[string]string{},
		client: &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("nginx log of %s:\n%s", s.project, s.logs())
		}
		_, _ = s.compose("down", "--volumes", "--remove-orphans", "--timeout", "1")
	})

	if out, err := s.compose("up", "--detach", "--quiet-pull"); err != nil {
		t.Fatalf("compose up: %v\n%s\n%s", err, out, s.logs())
	}

	s.base = "http://" + s.port(t, "nginx", "8080")
	s.tlsAddr = s.port(t, "nginx", "8443")
	for _, svc := range []string{"ailuros", "tinyauth", "echo"} {
		s.control[svc] = "http://" + s.port(t, svc, "9999")
	}
	s.waitReady(t)
	return s
}

// ModuleSO returns the absolute path of the module the stack mounts.
func ModuleSO(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AILUROS_MODULE_SO"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("AILUROS_MODULE_SO: %v", err)
		}
		return abs
	}
	ngf := NGFVersion(t)
	arch := dockerArch(t)
	p := filepath.Join(RepoRoot(t), "out",
		fmt.Sprintf("ngx_http_ailuros_module-dev-ngf%s-linux-%s.so", ngf, arch))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("module not built: %v (run make build NGF=%s ARCH=%s, or set AILUROS_MODULE_SO)", err, ngf, arch)
	}
	return p
}

// StockImage is the NGF data plane image for NGFVersion.
func StockImage(t *testing.T) string {
	t.Helper()
	return "ghcr.io/nginx/nginx-gateway-fabric/nginx:" + NGFVersion(t)
}

// NGFVersion is $AILUROS_NGF, or the first version in versions.json.
func NGFVersion(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("AILUROS_NGF"); v != "" {
		return v
	}
	versions, err := repo.LoadVersions(filepath.Join(RepoRoot(t), "versions.json"))
	if err != nil || len(versions) == 0 {
		t.Fatalf("versions.json: %v (%d rows)", err, len(versions))
	}
	return versions[0].NGF
}

// RepoRoot is the nearest directory above the working directory holding
// versions.json.
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "versions.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("versions.json not found above the working directory")
		}
		dir = parent
	}
}

// CheckConfig runs nginx -t, as uid 101 in the stack's image with the module
// loaded the same way, on conf included in the http block. It reports whether
// the test passed and nginx's output.
func CheckConfig(t *testing.T, conf string) (ok bool, output string) {
	t.Helper()
	requireDocker(t)
	so := ModuleSO(t)
	dir := confDir(t, conf)
	args := []string{"run", "--rm", "--user", "101:1001", "--entrypoint", "nginx",
		"--tmpfs", "/var/run/nginx:mode=1777",
		"-v", so + ":" + ModulePath + ":ro",
		"-v", filepath.Join(dir, "ailuros.conf") + ":/etc/nginx/main-includes/ailuros.conf:ro",
		"-v", filepath.Join(dir, "harness.conf") + ":/etc/nginx/main-includes/harness.conf:ro",
		"-v", filepath.Join(dir, "default.conf") + ":/etc/nginx/conf.d/default.conf:ro",
		"-v", filepath.Join(dir, "tls") + ":/etc/nginx/test-tls:ro",
		StockImage(t), "-t"}
	out, err := run(2*time.Minute, "docker", args...)
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	return err == nil, out
}

// SetAilurosRoutes makes the Ailuros stub answer with the status of the
// route with the longest key that is a prefix of the request's X-Original-URI, instead of the
// default answer's status.
func (s *Stack) SetAilurosRoutes(status int, routes map[string]int) {
	body, _ := json.Marshal(map[string]any{"status": status, "routes": routes})
	resp, err := s.client.Post(s.control["ailuros"]+"/answer", "application/json", bytes.NewReader(body))
	if err != nil {
		panic(fmt.Sprintf("set ailuros routes: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		panic(fmt.Sprintf("set ailuros routes: status %d", resp.StatusCode))
	}
}

// Base is NGINX's plain HTTP URL, http://127.0.0.1:<port>.
func (s *Stack) Base() string { return s.base }

// Client is the stack's default HTTP client (no redirects followed).
func (s *Stack) Client() *http.Client { return s.client }

// Dial opens a raw TCP connection to NGINX's 8080.
func (s *Stack) Dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(s.base, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial nginx: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TLSAddr is the host address of NGINX's TLS listener (container port 8443,
// certificate /etc/nginx/test-tls/cert.pem and key.pem, generated per test).
func (s *Stack) TLSAddr() string { return s.tlsAddr }

// TLSClient is an HTTP client for NGINX's TLS listener that accepts the test
// certificate and speaks HTTP/2 when the server offers it.
func (s *Stack) TLSClient() *http.Client {
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true},
	}
}

// SetAilurosAnswer makes the Ailuros stub answer every request with status,
// after delay, with headers.
func (s *Stack) SetAilurosAnswer(status int, delay time.Duration, headers map[string]string) {
	body, _ := json.Marshal(map[string]any{
		"status": status, "delay_ms": delay.Milliseconds(), "headers": headers,
	})
	resp, err := s.client.Post(s.control["ailuros"]+"/answer", "application/json", bytes.NewReader(body))
	if err != nil {
		panic(fmt.Sprintf("set ailuros answer: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		panic(fmt.Sprintf("set ailuros answer: status %d", resp.StatusCode))
	}
}

// StopAiluros makes the Ailuros stub stop listening on 9002, so NGINX's
// connections to it are refused (the container keeps its address, so this is
// a refusal rather than an unreachable host). Its calls stay readable.
func (s *Stack) StopAiluros() {
	resp, err := s.client.Post(s.control["ailuros"]+"/stop", "", nil)
	if err != nil {
		panic(fmt.Sprintf("stop ailuros: %v", err))
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		panic(fmt.Sprintf("stop ailuros: status %d", resp.StatusCode))
	}
}

// AilurosCalls, TinyauthCalls and EchoCalls return the requests each stub
// received so far.
func (s *Stack) AilurosCalls() []Call  { return s.calls("ailuros") }
func (s *Stack) TinyauthCalls() []Call { return s.calls("tinyauth") }
func (s *Stack) EchoCalls() []Call     { return s.calls("echo") }

func (s *Stack) calls(svc string) []Call {
	resp, err := s.client.Get(s.control[svc] + "/calls")
	if err != nil {
		panic(fmt.Sprintf("%s calls: %v", svc, err))
	}
	defer resp.Body.Close()
	var calls []Call
	if err := json.NewDecoder(resp.Body).Decode(&calls); err != nil {
		panic(fmt.Sprintf("%s calls: %v", svc, err))
	}
	return calls
}

// Get sends GET path to NGINX.
func (s *Stack) Get(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Do(t, req)
}

// Do sends req to NGINX (its URL's scheme and host are replaced; req.Host,
// if set, is sent as the Host header). The returned body is already read,
// so it need not be closed.
func (s *Stack) Do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	u := *req.URL
	base, _ := http.NewRequest(http.MethodGet, s.base, nil)
	u.Scheme, u.Host = base.URL.Scheme, base.URL.Host
	req.URL = &u
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, u.String(), err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("%s %s: read body: %v", req.Method, u.String(), err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp
}

// Reload replaces the http-level config with conf (the bind-mounted file is
// rewritten in place) and sends the NGINX master SIGHUP. NGINX reports a
// config that fails only in its error log, where the old config keeps
// serving.
func (s *Stack) Reload(t *testing.T, conf string) {
	t.Helper()
	if err := os.WriteFile(s.conf, []byte(conf), 0o644); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	// SIGHUP rather than nginx -s reload, which parses the new config first
	// and refuses to signal when it is invalid.
	if out, err := s.Exec(t, "sh", "-c", "kill -HUP $(cat /var/run/nginx/nginx.pid)"); err != nil {
		t.Fatalf("reload: %v\n%s", err, out)
	}
}

// Exec runs a command in the NGINX container as uid 101.
func (s *Stack) Exec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return s.compose(append([]string{"exec", "-T", "nginx"}, args...)...)
}

// ErrorLog returns NGINX's error log (stderr) so far.
func (s *Stack) ErrorLog() string {
	out, _ := run(30*time.Second, "docker", "compose", "-p", s.project, "-f", s.file,
		"logs", "--no-color", "--no-log-prefix", "nginx")
	return out
}

func (s *Stack) logs() string { return s.ErrorLog() }

func (s *Stack) compose(args ...string) (string, error) {
	return run(3*time.Minute, "docker", append([]string{"compose", "-p", s.project, "-f", s.file}, args...)...)
}

// state is a service container's state (running, exited, ...).
func (s *Stack) state(svc string) string {
	out, err := s.compose("ps", "--all", "--format", "{{.State}}", svc)
	if err != nil {
		return "unknown (" + strings.TrimSpace(out) + ")"
	}
	return strings.TrimSpace(out)
}

func (s *Stack) port(t *testing.T, svc, port string) string {
	t.Helper()
	out, err := s.compose("port", svc, port)
	if err != nil {
		t.Fatalf("compose port %s %s: %v\n%s\n%s", svc, port, err, out, s.logs())
	}
	return strings.TrimSpace(out)
}

func (s *Stack) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	urls := []string{s.base + "/__harness_ready"}
	for _, c := range s.control {
		urls = append(urls, c+"/ready")
	}
	for _, u := range urls {
		for {
			resp, err := s.client.Get(u)
			if err == nil {
				resp.Body.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("stack not ready: %s: %v\n%s", u, err, s.logs())
			}
			if state := s.state("nginx"); state != "running" && state != "created" && state != "restarting" {
				t.Fatalf("nginx container is %s, not running:\n%s", state, s.logs())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// confDir writes the config files mounted into NGINX.
func confDir(t *testing.T, conf string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestCert(t, filepath.Join(dir, "tls"))
	files := map[string]string{
		"ailuros.conf": "load_module " + ModulePath + ";\n",
		// NGF raises the error log level the same way; warnings must be seen.
		"harness.conf": "error_log stderr info;\n",
		"default.conf": conf,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeTestCert writes a self-signed certificate and key for localhost.
func writeTestCert(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"cert.pem": {Type: "CERTIFICATE", Bytes: der},
		"key.pem":  {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func buildStub(t *testing.T) {
	t.Helper()
	stubOnce.Do(func() {
		testDir := filepath.Join(RepoRoot(t), "test")
		out, err := run(10*time.Minute, "docker", "build", "--quiet", "-t", stubImage,
			"-f", filepath.Join(testDir, "harness", "Dockerfile"), testDir)
		if err != nil {
			stubErr = fmt.Errorf("build stub image: %v\n%s", err, out)
		}
	})
	if stubErr != nil {
		t.Fatal(stubErr)
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required for the behaviour suite")
	}
}

func dockerArch(t *testing.T) string {
	t.Helper()
	out, err := run(30*time.Second, "docker", "info", "--format", "{{.Architecture}}")
	if err != nil {
		t.Fatalf("docker info: %v\n%s", err, out)
	}
	switch a := strings.TrimSpace(out); a {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	default:
		t.Fatalf("unsupported Docker architecture %q", a)
		return ""
	}
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// The stub's -control port (9999) and NGINX's 8080 are published on
// loopback ephemeral ports; nothing else is reachable from the host.
const composeTemplate = `services:
  nginx:
    image: %s
    user: "101:1001"
    entrypoint: ["nginx", "-g", "daemon off;"]
    tmpfs:
      - /var/run/nginx:mode=1777
    volumes:
      - type: bind
        source: %s
        target: %s
        read_only: true
      - type: bind
        source: %s
        target: /etc/nginx/main-includes/ailuros.conf
        read_only: true
      - type: bind
        source: %s
        target: /etc/nginx/main-includes/harness.conf
        read_only: true
      - type: bind
        source: %s
        target: /etc/nginx/conf.d/default.conf
        read_only: true
      - type: bind
        source: %s
        target: /etc/nginx/test-tls
        read_only: true
    ports:
      - "127.0.0.1::8080"
      - "127.0.0.1::8443"
    depends_on: [ailuros, tinyauth, echo]
  ailuros:
    image: %s
    command: ["-role", "ailuros", "-listen", ":9002"]
    ports:
      - "127.0.0.1::9999"
  tinyauth:
    image: %s
    command: ["-role", "tinyauth", "-listen", ":3000"]
    ports:
      - "127.0.0.1::9999"
  echo:
    image: %s
    command: ["-role", "echo", "-listen", ":8080"]
    ports:
      - "127.0.0.1::9999"
`
