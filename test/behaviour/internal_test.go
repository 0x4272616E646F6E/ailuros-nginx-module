package behaviour

import (
	"strconv"
	"testing"

	"github.com/0x4272616E646F6E/decision-waf-nginx-module/test/harness"
)

// internalConf points decision_waf_request at three kinds of location:
// /_nowhere, answered by the catch-all location / (return 200);
// /_exposed, an exact location proxying to decision-waf without "internal";
// /_decision_waf, the normal internal location.
const internalConf = `
server {
    listen 8080;

    location / {
        return 200;
    }

    location = /_exposed {
        proxy_pass http://decision-waf:9002;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
    }

    location = /_decision_waf {
        internal;
        proxy_pass http://decision-waf:9002;
        proxy_pass_request_body off;
        proxy_cache off;
        proxy_set_header Content-Length "";
    }

    location /catchall-closed/ { decision_waf_request /_nowhere; proxy_pass http://echo:8080; }
    location /catchall-open/   { decision_waf_request /_nowhere; decision_waf_request_unavailable open; proxy_pass http://echo:8080; }
    location /exposed-closed/  { decision_waf_request /_exposed; proxy_pass http://echo:8080; }
    location /exposed-open/    { decision_waf_request /_exposed; decision_waf_request_unavailable open; proxy_pass http://echo:8080; }
    location /internal-closed/ { decision_waf_request /_decision_waf; proxy_pass http://echo:8080; }
}
`

func TestSubrequestLocationMustBeInternal(t *testing.T) {
	stack := harness.Up(t, harness.Config{NGINXConf: internalConf})
	stack.SetDecisionWafAnswer(200, 0, nil)

	cases := []struct {
		prefix, uri     string
		wantClient      int
		wantUpstream    bool
		wantWarn        bool
		wantInternal    bool // the "must be internal" error
		wantDecisionWaf bool
	}{
		{"/catchall-closed/", "/_nowhere", 503, false, false, true, false},
		{"/catchall-open/", "/_nowhere", 200, true, true, true, false},
		{"/exposed-closed/", "/_exposed", 503, false, false, true, true},
		{"/exposed-open/", "/_exposed", 200, true, true, true, true},
		{"/internal-closed/", "/_decision_waf", 200, true, false, false, true},
	}
	for i, tc := range cases {
		t.Run(tc.prefix, func(t *testing.T) {
			echoBefore := len(stack.EchoCalls())
			decisionWafBefore := len(stack.DecisionWafCalls())
			logBefore := stack.ErrorLog()

			resp := stack.Get(t, tc.prefix+"x?case="+strconv.Itoa(i))

			if resp.StatusCode != tc.wantClient {
				t.Errorf("client status = %d, want %d", resp.StatusCode, tc.wantClient)
			}
			if got := len(stack.EchoCalls()) > echoBefore; got != tc.wantUpstream {
				t.Errorf("upstream called = %v, want %v", got, tc.wantUpstream)
			}
			if got := len(stack.DecisionWafCalls()) > decisionWafBefore; got != tc.wantDecisionWaf {
				t.Errorf("decision-waf called = %v, want %v", got, tc.wantDecisionWaf)
			}
			newLog := waitLog(stack, logBefore, tc.wantWarn || tc.wantInternal)
			if got := hasLine(newLog, "[error]", "decision_waf_request", tc.uri, "internal"); got != tc.wantInternal {
				t.Errorf("must-be-internal error logged = %v, want %v; new log:\n%s", got, tc.wantInternal, newLog)
			}
			if got := hasLine(newLog, "[warn]", "decision_waf_request"); got != tc.wantWarn {
				t.Errorf("fail-open warning logged = %v, want %v; new log:\n%s", got, tc.wantWarn, newLog)
			}
		})
	}
}
