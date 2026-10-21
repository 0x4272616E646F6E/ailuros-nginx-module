package behaviour

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/harness"
)

// answersConf protects /closed/ and /open/ (both modes) with the Ailuros
// stub, and /missing-closed/ and /missing-open/ with a location that does not
// exist. There is no catch-all location, so /_missing is a 404.
const answersConf = `
server {
    listen 8080;

    location = /_ailuros {
        internal;
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_cache off;
        proxy_set_header Content-Length "";
        proxy_set_header X-Original-URI $request_uri;
        proxy_set_header X-Original-Method $request_method;
        proxy_connect_timeout 1s;
        proxy_read_timeout 1s;
    }

    location /closed/ {
        ailuros_request /_ailuros;
        ailuros_request_set $ailuros_request_id $upstream_http_x_ailuros_request_id;
        add_header X-Status $ailuros_request_status always;
        add_header X-Ailuros-Request-Id $ailuros_request_id always;
        proxy_pass http://echo:8080;
    }

    location /open/ {
        ailuros_request /_ailuros;
        ailuros_request_set $ailuros_request_id $upstream_http_x_ailuros_request_id;
        ailuros_request_unavailable open;
        add_header X-Status $ailuros_request_status always;
        add_header X-Ailuros-Request-Id $ailuros_request_id always;
        proxy_pass http://echo:8080;
    }

    location /missing-closed/ {
        ailuros_request /_missing;
        add_header X-Status $ailuros_request_status always;
        proxy_pass http://echo:8080;
    }

    location /missing-open/ {
        ailuros_request /_missing;
        ailuros_request_unavailable open;
        add_header X-Status $ailuros_request_status always;
        proxy_pass http://echo:8080;
    }
}
`

type condition int

const (
	answer condition = iota
	delayed
	missingLocation
	stopped // irreversible: these rows run last
)

type answerCase struct {
	name          string
	cond          condition
	ailurosStatus int // for answer
	open          bool
	wantClient    int
	wantUpstream  bool
	wantWarn      bool
	wantSubStatus string // $ailuros_request_status
	wantLogged    string // a substring the error log must gain, if any
}

func answerCases() []answerCase {
	var cases []answerCase
	for _, open := range []bool{false, true} {
		mode := "closed"
		if open {
			mode = "open"
		}
		row := func(name string, cond condition, status, client int, upstream, warn bool, sub string) {
			cases = append(cases, answerCase{
				name: mode + "/" + name, cond: cond, ailurosStatus: status, open: open,
				wantClient: client, wantUpstream: upstream, wantWarn: warn, wantSubStatus: sub,
			})
		}
		for _, s := range []int{200, 204} {
			row(strconv.Itoa(s), answer, s, 200, true, false, strconv.Itoa(s))
		}
		for _, s := range []int{403, 400, 413, 503} {
			row(strconv.Itoa(s), answer, s, s, false, false, strconv.Itoa(s))
		}
		noDecision := func(name string, cond condition, status int, sub string) {
			if open {
				row(name, cond, status, 200, true, true, sub)
			} else {
				row(name, cond, status, 503, false, false, sub)
			}
		}
		for _, s := range []int{500, 502, 404, 302} {
			noDecision(strconv.Itoa(s), answer, s, strconv.Itoa(s))
		}
		noDecision("delayed beyond proxy_read_timeout", delayed, 200, "504")
		noDecision("missing location", missingLocation, 0, "404")
		if !open {
			cases[len(cases)-1].wantLogged = "/_missing"
		}
	}
	for _, open := range []bool{false, true} {
		c := answerCase{name: "closed/ailuros stopped", cond: stopped, open: open,
			wantClient: 503, wantSubStatus: "502"}
		if open {
			c = answerCase{name: "open/ailuros stopped", cond: stopped, open: open,
				wantClient: 200, wantUpstream: true, wantWarn: true, wantSubStatus: "502"}
		}
		cases = append(cases, c)
	}
	return cases
}

func TestAnswerMapping(t *testing.T) {
	t.Run("config errors", testConfigErrors)

	stack := harness.Up(t, harness.Config{NGINXConf: answersConf})

	for i, tc := range answerCases() {
		t.Run(tc.name, func(t *testing.T) {
			requestID := fmt.Sprintf("req-%d", i)
			prefix := "/closed/"
			if tc.open {
				prefix = "/open/"
			}
			reachable := true
			switch tc.cond {
			case answer:
				stack.SetAilurosAnswer(tc.ailurosStatus, 0,
					map[string]string{"X-Ailuros-Request-Id": requestID})
			case delayed:
				stack.SetAilurosAnswer(200, 3*time.Second,
					map[string]string{"X-Ailuros-Request-Id": requestID})
				reachable = false
			case missingLocation:
				prefix = strings.Replace(prefix, "/", "/missing-", 1)
				reachable = false
			case stopped:
				stack.StopAiluros()
				reachable = false
			}

			echoBefore := len(stack.EchoCalls())
			logBefore := stack.ErrorLog()

			path := prefix + "item?case=" + strconv.Itoa(i)
			resp := stack.Get(t, path)

			if resp.StatusCode != tc.wantClient {
				t.Errorf("client status = %d, want %d", resp.StatusCode, tc.wantClient)
			}
			if got := resp.Header.Get("X-Status"); got != tc.wantSubStatus {
				t.Errorf("$ailuros_request_status = %q, want %q", got, tc.wantSubStatus)
			}
			if tc.cond == answer {
				if got := resp.Header.Get("X-Ailuros-Request-Id"); got != requestID {
					t.Errorf("X-Ailuros-Request-Id = %q, want %q", got, requestID)
				}
			} else if !reachable && tc.cond != delayed {
				if got := resp.Header.Get("X-Ailuros-Request-Id"); got != "" {
					t.Errorf("X-Ailuros-Request-Id = %q, want none", got)
				}
			}

			echoCalls := stack.EchoCalls()[echoBefore:]
			if got := len(echoCalls) > 0; got != tc.wantUpstream {
				t.Errorf("upstream called = %v, want %v (%d calls)", got, tc.wantUpstream, len(echoCalls))
			}
			for _, c := range echoCalls {
				if c.URI != path {
					t.Errorf("upstream got %q, want %q", c.URI, path)
				}
			}

			// closed and no decision: an error naming the directive
			wantErr := !tc.open && tc.wantClient == 503 && tc.wantSubStatus != "503"
			newLog := waitLog(stack, logBefore, tc.wantWarn || wantErr)
			warned := hasLine(newLog, "[warn]", "ailuros_request")
			if warned != tc.wantWarn {
				t.Errorf("fail-open warning logged = %v, want %v; new log:\n%s", warned, tc.wantWarn, newLog)
			}
			if wantErr && !hasLine(newLog, "[error]", "ailuros_request") {
				t.Errorf("no-decision error not logged; new log:\n%s", newLog)
			}
			if tc.wantLogged != "" && !hasLine(newLog, "[error]", tc.wantLogged) {
				t.Errorf("error log lacks a line with %q; new log:\n%s", tc.wantLogged, newLog)
			}
		})
	}
}

// waitLog returns what the error log gained since before; when want is set
// it waits briefly for something to arrive (docker logs can lag).
func waitLog(stack *harness.Stack, before string, want bool) string {
	deadline := time.Now().Add(3 * time.Second)
	for {
		now := stack.ErrorLog()
		gained := strings.TrimPrefix(now, before)
		if !want || hasLine(gained, "ailuros_request") || time.Now().After(deadline) {
			if !want {
				// give a stray line the same chance to show up
				time.Sleep(300 * time.Millisecond)
				gained = strings.TrimPrefix(stack.ErrorLog(), before)
			}
			return gained
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func hasLine(log string, subs ...string) bool {
	for _, line := range strings.Split(log, "\n") {
		all := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func testConfigErrors(t *testing.T) {
	const valid = `
ailuros_request off;
ailuros_request_unavailable closed;
server {
    listen 8080;
    ailuros_request /_ailuros;
    ailuros_request_unavailable open;
    location = /_ailuros { internal; return 200; }
    location / {
        ailuros_request_set $ailuros_request_id $ailuros_request_status;
        return 200;
    }
    location /off/ { ailuros_request off; return 200; }
}
`
	if ok, out := harness.CheckConfig(t, valid); !ok {
		t.Fatalf("valid config rejected:\n%s", out)
	}

	cases := []struct {
		name, directive, conf string
	}{
		{"uri without slash", "ailuros_request",
			`server { listen 8080; location / { ailuros_request _ailuros; return 200; } }`},
		{"unknown unavailable mode", "ailuros_request_unavailable",
			`server { listen 8080; location / { ailuros_request_unavailable maybe; return 200; } }`},
		{"set without variable", "ailuros_request_set",
			`server { listen 8080; location / { ailuros_request_set ailuros_request_id $upstream_http_x_ailuros_request_id; return 200; } }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, out := harness.CheckConfig(t, tc.conf)
			if ok {
				t.Fatalf("nginx -t accepted it:\n%s", out)
			}
			if !strings.Contains(out, `"`+tc.directive+`"`) {
				t.Errorf("nginx -t output does not name %q:\n%s", tc.directive, out)
			}
		})
	}
}
