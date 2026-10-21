package behaviour

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/harness"
)

// tinyauthLocation is tinyauth's forward-auth location, and @error401 the
// homelab's redirect to its login page.
const tinyauthLocation = `
    location = /auth {
        internal;
        proxy_pass http://tinyauth:3000;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
    }
    location @error401 { return 302 http://login.example/?rd=$request_uri; }
`

// exposedLocation proxies to Ailuros without "internal": its answers are
// always no decision.
const exposedLocation = `
    location = /_exposed {
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
    }
`

// orderConf has one virtual host per case. ailuros_request is set at server
// level wherever named locations must inherit it, so that a second check
// would happen if the result were not reused. Named locations that must reach
// the preaccess phase proxy to echo: "return" ends a request in the rewrite
// phase, before any check could run.
//
//	stack          ailuros_request, then auth_request /auth (tinyauth)
//	satisfy        the same with "satisfy any; allow all;"
//	satisfy-deny   the same with "satisfy any; deny all;"
//	limited        limit_req at 1r/m, no burst, nodelay
//	named          an upstream 404 sent to @named by error_page
//	rewrite        rewrite ^/old/ /new/ last before the check
//	blocked        error_page 403 = @blocked (inherits, proxies) and
//	               error_page 403 = @page (a "return" block page)
//	exposed-closed no decision (location not internal), closed, with
//	               error_page 503 = @named
//	exposed-open   no decision, open, with an upstream 404 sent to @named
const orderConf = `
limit_req_zone $binary_remote_addr zone=ailuros_order:1m rate=1r/m;

server {
    listen 8081;
    return 404;
}

server {
    listen 8080 default_server;
    server_name stack;
    ailuros_request /_ailuros;
` + ailurosLocation + tinyauthLocation + `
    location /app/ {
        auth_request /auth;
        error_page 401 = @error401;
        proxy_pass http://echo:8080;
    }
}

server {
    listen 8080;
    server_name satisfy;
    ailuros_request /_ailuros;
` + ailurosLocation + tinyauthLocation + `
    location /app/ {
        satisfy any;
        allow all;
        auth_request /auth;
        error_page 401 = @error401;
        proxy_pass http://echo:8080;
    }
}

server {
    listen 8080;
    server_name satisfy-deny;
    ailuros_request /_ailuros;
` + ailurosLocation + tinyauthLocation + `
    location /app/ {
        satisfy any;
        deny all;
        auth_request /auth;
        error_page 401 = @error401;
        proxy_pass http://echo:8080;
    }
}

server {
    listen 8080;
    server_name limited;
` + ailurosLocation + `
    location /app/ {
        # burst defaults to 0; NGINX rejects an explicit "burst=0"
        limit_req zone=ailuros_order nodelay;
        ailuros_request /_ailuros;
        proxy_pass http://echo:8080;
    }
}

server {
    listen 8080;
    server_name named;
    ailuros_request /_ailuros;
    ailuros_request_set $ailuros_request_id $upstream_http_x_ailuros_request_id;
` + ailurosLocation + `
    location /app/ {
        proxy_pass http://127.0.0.1:8081;
        proxy_intercept_errors on;
        error_page 404 = @named;
    }
    location @named {
        add_header X-Ailuros-Request-Id $ailuros_request_id always;
        add_header X-Status $ailuros_request_status always;
        proxy_pass http://echo:8080;
    }
}

server {
    listen 8080;
    server_name rewrite;
    ailuros_request /_ailuros;
` + ailurosLocation + `
    location /old/ { rewrite ^/old/(.*)$ /new/$1 last; }
    location /new/ { proxy_pass http://echo:8080; }
}

server {
    listen 8080;
    server_name blocked;
    ailuros_request /_ailuros;
` + ailurosLocation + `
    location /inherits/ {
        error_page 403 = @blocked;
        proxy_pass http://echo:8080;
    }
    location @blocked { proxy_pass http://echo:8080; }
    location /page/ {
        error_page 403 = @page;
        proxy_pass http://echo:8080;
    }
    location @page { return 403 "custom block page\n"; }
}

server {
    listen 8080;
    server_name exposed-closed;
    ailuros_request /_exposed;
` + exposedLocation + `
    location /app/ {
        error_page 503 = @named;
        proxy_pass http://echo:8080;
    }
    location @named { proxy_pass http://echo:8080; }
}

server {
    listen 8080;
    server_name exposed-open;
    ailuros_request /_exposed;
    ailuros_request_unavailable open;
` + exposedLocation + `
    location /app/ {
        proxy_pass http://127.0.0.1:8081;
        proxy_intercept_errors on;
        error_page 404 = @named;
    }
    location @named { proxy_pass http://echo:8080; }
}
`

type orderResult struct {
	status            int
	body              string
	header            http.Header
	ailuros, tinyauth int // calls during the request
	echo              int
	newLog            string
	logBefore         string
}

// settledLog waits for the error log to settle and returns what the request
// added, for counting lines.
func (r orderResult) settledLog(stack *harness.Stack) string {
	time.Sleep(500 * time.Millisecond)
	return strings.TrimPrefix(stack.ErrorLog(), r.logBefore)
}

func orderGet(t *testing.T, stack *harness.Stack, host, path string, wantLog bool) orderResult {
	t.Helper()
	ailurosBefore := len(stack.AilurosCalls())
	tinyauthBefore := len(stack.TinyauthCalls())
	echoBefore := len(stack.EchoCalls())
	logBefore := stack.ErrorLog()

	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp := stack.Do(t, req)
	body, _ := io.ReadAll(resp.Body)

	return orderResult{
		status:   resp.StatusCode,
		body:     string(body),
		header:   resp.Header,
		ailuros:  len(stack.AilurosCalls()) - ailurosBefore,
		tinyauth: len(stack.TinyauthCalls()) - tinyauthBefore,
		echo:     len(stack.EchoCalls()) - echoBefore,
		newLog:   waitLog(stack, logBefore, wantLog),

		logBefore: logBefore,
	}
}

func countLines(log string, subs ...string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if hasLine(line, subs...) {
			n++
		}
	}
	return n
}

func (r orderResult) check(t *testing.T, status, ailuros, tinyauth int) {
	t.Helper()
	if r.status != status {
		t.Errorf("client status = %d, want %d (body %q)", r.status, status, r.body)
	}
	if r.ailuros != ailuros {
		t.Errorf("ailuros calls = %d, want %d", r.ailuros, ailuros)
	}
	if tinyauth >= 0 && r.tinyauth != tinyauth {
		t.Errorf("tinyauth calls = %d, want %d", r.tinyauth, tinyauth)
	}
}

func TestRunsBeforeAuthRequest(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: orderConf})

	t.Run("ailuros 403 blocks before tinyauth", func(t *testing.T) {
		stack.SetAilurosAnswer(403, 0, nil)
		r := orderGet(t, stack, "stack", "/app/x", false)
		r.check(t, 403, 1, 0)
	})
	t.Run("ailuros 200 then tinyauth 401 redirects to login", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		r := orderGet(t, stack, "stack", "/app/x", false)
		r.check(t, 302, 1, 1)
		if loc := r.header.Get("Location"); !strings.HasPrefix(loc, "http://login.example/") {
			t.Errorf("Location = %q, want the login URL", loc)
		}
		if r.echo != 0 {
			t.Errorf("upstream calls = %d, want 0", r.echo)
		}
	})
}

func TestSatisfyAnyDoesNotSkip(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: orderConf})

	for _, host := range []string{"satisfy", "satisfy-deny"} {
		t.Run(host+"/ailuros 403 still blocks", func(t *testing.T) {
			stack.SetAilurosAnswer(403, 0, nil)
			r := orderGet(t, stack, host, "/app/x", false)
			r.check(t, 403, 1, 0)
			if r.echo != 0 {
				t.Errorf("upstream calls = %d, want 0", r.echo)
			}
		})
	}

	// "satisfy any" combines only access-phase handlers, and an Ailuros
	// allow is not one of them: with "deny all", tinyauth decides alone.
	t.Run("satisfy-deny/ailuros 200 does not stand in for tinyauth", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		r := orderGet(t, stack, "satisfy-deny", "/app/x", false)
		r.check(t, 302, 1, 1)
		if r.echo != 0 {
			t.Errorf("upstream calls = %d, want 0", r.echo)
		}
	})

	// With "allow all", NGINX's access module (which runs before
	// auth_request in the access phase) satisfies "satisfy any" on its own,
	// so tinyauth is not asked, whatever Ailuros said. Ailuros is still
	// asked once.
	t.Run("satisfy/allow all satisfies the access phase", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		r := orderGet(t, stack, "satisfy", "/app/x", false)
		r.check(t, 200, 1, 0)
	})
}

func TestLimitReqFirst(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: orderConf})
	stack.SetAilurosAnswer(200, 0, nil)

	first := orderGet(t, stack, "limited", "/app/1", false)
	first.check(t, 200, 1, 0)
	second := orderGet(t, stack, "limited", "/app/2", false)
	second.check(t, 503, 0, 0)
	if !hasLine(second.newLog, "limiting requests") {
		t.Errorf("limit_req did not reject the second request; new log:\n%s", second.newLog)
	}
}

func TestOncePerRequest(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: orderConf})

	t.Run("error_page 404 = @named after an upstream 404", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, map[string]string{"X-Ailuros-Request-Id": "rid-named"})
		r := orderGet(t, stack, "named", "/app/x", false)
		r.check(t, 200, 1, 0)
		if r.echo != 1 {
			t.Errorf("upstream (@named) calls = %d, want 1", r.echo)
		}
		if got := r.header.Get("X-Ailuros-Request-Id"); got != "rid-named" {
			t.Errorf("$ailuros_request_id in @named = %q, want rid-named", got)
		}
		if got := r.header.Get("X-Status"); got != "200" {
			t.Errorf("$ailuros_request_status in @named = %q, want 200", got)
		}
	})

	t.Run("tinyauth @error401", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		r := orderGet(t, stack, "stack", "/app/x", false)
		r.check(t, 302, 1, 1)
	})

	t.Run("rewrite last before the check", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		r := orderGet(t, stack, "rewrite", "/old/x", false)
		r.check(t, 200, 1, 0)
		if r.echo != 1 {
			t.Errorf("upstream calls = %d, want 1", r.echo)
		}
	})

	t.Run("server level, /auth and /_ailuros in the same server", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		before := len(stack.TinyauthCalls())
		req, _ := http.NewRequest(http.MethodGet, "/app/x", nil)
		req.Host = "stack"
		req.Header.Set("Authorization", "ok")
		ailurosBefore := len(stack.AilurosCalls())
		resp := stack.Do(t, req)
		if resp.StatusCode != 200 {
			t.Errorf("client status = %d, want 200", resp.StatusCode)
		}
		if got := len(stack.AilurosCalls()) - ailurosBefore; got != 1 {
			t.Errorf("ailuros calls = %d, want 1", got)
		}
		if got := len(stack.TinyauthCalls()) - before; got != 1 {
			t.Errorf("tinyauth calls = %d, want 1", got)
		}
	})

	// A reused result reproduces the decision, not only the status.
	t.Run("blocked, error_page to a location that inherits", func(t *testing.T) {
		stack.SetAilurosAnswer(403, 0, nil)
		r := orderGet(t, stack, "blocked", "/inherits/x", false)
		r.check(t, 403, 1, 0)
		if r.echo != 0 {
			t.Errorf("upstream calls = %d, want 0: @blocked was let through", r.echo)
		}
	})
	t.Run("blocked, error_page to a return block page", func(t *testing.T) {
		stack.SetAilurosAnswer(403, 0, nil)
		r := orderGet(t, stack, "blocked", "/page/x", false)
		r.check(t, 403, 1, 0)
		if r.body != "custom block page\n" {
			t.Errorf("body = %q, want the custom block page", r.body)
		}
	})
	t.Run("no decision (not internal), closed, error_page 503 = @named", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		r := orderGet(t, stack, "exposed-closed", "/app/x", true)
		r.check(t, 503, 1, 0)
		if r.echo != 0 {
			t.Errorf("upstream calls = %d, want 0: @named was let through", r.echo)
		}
		log := r.settledLog(stack)
		if n := countLines(log, "[error]", "ailuros_request", "internal"); n != 1 {
			t.Errorf("must-be-internal errors = %d, want 1; new log:\n%s", n, log)
		}
	})
	t.Run("no decision (not internal), open, upstream 404 to @named", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		r := orderGet(t, stack, "exposed-open", "/app/x", true)
		r.check(t, 200, 1, 0)
		if r.echo != 1 {
			t.Errorf("upstream (@named) calls = %d, want 1", r.echo)
		}
		log := r.settledLog(stack)
		if n := countLines(log, "[warn]", "ailuros_request"); n != 1 {
			t.Errorf("fail-open warnings = %d, want 1; new log:\n%s", n, log)
		}
	})
}

// TestStoredDecisionNotWritable pins that the configuration cannot define the
// variable the module keeps its decision in: a pre-set value would be
// reused as a decision.
func TestStoredDecisionNotWritable(t *testing.T) {
	for name, conf := range map[string]string{
		"map": `map $host $ailuros-request-outcome { default continue; }
server { listen 8080; location / { return 200; } }`,
		"ailuros_request_set": `server { listen 8080; ailuros_request_set $ailuros-request-outcome continue; location / { return 200; } }`,
		"set":                 `server { listen 8080; location / { set $ailuros-request-outcome continue; return 200; } }`,
	} {
		t.Run(name, func(t *testing.T) {
			ok, out := harness.CheckConfig(t, conf)
			if ok || !strings.Contains(out, `the duplicate "ailuros-request-outcome" variable`) {
				t.Errorf("nginx -t ok = %v, want a duplicate variable error; output:\n%s", ok, out)
			}
		})
	}
}
