package behaviour

import (
	"strconv"
	"testing"

	"github.com/0x4272616E646F6E/ailuros-nginx-module/test/harness"
)

// internalConf points ailuros_request at three kinds of location:
// /_nowhere, answered by the catch-all location / (return 200);
// /_exposed, an exact location proxying to Ailuros without "internal";
// /_ailuros, the normal internal location.
const internalConf = `
server {
    listen 8080;

    location / {
        return 200;
    }

    location = /_exposed {
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
    }

    location = /_ailuros {
        internal;
        proxy_pass http://ailuros:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
    }

    location /catchall-closed/ { ailuros_request /_nowhere; proxy_pass http://echo:8080; }
    location /catchall-open/   { ailuros_request /_nowhere; ailuros_request_unavailable open; proxy_pass http://echo:8080; }
    location /exposed-closed/  { ailuros_request /_exposed; proxy_pass http://echo:8080; }
    location /exposed-open/    { ailuros_request /_exposed; ailuros_request_unavailable open; proxy_pass http://echo:8080; }
    location /internal-closed/ { ailuros_request /_ailuros; proxy_pass http://echo:8080; }
}
`

func TestSubrequestLocationMustBeInternal(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: internalConf})
	stack.SetAilurosAnswer(200, 0, nil)

	cases := []struct {
		prefix, uri  string
		wantClient   int
		wantUpstream bool
		wantWarn     bool
		wantInternal bool // the "must be internal" error
		wantAiluros  bool
	}{
		{"/catchall-closed/", "/_nowhere", 503, false, false, true, false},
		{"/catchall-open/", "/_nowhere", 200, true, true, true, false},
		{"/exposed-closed/", "/_exposed", 503, false, false, true, true},
		{"/exposed-open/", "/_exposed", 200, true, true, true, true},
		{"/internal-closed/", "/_ailuros", 200, true, false, false, true},
	}
	for i, tc := range cases {
		t.Run(tc.prefix, func(t *testing.T) {
			echoBefore := len(stack.EchoCalls())
			ailurosBefore := len(stack.AilurosCalls())
			logBefore := stack.ErrorLog()

			resp := stack.Get(t, tc.prefix+"x?case="+strconv.Itoa(i))

			if resp.StatusCode != tc.wantClient {
				t.Errorf("client status = %d, want %d", resp.StatusCode, tc.wantClient)
			}
			if got := len(stack.EchoCalls()) > echoBefore; got != tc.wantUpstream {
				t.Errorf("upstream called = %v, want %v", got, tc.wantUpstream)
			}
			if got := len(stack.AilurosCalls()) > ailurosBefore; got != tc.wantAiluros {
				t.Errorf("ailuros called = %v, want %v", got, tc.wantAiluros)
			}
			newLog := waitLog(stack, logBefore, tc.wantWarn || tc.wantInternal)
			if got := hasLine(newLog, "[error]", "ailuros_request", tc.uri, "internal"); got != tc.wantInternal {
				t.Errorf("must-be-internal error logged = %v, want %v; new log:\n%s", got, tc.wantInternal, newLog)
			}
			if got := hasLine(newLog, "[warn]", "ailuros_request"); got != tc.wantWarn {
				t.Errorf("fail-open warning logged = %v, want %v; new log:\n%s", got, tc.wantWarn, newLog)
			}
		})
	}
}
