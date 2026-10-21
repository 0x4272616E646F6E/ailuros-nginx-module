package behaviour

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/harness"
)

const ailurosLocation = `
    location = /_ailuros {
        internal;
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_connect_timeout 1s;
        proxy_read_timeout 1s;
    }
`

const protected = `
    location /closed/ { ailuros_request /_ailuros; proxy_pass http://echo:8080; }
    location /open/   { ailuros_request /_ailuros; ailuros_request_unavailable open; proxy_pass http://echo:8080; }
`

// subrequestConf has one virtual host per case:
//
//	srvlevel  ailuros_request at server level, switched off in /off/
//	tryfiles  /_ailuros reaches Ailuros only through a named location
//	named     server-level error_page 502 504 = @ok (a named 200)
//	maint     server-level error_page 502 =200 /maint.html (internal)
const subrequestConf = `
server {
    listen 8080 default_server;
    server_name srvlevel;
    ailuros_request /_ailuros;
` + ailurosLocation + `
    location /app/ { proxy_pass http://echo:8080; }
    location /off/ { ailuros_request off; proxy_pass http://echo:8080; }
}

server {
    listen 8080;
    server_name tryfiles;

    location = /_ailuros {
        internal;
        try_files /nonexistent @ailuros;
    }
    location @ailuros {
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
    }
` + protected + `
}

server {
    listen 8080;
    server_name named;
    error_page 502 504 = @ok;
    location @ok { return 200; }
` + ailurosLocation + protected + `
}

server {
    listen 8080;
    server_name maint;
    error_page 502 =200 /maint.html;
    location = /maint.html { internal; return 200; }
` + ailurosLocation + protected + `
}
`

type subCase struct {
	name, host, path string
	wantClient       int
	wantUpstream     bool
	wantWarn         bool
	wantInternalErr  bool
	wantAilurosCalls int // -1: not checked
}

func runSubCases(t *testing.T, stack *harness.Stack, cases []subCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			echoBefore := len(stack.EchoCalls())
			ailurosBefore := len(stack.AilurosCalls())
			logBefore := stack.ErrorLog()

			req, err := http.NewRequest(http.MethodGet, tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = tc.host
			resp := stack.Do(t, req)

			if resp.StatusCode != tc.wantClient {
				t.Errorf("client status = %d, want %d", resp.StatusCode, tc.wantClient)
			}
			if got := len(stack.EchoCalls()) > echoBefore; got != tc.wantUpstream {
				t.Errorf("upstream called = %v, want %v", got, tc.wantUpstream)
			}
			if tc.wantAilurosCalls >= 0 {
				if got := len(stack.AilurosCalls()) - ailurosBefore; got != tc.wantAilurosCalls {
					t.Errorf("ailuros calls = %d, want %d", got, tc.wantAilurosCalls)
				}
			}
			newLog := waitLog(stack, logBefore, tc.wantWarn || tc.wantInternalErr)
			if got := hasLine(newLog, "[error]", "must be", "internal"); got != tc.wantInternalErr {
				t.Errorf("must-be-internal error logged = %v, want %v; new log:\n%s", got, tc.wantInternalErr, newLog)
			}
			if got := hasLine(newLog, "[warn]", "ailuros_request"); got != tc.wantWarn {
				t.Errorf("fail-open warning logged = %v, want %v; new log:\n%s", got, tc.wantWarn, newLog)
			}
		})
	}
}

// TestSubrequest covers what can happen inside the subrequest: inherited
// error_page, named locations, and the directive inherited from server.
func TestSubrequest(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: subrequestConf})
	stack.SetAilurosAnswer(200, 0, nil)

	// Ailuros up.
	runSubCases(t, stack, []subCase{
		// /_ailuros inherits ailuros_request but is never checked itself
		{"server level/checked once", "srvlevel", "/app/x", 200, true, false, false, 1},
		{"server level/off in location", "srvlevel", "/off/x", 200, true, false, false, 0},
		{"named location via try_files/closed", "tryfiles", "/closed/x", 503, false, false, true, 1},
		{"named location via try_files/open", "tryfiles", "/open/x", 200, true, true, true, 1},
	})

	// Ailuros down: the proxy's 502 must stay a 502 despite error_page.
	stack.StopAiluros()
	runSubCases(t, stack, []subCase{
		{"error_page to named 200/closed", "named", "/closed/x", 503, false, false, false, -1},
		{"error_page to named 200/open", "named", "/open/x", 200, true, true, false, -1},
		{"error_page =200 internal page/closed", "maint", "/closed/x", 503, false, false, false, -1},
		{"error_page =200 internal page/open", "maint", "/open/x", 200, true, true, false, -1},
	})
}

const reloadConf = `
server {
    listen 8080;
    ailuros_request_set $ailuros_request_id $upstream_http_x_ailuros_request_id;
` + ailurosLocation + `
    location /closed/ {
        ailuros_request /_ailuros;
        add_header X-Status $ailuros_request_status always;
        add_header X-Ailuros-Request-Id $ailuros_request_id always;
        proxy_pass http://echo:8080;
    }
}
`

// TestFailedReload reloads with a config that fails after the http block
// has started (so the module's preconfiguration ran for it), kills the
// worker so the master forks a new one from the old cycle, and checks that
// the new worker still applies answers and sets $ailuros_request_status.
func TestFailedReload(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: reloadConf})

	check := func(t *testing.T, status int) {
		t.Helper()
		stack.SetAilurosAnswer(status, 0, map[string]string{"X-Ailuros-Request-Id": "rid"})
		resp := stack.Get(t, "/closed/x")
		want := status
		if status < 300 {
			want = 200
		}
		if resp.StatusCode != want {
			t.Errorf("Ailuros %d: client status = %d, want %d", status, resp.StatusCode, want)
		}
		if got := resp.Header.Get("X-Status"); got != strconv.Itoa(status) {
			t.Errorf("Ailuros %d: $ailuros_request_status = %q", status, got)
		}
		if got := resp.Header.Get("X-Ailuros-Request-Id"); got != "rid" {
			t.Errorf("Ailuros %d: $ailuros_request_id = %q", status, got)
		}
	}
	check(t, 200)
	check(t, 403)

	// Extra variables before ours would shift indexes in the failed cycle.
	failing := `
map $host $a1 { default 1; }
map $host $a2 { default 2; }
server { listen 8081; set $x $a1$a2; ailuros_request_set $y $a1; location / { return 200; } }
` + reloadConf + `
server { listen 8082; no_such_directive; }
`
	stack.Reload(t, failing)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stack.ErrorLog(), "no_such_directive") {
		if time.Now().After(deadline) {
			t.Fatalf("failed reload not logged:\n%s", stack.ErrorLog())
		}
		time.Sleep(100 * time.Millisecond)
	}

	if out, err := stack.Exec(t, "sh", "-c", `kill -9 $(pgrep -f "^nginx: worker")`); err != nil {
		t.Fatalf("kill worker: %v\n%s", err, out)
	}
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(stack.ErrorLog(), "exited on signal 9") {
		if time.Now().After(deadline) {
			t.Fatalf("worker not respawned:\n%s", stack.ErrorLog())
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	check(t, 200)
	check(t, 403)
	check(t, 413)

	logs := stack.ErrorLog()
	for _, bad := range []string{"signal 11", "signal 6", "signal 7", "core dumped"} {
		if strings.Contains(logs, bad) {
			t.Errorf("error log has %q:\n%s", bad, logs)
		}
	}
}
