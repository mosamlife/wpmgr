package agentcmd

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

// siteSuppliedText stands for text a site sent. Every failure below carries it
// in the reply, so a message that shows it has shown the site's text.
const siteSuppliedText = "site-supplied-QZXJ"

const failureAction = "Diagnostics check"

// rejected builds the error postRaw returns for a non-2xx reply: the typed
// CommandError, whose Error() carries a snippet of the reply.
func rejected(status int, body string) error {
	snippet := body
	if len(snippet) > 512 {
		snippet = snippet[:512] + "…(truncated)"
	}
	return newCommandError("diagnostics", status, snippet, []byte(body))
}

// transportFailure is a failure of the exchange the way sendSigned reports it.
func transportFailure(inner error) error {
	return fmt.Errorf("diagnostics command transport: %w", &url.Error{Op: "Post", URL: "https://site.test/wp-json/wpmgr/v1/command/diagnostics", Err: inner})
}

// TestDescribeSiteFailure: each way a signed command can fail maps to one code
// and fixed copy, wrapped or not, and none of the copy shows the site's text.
func TestDescribeSiteFailure(t *testing.T) {
	var into map[string]json.RawMessage
	decodeErr := json.Unmarshal([]byte("Warning: "+siteSuppliedText), &into)
	if !isDecodeErr(decodeErr) {
		t.Fatalf("fixture is not a decode error: %v", decodeErr)
	}
	typeErr := json.Unmarshal([]byte("[1,2,3]"), &into)
	if !isDecodeErr(typeErr) {
		t.Fatalf("fixture is not a decode error: %v", typeErr)
	}

	cases := []struct {
		name       string
		err        error
		wantOK     bool
		wantCode   string
		wantStatus int
		// wantHas are fragments the copy must contain; the whole message
		// must also start with wantLead.
		wantLead string
		wantHas  []string
	}{
		{
			name:       "redirect to another address",
			err:        &RedirectError{Command: "diagnostics", Status: 302, From: "https://site.test/wp-json/wpmgr/v1/command/diagnostics", To: "https://other.test/wp-json/wpmgr/v1/command/diagnostics"},
			wantOK:     true,
			wantCode:   "site_url_redirects",
			wantStatus: 302,
			wantLead:   failureAction + " not started. ",
			wantHas:    []string{"https://site.test redirects to https://other.test", "Exempt /wp-json/wpmgr/ from the redirect"},
		},
		{
			name:       "403 page from the web server",
			err:        rejected(403, "<html><body>"+siteSuppliedText+"</body></html>"),
			wantOK:     true,
			wantCode:   "site_refused",
			wantStatus: 403,
			wantLead:   failureAction + " not started. ",
			wantHas: []string{
				"A firewall or security rule blocked the request (HTTP 403) before it reached the WPMgr agent.",
				"Ask the site's host to allow requests to /wp-json/wpmgr/v1/ from WPMgr",
			},
		},
		{
			name:       "403 from the agent, token expired",
			err:        rejected(403, `{"code":"wpmgr_token_expired","message":"Forbidden. `+siteSuppliedText+`","data":{"status":403}}`),
			wantOK:     true,
			wantCode:   "site_refused",
			wantStatus: 403,
			wantLead:   failureAction + " not started. ",
			wantHas:    []string{"(HTTP 403, wpmgr_token_expired)", "server clock differs", "then run the diagnostics check again."},
		},
		{
			name:       "403 from something other than the agent",
			err:        rejected(403, `{"code":"rest_forbidden","message":"`+siteSuppliedText+`","data":{"status":403}}`),
			wantOK:     true,
			wantCode:   "site_refused",
			wantStatus: 403,
			wantLead:   failureAction + " not started. ",
			wantHas:    []string{"Something on the site other than the WPMgr agent refused the request (HTTP 403)"},
		},
		{
			name:       "403 from the agent behind a PHP notice",
			err:        rejected(403, "\nNotice: "+siteSuppliedText+"\n"+`{"code":"wpmgr_invalid_token","message":"x","data":{"status":403}}`),
			wantOK:     true,
			wantCode:   "site_refused",
			wantStatus: 403,
			wantLead:   failureAction + " not started. ",
			wantHas:    []string{"The WPMgr agent could not verify the request (HTTP 403, wpmgr_invalid_token)"},
		},
		{
			name: "the agent reports its own failure",
			err: rejected(500, `{"code":"wpmgr_command_failed","message":"Command execution failed: RuntimeException: `+siteSuppliedText+`",`+
				`"data":{"status":500,"command":"diagnostics","exception":"RuntimeException","at":"includes/commands/class-diagnostics-command.php:77"}}`),
			wantOK:     true,
			wantCode:   "agent_command_failed",
			wantStatus: 500,
			wantLead:   failureAction + " failed: ",
			wantHas: []string{
				"the WPMgr agent on this site stopped with an error while running it.",
				"Check the site's PHP error log for the cause, fix it, then run the diagnostics check again.",
			},
		},
		{
			name: "a 500 that names another command is not the agent's failure",
			err: rejected(500, `{"code":"wpmgr_command_failed","message":"`+siteSuppliedText+`",`+
				`"data":{"status":500,"command":"backup","exception":"RuntimeException","at":"includes/commands/class-backup-command.php:9"}}`),
			wantOK:     true,
			wantCode:   "site_error",
			wantStatus: 500,
			wantLead:   failureAction + " failed. ",
			wantHas:    []string{"The site returned a server error (HTTP 500)."},
		},
		{
			name:       "WordPress critical error",
			err:        rejected(500, `{"code":"internal_server_error","message":"`+siteSuppliedText+`","data":{"status":500}}`),
			wantOK:     true,
			wantCode:   "site_error",
			wantStatus: 500,
			wantLead:   failureAction + " failed. ",
			wantHas:    []string{"WordPress on the site hit a critical error (HTTP 500)."},
		},
		{
			name:       "error page from a proxy",
			err:        rejected(502, "<html>Bad Gateway "+siteSuppliedText+"</html>"),
			wantOK:     true,
			wantCode:   "site_error",
			wantStatus: 502,
			wantLead:   failureAction + " failed. ",
			wantHas:    []string{"The site returned a server error (HTTP 502) that did not come from the WPMgr agent."},
		},
		{
			name:       "agent route not found",
			err:        rejected(404, `{"code":"rest_no_route","message":"`+siteSuppliedText+`","data":{"status":404}}`),
			wantOK:     true,
			wantCode:   "site_error",
			wantStatus: 404,
			wantLead:   failureAction + " failed. ",
			wantHas:    []string{"The site did not find the WPMgr agent (HTTP 404). Check that the plugin is active."},
		},
		{
			name:       "any other status",
			err:        rejected(401, "<html>"+siteSuppliedText+"</html>"),
			wantOK:     true,
			wantCode:   "site_error",
			wantStatus: 401,
			wantLead:   failureAction + " failed. ",
			wantHas:    []string{"The site answered HTTP 401."},
		},
		{
			name:     "timeout",
			err:      transportFailure(context.DeadlineExceeded),
			wantOK:   true,
			wantCode: "site_timeout",
			wantLead: failureAction + " failed. ",
			wantHas:  []string{"The site did not answer in time."},
		},
		{
			name:     "timeout reading the reply",
			err:      fmt.Errorf("read diagnostics response: %w", context.DeadlineExceeded),
			wantOK:   true,
			wantCode: "site_timeout",
			wantLead: failureAction + " failed. ",
			wantHas:  []string{"The site did not answer in time."},
		},
		{
			name:     "reply that is not JSON",
			err:      fmt.Errorf("ingest agent response: %w", decodeErr),
			wantOK:   true,
			wantCode: "site_bad_reply",
			wantLead: failureAction + " failed. ",
			wantHas:  []string{"The site answered, but the reply was not the WPMgr agent's response. A PHP notice or another plugin's output may be in the way."},
		},
		{
			name:     "reply that is JSON but not an object",
			err:      typeErr,
			wantOK:   true,
			wantCode: "site_bad_reply",
			wantLead: failureAction + " failed. ",
			wantHas:  []string{"The site answered, but the reply was not the WPMgr agent's response."},
		},
		{
			name:     "connection refused",
			err:      markNotSent(transportFailure(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused " + siteSuppliedText)})),
			wantOK:   true,
			wantCode: "site_unreachable",
			wantLead: failureAction + " failed. ",
			wantHas:  []string{"Could not connect to the site."},
		},
		{
			name:     "certificate not accepted",
			err:      markNotSent(transportFailure(x509.UnknownAuthorityError{})),
			wantOK:   true,
			wantCode: "site_unreachable",
			wantLead: failureAction + " failed. ",
			wantHas:  []string{"The site's HTTPS certificate was not accepted."},
		},
		{
			name:     "reply cut off while it was read",
			err:      fmt.Errorf("read diagnostics response: %w", errors.New("stream error: stream ID 3; INTERNAL_ERROR; received from peer")),
			wantOK:   true,
			wantCode: "site_unreachable",
			wantLead: failureAction + " failed. ",
			wantHas:  []string{"Could not connect to the site."},
		},
		{name: "no error", err: nil},
		{name: "cancelled by the caller", err: transportFailure(context.Canceled)},
		{name: "cancelled before the request was written", err: markNotSent(transportFailure(context.Canceled))},
		{name: "token could not be minted", err: markNotSent(fmt.Errorf("mint command jwt: %w", errors.New("signing key is unusable")))},
		{name: "saved address unusable", err: markNotSent(errors.New(`invalid site url scheme "ftp"`))},
		{name: "agent reported ok false", err: &agentReportedError{msg: "diagnostics rejected by agent: " + siteSuppliedText}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{tc.err, wrapIfSet(tc.err)} {
				got, ok := DescribeSiteFailure(err, failureAction)
				if ok != tc.wantOK {
					t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
				}
				if !tc.wantOK {
					if got != (SiteFailure{}) {
						t.Errorf("not mapped, but got %+v", got)
					}
					continue
				}
				if got.Code != tc.wantCode || got.Status != tc.wantStatus {
					t.Errorf("code %q status %d, want %q %d", got.Code, got.Status, tc.wantCode, tc.wantStatus)
				}
				if !strings.HasPrefix(got.Message, tc.wantLead) {
					t.Errorf("message %q does not start with %q", got.Message, tc.wantLead)
				}
				for _, has := range tc.wantHas {
					if !strings.Contains(got.Message, has) {
						t.Errorf("message %q does not contain %q", got.Message, has)
					}
				}
				for _, banned := range []string{siteSuppliedText, "<html", "Forbidden.", "RuntimeException", "class-diagnostics-command", "body="} {
					if strings.Contains(got.Message, banned) {
						t.Errorf("message shows site text %q: %q", banned, got.Message)
					}
				}
			}
		})
	}
}

// wrapIfSet wraps err the way a caller's own context does; nil stays nil.
func wrapIfSet(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("while refreshing: %w", err)
}

// TestDescribeSiteFailureAgentFailureCopyIsFixed: the agent's failure is told
// in one fixed sentence whatever the agent said, so two different failures
// read the same.
func TestDescribeSiteFailureAgentFailureCopyIsFixed(t *testing.T) {
	a := rejected(500, `{"code":"wpmgr_command_failed","message":"Command execution failed: A: one","data":{"status":500,"command":"diagnostics","exception":"A","at":"a/b.php:1"}}`)
	b := rejected(500, `{"code":"wpmgr_command_failed","message":"Command execution failed: B: two","data":{"status":500,"command":"diagnostics","exception":"B","at":"c/d.php:2"}}`)
	fa, okA := DescribeSiteFailure(a, failureAction)
	fb, okB := DescribeSiteFailure(b, failureAction)
	if !okA || !okB || fa != fb {
		t.Fatalf("want identical fixed copy, got %+v and %+v", fa, fb)
	}
	if fa.Code != "agent_command_failed" {
		t.Fatalf("code = %q, want agent_command_failed", fa.Code)
	}
}
