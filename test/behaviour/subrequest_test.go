package behaviour

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/decision-waf-nginx-module/test/harness"
)

const decisionWafLocation = `
    location = /_decision_waf {
        internal;
        proxy_pass http://decision-waf:9002;
        proxy_pass_request_body off;
        proxy_cache off;
        proxy_set_header Content-Length "";
        proxy_connect_timeout 1s;
        proxy_read_timeout 1s;
    }
`

const protected = `
    location /closed/ { decision_waf_request /_decision_waf; proxy_pass http://echo:8080; }
    location /open/   { decision_waf_request /_decision_waf; decision_waf_request_unavailable open; proxy_pass http://echo:8080; }
`

// subrequestConf has one virtual host per case:
//
//	srvlevel  decision_waf_request at server level, switched off in /off/
//	tryfiles  /_decision_waf reaches decision-waf only through a named location
//	named     server-level error_page 502 504 = @ok (a named 200)
//	maint     server-level error_page 502 504 =200 /maint.html (internal)
//	xaccel    the normal /_decision_waf, plus an internal /_ok decision-waf can
//	          X-Accel-Redirect to
//	rewrite   /_decision_waf rewrites to the internal /_ok
const subrequestConf = `
server {
    listen 8080 default_server;
    server_name srvlevel;
    decision_waf_request /_decision_waf;
` + decisionWafLocation + `
    location /app/ { proxy_pass http://echo:8080; }
    location /off/ { decision_waf_request off; proxy_pass http://echo:8080; }
}

server {
    listen 8080;
    server_name tryfiles;

    location = /_decision_waf {
        internal;
        try_files /nonexistent @decision_waf;
    }
    location @decision_waf {
        proxy_pass http://decision-waf:9002;
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
` + decisionWafLocation + protected + `
}

server {
    listen 8080;
    server_name maint;
    error_page 502 504 =200 /maint.html;
    location = /maint.html { internal; return 200; }
` + decisionWafLocation + protected + `
}

server {
    listen 8080;
    server_name xaccel;
    location = /_ok { internal; return 200; }
` + decisionWafLocation + protected + `
}

server {
    listen 8080;
    server_name rewrite;
    location = /_ok { internal; return 200; }
    location = /_decision_waf {
        internal;
        rewrite ^ /_ok last;
    }
` + protected + `
}
`

type subCase struct {
	name, host, path     string
	wantClient           int
	wantUpstream         bool
	wantWarn             bool
	wantInternalErr      bool
	wantDecisionWafCalls int // -1: not checked
}

func runSubCases(t *testing.T, stack *harness.Stack, cases []subCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			echoBefore := len(stack.EchoCalls())
			decisionWafBefore := len(stack.DecisionWafCalls())
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
			if tc.wantDecisionWafCalls >= 0 {
				if got := len(stack.DecisionWafCalls()) - decisionWafBefore; got != tc.wantDecisionWafCalls {
					t.Errorf("decision-waf calls = %d, want %d", got, tc.wantDecisionWafCalls)
				}
			}
			newLog := waitLog(stack, logBefore, tc.wantWarn || tc.wantInternalErr)
			if got := hasLine(newLog, "[error]", "decision_waf_request", "must"); got != tc.wantInternalErr {
				t.Errorf("must-be-internal error logged = %v, want %v; new log:\n%s", got, tc.wantInternalErr, newLog)
			}
			if got := hasLine(newLog, "[warn]", "decision_waf_request"); got != tc.wantWarn {
				t.Errorf("fail-open warning logged = %v, want %v; new log:\n%s", got, tc.wantWarn, newLog)
			}
		})
	}
}

// TestSubrequest covers what can happen inside the subrequest: inherited
// error_page, named locations, and the directive inherited from server.
func TestSubrequest(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: subrequestConf})
	stack.SetDecisionWafAnswer(200, 0, nil)

	// decision-waf up, answering by an X-Accel-Redirect to an internal 200.
	stack.SetDecisionWafAnswer(200, 0, map[string]string{"X-Accel-Redirect": "/_ok"})
	runSubCases(t, stack, []subCase{
		{"X-Accel-Redirect/closed", "xaccel", "/closed/x", 503, false, false, true, 1},
		{"X-Accel-Redirect/open", "xaccel", "/open/x", 200, true, true, true, 1},
	})
	stack.SetDecisionWafAnswer(200, 0, nil)

	// decision-waf up.
	runSubCases(t, stack, []subCase{
		{"rewrite last/closed", "rewrite", "/closed/x", 503, false, false, true, 0},
		{"rewrite last/open", "rewrite", "/open/x", 200, true, true, true, 0},
		// /_decision_waf inherits decision_waf_request but is never checked itself
		{"server level/checked once", "srvlevel", "/app/x", 200, true, false, false, 1},
		{"server level/off in location", "srvlevel", "/off/x", 200, true, false, false, 0},
		{"named location via try_files/closed", "tryfiles", "/closed/x", 503, false, false, true, 1},
		{"named location via try_files/open", "tryfiles", "/open/x", 200, true, true, true, 1},
	})

	// decision-waf down: the proxy's 502 must stay a 502 despite error_page.
	stack.StopDecisionWaf()
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
    decision_waf_request_set $decision_waf_request_id $upstream_http_x_decision_waf_request_id;
` + decisionWafLocation + `
    location /closed/ {
        decision_waf_request /_decision_waf;
        add_header X-Status $decision_waf_request_status always;
        add_header X-Decision-Waf-Request-Id $decision_waf_request_id always;
        proxy_pass http://echo:8080;
    }
}
`

// TestFailedReload pins the failed-reload and worker-respawn path: it
// reloads with a config that fails after the http block has started (so
// the module's preconfiguration ran for it), kills the worker so the master
// forks a new one from the old cycle, and checks that the new worker still
// applies answers and sets $decision_waf_request_status.
func TestFailedReload(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: reloadConf})

	check := func(t *testing.T, status int) {
		t.Helper()
		stack.SetDecisionWafAnswer(status, 0, map[string]string{"X-Decision-Waf-Request-Id": "rid"})
		resp := stack.Get(t, "/closed/x")
		want := status
		if status < 300 {
			want = 200
		}
		if resp.StatusCode != want {
			t.Errorf("decision-waf %d: client status = %d, want %d", status, resp.StatusCode, want)
		}
		if got := resp.Header.Get("X-Status"); got != strconv.Itoa(status) {
			t.Errorf("decision-waf %d: $decision_waf_request_status = %q", status, got)
		}
		if got := resp.Header.Get("X-Decision-Waf-Request-Id"); got != "rid" {
			t.Errorf("decision-waf %d: $decision_waf_request_id = %q", status, got)
		}
	}
	check(t, 200)
	check(t, 403)

	// Variables the old config does not have. They do not move our index
	// (it is taken in preconfiguration, before they are parsed), so this
	// does not reproduce a stale index; it exercises the path.
	failing := `
map $host $a1 { default 1; }
map $host $a2 { default 2; }
server { listen 8081; set $x $a1$a2; decision_waf_request_set $y $a1; location / { return 200; } }
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
