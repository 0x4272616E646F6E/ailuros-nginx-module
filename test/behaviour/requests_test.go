package behaviour

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/harness"
)

// requestsConf serves plain HTTP on 8080 and HTTP/2 over TLS on 8443 from
// one server, so each test can compare the two. /c/ proxies to echo; /ep/
// proxies to a backend that answers 404, sent to @named by error_page.
const requestsConf = `
server {
    listen 8081;
    return 404;
}

server {
    listen 8080;
    listen 8443 ssl;
    http2 on;
    ssl_certificate     /etc/nginx/test-tls/cert.pem;
    ssl_certificate_key /etc/nginx/test-tls/key.pem;

    large_client_header_buffers 4 32k;
    client_max_body_size 4m;

    ailuros_request /_ailuros;

    location = /_ailuros {
        internal;
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_set_header X-Original-URI $request_uri;
        proxy_set_header X-Original-Method $request_method;
        proxy_connect_timeout 1s;
        proxy_read_timeout 5s;
    }

    location /c/ { proxy_pass http://echo:8080; }

    location /ep/ {
        proxy_pass http://127.0.0.1:8081;
        proxy_intercept_errors on;
        error_page 404 = @named;
    }
    location @named { proxy_pass http://echo:8080; }
}
`

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMethodsAndBody(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: requestsConf})
	stack.SetAilurosAnswer(200, 0, nil)

	body64k := randomBytes(t, 64*1024)
	cases := []struct {
		method string
		body   []byte
	}{
		{http.MethodGet, nil},
		{http.MethodHead, nil},
		{http.MethodPost, body64k},
		{http.MethodPut, []byte("small put body")},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			uri := "/c/" + strings.ToLower(tc.method) + "?a=1&b=two%20words"
			ailurosBefore, echoBefore := len(stack.AilurosCalls()), len(stack.EchoCalls())

			var rd io.Reader
			if tc.body != nil {
				rd = bytes.NewReader(tc.body)
			}
			req, err := http.NewRequest(tc.method, uri, rd)
			if err != nil {
				t.Fatal(err)
			}
			resp := stack.Do(t, req)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			ac := stack.AilurosCalls()[ailurosBefore:]
			if len(ac) != 1 {
				t.Fatalf("ailuros calls = %d, want 1", len(ac))
			}
			if got := ac[0].Header.Get("X-Original-Method"); got != tc.method {
				t.Errorf("X-Original-Method = %q, want %q", got, tc.method)
			}
			if got := ac[0].Header.Get("X-Original-URI"); got != uri {
				t.Errorf("X-Original-URI = %q, want %q", got, uri)
			}
			if len(ac[0].Body) != 0 {
				t.Errorf("ailuros received a %d-byte body, want none", len(ac[0].Body))
			}

			ec := stack.EchoCalls()[echoBefore:]
			if len(ec) != 1 {
				t.Fatalf("echo calls = %d, want 1", len(ec))
			}
			if ec[0].Method != tc.method {
				t.Errorf("echo method = %q, want %q", ec[0].Method, tc.method)
			}
			if !bytes.Equal(ec[0].Body, tc.body) {
				t.Errorf("echo body = %d bytes, want %d identical bytes", len(ec[0].Body), len(tc.body))
			}
		})
	}
}

// readResponse reads one response from br, draining its body.
func readResponse(t *testing.T, br *bufio.Reader, method string) *http.Response {
	t.Helper()
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// TestExpectContinue pins when NGINX sends "100 Continue": it must not before
// the check, so a blocked request's body is never invited, let alone sent.
func TestExpectContinue(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: requestsConf})
	body := randomBytes(t, 1<<20)
	head := fmt.Sprintf("POST /c/expect HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: %d\r\n\r\n", len(body))

	t.Run("allow", func(t *testing.T) {
		const delay = 1 * time.Second
		stack.SetAilurosAnswer(200, delay, nil)
		ailurosBefore, echoBefore := len(stack.AilurosCalls()), len(stack.EchoCalls())

		conn := stack.Dial(t)
		br := bufio.NewReader(conn)
		start := time.Now()
		if _, err := io.WriteString(conn, head); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for 100 Continue: %v", err)
		}
		took := time.Since(start)
		if !strings.HasPrefix(line, "HTTP/1.1 100 Continue") {
			t.Fatalf("first line = %q, want 100 Continue", line)
		}
		if _, err := br.ReadString('\n'); err != nil { // blank line
			t.Fatal(err)
		}
		if took < delay-100*time.Millisecond {
			t.Errorf("100 Continue after %v, before Ailuros's %v answer", took, delay)
		}
		if got := len(stack.AilurosCalls()) - ailurosBefore; got != 1 {
			t.Errorf("ailuros calls when 100 Continue arrived = %d, want 1", got)
		}
		if got := len(stack.EchoCalls()) - echoBefore; got != 0 {
			t.Errorf("echo calls before the body was sent = %d, want 0", got)
		}

		if _, err := conn.Write(body); err != nil {
			t.Fatal(err)
		}
		resp := readResponse(t, br, http.MethodPost)
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		ec := stack.EchoCalls()[echoBefore:]
		if len(ec) != 1 || !bytes.Equal(ec[0].Body, body) {
			t.Errorf("echo calls = %d, want 1 with the full %d-byte body", len(ec), len(body))
		}
	})

	t.Run("block", func(t *testing.T) {
		stack.SetAilurosAnswer(403, 0, nil)
		ailurosBefore, echoBefore := len(stack.AilurosCalls()), len(stack.EchoCalls())

		conn := stack.Dial(t)
		br := bufio.NewReader(conn)
		if _, err := io.WriteString(conn, head); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for the response: %v", err)
		}
		if !strings.HasPrefix(line, "HTTP/1.1 403") {
			t.Fatalf("first line = %q, want 403 with no 100 Continue before it", line)
		}
		// The client, having had no 100, sends no body. Anything it sends
		// regardless must not reach the upstream either.
		time.Sleep(300 * time.Millisecond)
		if got := len(stack.AilurosCalls()) - ailurosBefore; got != 1 {
			t.Errorf("ailuros calls = %d, want 1", got)
		}
		if got := len(stack.EchoCalls()) - echoBefore; got != 0 {
			t.Errorf("echo calls = %d, want 0", got)
		}
	})
}

func TestHTTP2(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: requestsConf})
	client := stack.TLSClient()
	url := func(path string) string { return "https://" + stack.TLSAddr() + path }

	get := func(t *testing.T, path string) *http.Response {
		t.Helper()
		resp, err := client.Get(url(path))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("protocol = %s, want HTTP/2", resp.Proto)
		}
		return resp
	}

	t.Run("allow", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		a, e := len(stack.AilurosCalls()), len(stack.EchoCalls())
		resp := get(t, "/c/h2?x=1")
		if resp.StatusCode != 200 {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		ac := stack.AilurosCalls()[a:]
		if len(ac) != 1 || ac[0].Header.Get("X-Original-URI") != "/c/h2?x=1" || ac[0].Header.Get("X-Original-Method") != "GET" {
			t.Errorf("ailuros calls = %+v, want one for GET /c/h2?x=1", ac)
		}
		if got := len(stack.EchoCalls()) - e; got != 1 {
			t.Errorf("echo calls = %d, want 1", got)
		}
	})
	t.Run("block", func(t *testing.T) {
		stack.SetAilurosAnswer(403, 0, nil)
		a, e := len(stack.AilurosCalls()), len(stack.EchoCalls())
		resp := get(t, "/c/h2-blocked")
		if resp.StatusCode != 403 {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
		if got := len(stack.AilurosCalls()) - a; got != 1 {
			t.Errorf("ailuros calls = %d, want 1", got)
		}
		if got := len(stack.EchoCalls()) - e; got != 0 {
			t.Errorf("echo calls = %d, want 0", got)
		}
	})
	t.Run("once per request with error_page", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		a, e := len(stack.AilurosCalls()), len(stack.EchoCalls())
		resp := get(t, "/ep/h2")
		if resp.StatusCode != 200 {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if got := len(stack.AilurosCalls()) - a; got != 1 {
			t.Errorf("ailuros calls = %d, want 1", got)
		}
		if got := len(stack.EchoCalls()) - e; got != 1 {
			t.Errorf("echo (@named) calls = %d, want 1", got)
		}
	})
	t.Run("post body", func(t *testing.T) {
		stack.SetAilurosAnswer(200, 0, nil)
		e := len(stack.EchoCalls())
		body := randomBytes(t, 256*1024)
		resp, err := client.Post(url("/c/h2-post"), "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		ec := stack.EchoCalls()[e:]
		if resp.StatusCode != 200 || len(ec) != 1 || !bytes.Equal(ec[0].Body, body) {
			t.Errorf("status = %d, echo calls = %d; want 200 and the full body", resp.StatusCode, len(ec))
		}
	})
}

func TestLargeHeaders(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: requestsConf})
	stack.SetAilurosAnswer(200, 0, nil)

	a0, e0 := len(stack.AilurosCalls()), len(stack.EchoCalls())
	cookie := "session=" + strings.Repeat("a", 16*1024-len("session="))
	req, _ := http.NewRequest(http.MethodGet, "/c/large", nil)
	req.Header.Set("Cookie", cookie)
	resp := stack.Do(t, req)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ac, ec := stack.AilurosCalls()[a0:], stack.EchoCalls()[e0:]
	if len(ac) != 1 || len(ec) != 1 {
		t.Fatalf("ailuros calls = %d, echo calls = %d, want 1 each", len(ac), len(ec))
	}
	if got := ac[0].Header.Get("Cookie"); got != cookie {
		t.Errorf("ailuros saw a %d-byte Cookie, want the %d-byte original", len(got), len(cookie))
	}
	if got := ec[0].Header.Get("Cookie"); got != cookie {
		t.Errorf("echo saw a %d-byte Cookie, want the %d-byte original", len(got), len(cookie))
	}
}

// TestKeepaliveAfterBlock: one connection, a blocked request then an allowed
// one. The second must be asked again; a decision left over from the first
// would block it.
func TestKeepaliveAfterBlock(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: requestsConf})
	stack.SetAilurosRoutes(200, map[string]int{"/c/blocked": 403})

	a0, e0 := len(stack.AilurosCalls()), len(stack.EchoCalls())
	conn := stack.Dial(t)
	br := bufio.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	send := func(path string) *http.Response {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
			t.Fatal(err)
		}
		return readResponse(t, br, http.MethodGet)
	}

	if resp := send("/c/blocked"); resp.StatusCode != 403 {
		t.Fatalf("first status = %d, want 403", resp.StatusCode)
	}
	if resp := send("/c/allowed"); resp.StatusCode != 200 {
		t.Fatalf("second status (same connection) = %d, want 200", resp.StatusCode)
	}
	ac, ec := stack.AilurosCalls()[a0:], stack.EchoCalls()[e0:]
	if len(ac) != 2 {
		t.Errorf("ailuros calls = %d, want 2 (one per request)", len(ac))
	}
	if len(ec) != 1 || ec[0].URI != "/c/allowed" {
		t.Errorf("echo calls = %+v, want only /c/allowed", ec)
	}
}

func TestKeepaliveAndConcurrency(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: requestsConf})
	stack.SetAilurosRoutes(200, map[string]int{"/c/b/": 403, "/c/r/": 413, "/c/w/": 400})

	const conns, perConn = 20, 10
	before := len(stack.AilurosCalls())
	echoBefore := len(stack.EchoCalls())
	kinds := []struct {
		prefix string
		status int
	}{{"/c/ok/", 200}, {"/c/b/", 403}, {"/c/r/", 413}, {"/c/w/", 400}}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		reused  int
		errs    []string
		allowed = map[string]bool{}
	)
	addr := strings.TrimPrefix(stack.Base(), "http://")
	for c := 0; c < conns; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
			for i := 0; i < perConn; i++ {
				n := c*perConn + i
				k := kinds[n%len(kinds)]
				uri := fmt.Sprintf("%s%d", k.prefix, n)
				wasReused := false
				ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) { wasReused = info.Reused },
				})
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+uri, nil)
				resp, err := client.Do(req)
				mu.Lock()
				if err != nil {
					errs = append(errs, uri+": "+err.Error())
				} else {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != k.status {
						errs = append(errs, fmt.Sprintf("%s: status %d, want %d", uri, resp.StatusCode, k.status))
					}
					if k.status == 200 {
						allowed[uri] = true
					}
					if wasReused {
						reused++
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	for _, e := range errs {
		t.Error(e)
	}
	// NGINX closes the connection after a 400 or 413 answer, so not every
	// request after a connection's first reuses it; 200 and 403 do.
	if reused < conns {
		t.Errorf("reused connections = %d, want keepalive in use (at least %d)", reused, conns)
	}
	if got := len(stack.AilurosCalls()) - before; got != conns*perConn {
		t.Errorf("ailuros calls = %d, want %d", got, conns*perConn)
	}
	seen := map[string]int{}
	echoCalls := stack.EchoCalls()[echoBefore:]
	for _, c := range echoCalls {
		seen[c.URI]++
	}
	if len(seen) != len(allowed) || len(echoCalls) != len(allowed) {
		t.Errorf("echo got %d calls for %d URIs, want exactly the %d allowed ones once each",
			len(echoCalls), len(seen), len(allowed))
	}
	for uri := range allowed {
		if seen[uri] != 1 {
			t.Errorf("echo calls for %s = %d, want 1", uri, seen[uri])
		}
	}
}

func TestClientHangsUp(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: requestsConf})
	stack.SetAilurosAnswer(200, 2*time.Second, nil)
	a0, e0 := len(stack.AilurosCalls()), len(stack.EchoCalls())

	conn := stack.Dial(t)
	if _, err := io.WriteString(conn, "GET /c/hangup HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(stack.AilurosCalls()) == a0 {
		if time.Now().After(deadline) {
			t.Fatal("ailuros was never asked")
		}
		time.Sleep(50 * time.Millisecond)
	}
	conn.Close()
	time.Sleep(3 * time.Second)

	stack.SetAilurosAnswer(200, 0, nil)
	if resp := stack.Get(t, "/c/after"); resp.StatusCode != 200 {
		t.Errorf("request after the hang-up: status = %d, want 200", resp.StatusCode)
	}
	if got := len(stack.EchoCalls()) - e0; got != 1 {
		t.Errorf("echo calls = %d, want 1 (only the later request)", got)
	}
	log := stack.ErrorLog()
	for _, bad := range []string{"[alert]", "[crit]", "[emerg]", "exited on signal"} {
		if strings.Contains(log, bad) {
			t.Errorf("error log contains %s:\n%s", bad, log)
		}
	}
	calls := stack.AilurosCalls()[a0:]
	if len(calls) != 2 {
		t.Errorf("ailuros calls = %d, want 2 (the abandoned one and the later one)", len(calls))
	}
}

const reloadConfTemplate = `
server {
    listen 8080;
    ailuros_request /_ailuros;
    ailuros_request_unavailable %s;

    location = /_ailuros {
        internal;
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_connect_timeout 1s;
        proxy_read_timeout 5s;
    }
    location /c/ { proxy_pass http://echo:8080; }
}
`

func TestReloadMidCheck(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: fmt.Sprintf(reloadConfTemplate, "closed")})
	stack.SetAilurosAnswer(403, 2*time.Second, nil)

	const n = 10
	a0, e0 := len(stack.AilurosCalls()), len(stack.EchoCalls())
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := stack.Client().Get(fmt.Sprintf("%s/c/inflight/%d", stack.Base(), i))
			if err != nil {
				return
			}
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(stack.AilurosCalls())-a0 < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests reached ailuros", len(stack.AilurosCalls())-a0, n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// All ten are mid-check. Reload with a config that fails open.
	stack.Reload(t, fmt.Sprintf(reloadConfTemplate, "open"))
	wg.Wait()
	for i, s := range statuses {
		if s != 403 {
			t.Errorf("in-flight request %d: status = %d, want its decision (403)", i, s)
		}
	}
	if got := len(stack.EchoCalls()) - e0; got != 0 {
		t.Errorf("echo calls = %d, want 0 (all ten were blocked)", got)
	}

	// The new configuration: Ailuros unreachable now means continue.
	stack.StopAiluros()
	deadline = time.Now().Add(10 * time.Second)
	var last int
	for {
		last = stack.Get(t, "/c/after-reload").StatusCode
		if last == 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if last != 200 {
		t.Errorf("request after reload with ailuros down: status = %d, want 200 (open)", last)
	}
	log := stack.ErrorLog()
	for _, bad := range []string{"[alert]", "[crit]", "[emerg]", "exited on signal"} {
		if strings.Contains(log, bad) {
			t.Errorf("error log contains %s:\n%s", bad, log)
		}
	}
}
