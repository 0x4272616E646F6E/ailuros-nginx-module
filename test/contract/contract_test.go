// Package contract runs the module against the published Ailuros, to check
// the wire contract the stub in the behaviour suite assumes: the headers
// Ailuros reads, the status codes and request ID it returns, and its decision
// log. The model behind Ailuros is a local stub (harness/stub/model.go).
package contract

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/harness"
)

// ailurosImage is ghcr.io/0x4272616e646f6e/ailuros:0.1.0, pinned by digest.
const ailurosImage = "ghcr.io/0x4272616e646f6e/ailuros@sha256:b7540bacf0a1729bba473f728119157cc584f28449aa52b763d5e502f41db273"

// The /_ailuros location is the one in deploy/ngf/snippetspolicy.yaml, read
// from that file and reindented, with only the Ailuros host swapped for the
// compose service name (see shippedLocation).
var conf = `
server {
    listen 8080;

` + shippedLocation() + `

    location /closed/ {
        ailuros_request /_ailuros;
        ailuros_request_set $ailuros_request_id $upstream_http_x_ailuros_request_id;
        add_header X-Status $ailuros_request_status always;
        add_header X-Ailuros-Request-Id $ailuros_request_id always;
        proxy_pass http://echo:8080/;
    }

    location /open/ {
        ailuros_request /_ailuros;
        ailuros_request_set $ailuros_request_id $upstream_http_x_ailuros_request_id;
        ailuros_request_unavailable open;
        add_header X-Status $ailuros_request_status always;
        add_header X-Ailuros-Request-Id $ailuros_request_id always;
        proxy_pass http://echo:8080/;
    }

    location = /__harness_ready { return 204; }
}
`

// shippedLocation returns the `location = /_ailuros { ... }` block of
// deploy/ngf/snippetspolicy.yaml, byte for byte (the `value: |` indentation
// replaced by the test config's), except that the Service host
// ailuros.default.svc.cluster.local becomes the compose service "ailuros" and
// the cluster DNS resolver becomes Docker's embedded DNS.
func shippedLocation() string {
	data, err := os.ReadFile("../../deploy/ngf/snippetspolicy.yaml")
	if err != nil {
		panic(err)
	}
	const indent = "        " // the snippet's indentation in the YAML
	var out []string
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == indent+"location = /_ailuros {" {
			in = true
		}
		if !in {
			continue
		}
		out = append(out, "    "+strings.TrimPrefix(line, indent))
		if line == indent+"}" {
			break
		}
	}
	loc := strings.Join(out, "\n")
	if !in || !strings.HasSuffix(loc, "    }") || !strings.Contains(loc, "set $ailuros_upstream ailuros.default.svc.cluster.local:9002;") ||
		!strings.Contains(loc, "resolver 10.96.0.10 ") {
		panic("deploy/ngf/snippetspolicy.yaml: /_ailuros location not found in the expected form")
	}
	loc = strings.Replace(loc, "ailuros.default.svc.cluster.local", "ailuros", 1)
	return strings.Replace(loc, "resolver 10.96.0.10 ", "resolver 127.0.0.11 ", 1)
}

var requestID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// decision is the fields of Ailuros's decision log line the test reads.
type decision struct {
	Msg       string `json:"msg"`
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Frontend  string `json:"frontend"`
	Host      string `json:"host"`
	ClientIP  string `json:"client_ip"`
}

// waitDecision polls Ailuros's log for the decision line of id.
func waitDecision(t *testing.T, s *harness.Stack, id string) decision {
	t.Helper()
	var log string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		log = s.AilurosLog(t)
		for _, line := range strings.Split(log, "\n") {
			var d decision
			if json.Unmarshal([]byte(line), &d) == nil && d.Msg == "decision" && d.RequestID == id {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no decision log line for request %s; ailuros log:\n%s", id, log)
		}
	}
}

// forgedIP is the client IP the forged headers claim (TEST-NET-3).
const forgedIP = "203.0.113.9"

// appHost is the Host every contract request is sent with; Ailuros must see
// it, not the /_ailuros proxy_pass host or a client's X-Forwarded-Host.
const appHost = "app.example"

func get(t *testing.T, s *harness.Stack, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = appHost
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return s.Do(t, req)
}

func TestRealAilurosContract(t *testing.T) {
	s := harness.Up(t, harness.Config{NGINXConf: conf, RealAiluros: ailurosImage})

	// forged are client headers that would break or fool a proxy that passed
	// them on: with X-Forwarded-Uri next to X-Original-URI Ailuros answers 500,
	// a forged X-Original-URI would have the wrong path classified, a forged
	// X-Forwarded-Host would pick another route's policy, and a forged
	// X-Real-IP or X-Forwarded-For would set the client IP.
	forged := map[string]string{
		"X-Original-URI":     "/safe",
		"X-Forwarded-Uri":    "/safe",
		"X-Forwarded-Method": "GET",
		"X-Forwarded-Host":   "forged.example",
		"X-Real-IP":          forgedIP,
		"X-Forwarded-For":    forgedIP,
	}

	cases := []struct {
		name        string
		path        string // under /closed/
		wantStatus  int
		wantOutcome string
		wantPath    string
		wantEcho    bool
	}{
		{"allow", "/closed/hello?page=2", 200, "allow", "/closed/hello", true},
		{"block", "/closed/search?q=attack", 403, "block", "/closed/search", false},
	}
	for _, withForged := range []bool{false, true} {
		for _, tc := range cases {
			name := tc.name
			var h map[string]string
			if withForged {
				name += " with forged headers"
				h = forged
			}
			t.Run(name, func(t *testing.T) {
				before := len(s.EchoCalls())
				resp := get(t, s, tc.path, h)
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
				}
				if got := len(s.EchoCalls()) - before; (got == 1) != tc.wantEcho || got > 1 {
					t.Errorf("echo called %d times, want called=%v", got, tc.wantEcho)
				}
				id := resp.Header.Get("X-Ailuros-Request-Id")
				if !requestID.MatchString(id) {
					t.Fatalf("X-Ailuros-Request-Id = %q, want 16 hex characters", id)
				}
				d := waitDecision(t, s, id)
				if d.Outcome != tc.wantOutcome || d.Method != "GET" || d.Path != tc.wantPath || d.Frontend != "forwardauth" {
					t.Errorf("decision = %+v, want outcome=%s method=GET path=%s frontend=forwardauth", d, tc.wantOutcome, tc.wantPath)
				}
				if d.Host != appHost {
					t.Errorf("decision host = %q, want %q (the client's Host)", d.Host, appHost)
				}
				if ip := net.ParseIP(d.ClientIP); ip == nil || d.ClientIP == forgedIP {
					t.Errorf("decision client_ip = %q, want NGINX's $remote_addr", d.ClientIP)
				}
				t.Logf("status %d, request id %s, decision %+v", resp.StatusCode, id, d)
			})
		}
	}

	t.Run("model saw the classified request", func(t *testing.T) {
		var attack, benign int
		for _, c := range s.ModelCalls() {
			if strings.Contains(string(c.Body), "q=attack") {
				attack++
			} else {
				benign++
			}
		}
		if attack == 0 || benign == 0 {
			t.Errorf("model calls: %d with the attack query, %d without; want both", attack, benign)
		}
	})

	s.StopAiluros()

	t.Run("ailuros down, closed", func(t *testing.T) {
		before := len(s.EchoCalls())
		resp := get(t, s, "/closed/hello", nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", resp.StatusCode)
		}
		if got := len(s.EchoCalls()) - before; got != 0 {
			t.Errorf("echo called %d times, want 0", got)
		}
	})

	t.Run("ailuros down, open", func(t *testing.T) {
		before := len(s.EchoCalls())
		logBefore := s.ErrorLog()
		resp := get(t, s, "/open/hello", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if got := len(s.EchoCalls()) - before; got != 1 {
			t.Errorf("echo called %d times, want 1", got)
		}
		var warned bool
		for _, line := range strings.Split(strings.TrimPrefix(s.ErrorLog(), logBefore), "\n") {
			if strings.Contains(line, "[warn]") && strings.Contains(line, "ailuros_request") {
				warned = true
				t.Logf("warning: %s", line)
			}
		}
		if !warned {
			t.Errorf("no fail-open [warn] line mentioning ailuros_request in the nginx log")
		}
	})
}
