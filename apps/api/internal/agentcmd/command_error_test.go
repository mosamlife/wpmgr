package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPostRaw_AgentCommandFailedIsTyped proves the end-to-end path: a real
// agent 0.61.150+ command-failure body, served over httptest, comes back from
// postRaw as a *CommandError with AgentFailed()==true and the exception and
// location parsed — and Error() is still exactly the pre-existing string
// format every regex/substring classifier depends on.
func TestPostRaw_AgentCommandFailedIsTyped(t *testing.T) {
	siteID := uuid.New()
	const body = `{"code":"wpmgr_command_failed","message":"Command execution failed: RuntimeException: disk full writing chunk 4","data":{"status":500,"command":"backup","exception":"RuntimeException","at":"includes/commands/class-backup-command.php:239"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	_, err := client.postRaw(context.Background(), siteID, srv.URL, "backup", struct{}{})
	if err == nil {
		t.Fatal("postRaw: expected an error for a 500 response, got nil")
	}

	ce, ok := AsCommandError(err)
	if !ok {
		t.Fatalf("AsCommandError(err) = (_, false), want true; err = %v (%T)", err, err)
	}
	if !ce.AgentFailed() {
		t.Fatalf("AgentFailed() = false, want true; ce = %+v", ce)
	}
	if ce.Exception != "RuntimeException" {
		t.Errorf("Exception = %q, want %q", ce.Exception, "RuntimeException")
	}
	const wantAt = "includes/commands/class-backup-command.php:239"
	if ce.At != wantAt {
		t.Errorf("At = %q, want %q", ce.At, wantAt)
	}

	wantErr := fmt.Sprintf("backup command rejected by agent: status 500 body=%s", body)
	if got := err.Error(); got != wantErr {
		t.Errorf("Error() = %q, want %q (byte-identical to the legacy format)", got, wantErr)
	}

	status, ok := extractHTTPStatus(err)
	if !ok || status != 500 {
		t.Errorf("extractHTTPStatus(err) = (%d, %v), want (500, true)", status, ok)
	}
}

// TestCommandError_NotAgentFailed enumerates the shapes that must NOT be
// treated as a final, agent-produced failure: anything ambiguous stays
// retryable (see the #791 design doc's classification table).
func TestCommandError_NotAgentFailed(t *testing.T) {
	tests := []struct {
		name    string
		command string
		status  int
		body    string
	}{
		{
			name:    "502 HTML from a proxy",
			command: "backup",
			status:  502,
			body:    "<html><body><h1>502 Bad Gateway</h1></body></html>",
		},
		{
			name:    "500 internal_server_error (WordPress fatal)",
			command: "backup",
			status:  500,
			body:    `{"code":"wpmgr_internal_server_error","message":"Allowed memory size exhausted","data":{"status":500}}`,
		},
		{
			name:    "500 with data.command mismatch",
			command: "backup",
			status:  500,
			body:    `{"code":"wpmgr_command_failed","message":"Command execution failed: RuntimeException: x","data":{"status":500,"command":"restore","exception":"RuntimeException","at":"includes/commands/class-restore-command.php:10"}}`,
		},
		{
			name:    "503 with the agent code but a non-500 status",
			command: "backup",
			status:  503,
			body:    `{"code":"wpmgr_command_failed","message":"Command execution failed.","data":{"status":500}}`,
		},
		{
			name:    "PHP-notice-prefixed body",
			command: "backup",
			status:  500,
			body:    "Notice: Undefined index: foo in includes/commands/class-backup-command.php on line 12\n" + `{"code":"wpmgr_command_failed","message":"Command execution failed.","data":{"status":500}}`,
		},
		{
			name:    "body over 8 KiB",
			command: "backup",
			status:  500,
			body:    `{"code":"wpmgr_command_failed","message":"` + strings.Repeat("a", 9000) + `","data":{"status":500}}`,
		},
		{
			name:    "403 wpmgr_token_expired",
			command: "backup",
			status:  403,
			body:    `{"code":"wpmgr_token_expired","message":"token expired","data":{"status":403}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := newCommandError(tt.command, tt.status, tt.body, []byte(tt.body))
			if ce.AgentFailed() {
				t.Errorf("AgentFailed() = true, want false; ce = %+v", ce)
			}
		})
	}
}

// TestOperatorMessage_Sanitises proves the sanitiser: an unsafe field is
// dropped or replaced, never shown verbatim, and the composed message never
// exceeds the 512-byte cap.
func TestOperatorMessage_Sanitises(t *testing.T) {
	t.Run("absolute path in at is dropped", func(t *testing.T) {
		ce := &CommandError{
			Command:   "backup",
			Status:    500,
			Code:      commandFailedCode,
			Message:   "Command execution failed: RuntimeException: disk full",
			Exception: "RuntimeException",
			At:        "/var/www/html/wp-content/plugins/wpmgr/includes/class-backup-command.php:239",
		}
		got := ce.OperatorMessage("Backup")
		if strings.Contains(got, "/var/www") {
			t.Errorf("OperatorMessage() leaked the absolute path: %q", got)
		}
		if !strings.Contains(got, "RuntimeException") {
			t.Errorf("OperatorMessage() dropped the exception name too: %q", got)
		}
	})

	t.Run("url, bidi, control characters and encoded runs are replaced in the reason", func(t *testing.T) {
		longRun := strings.Repeat("Qb64XyZ9", 5) // 40 chars, well past the 32-char encoded-run threshold
		// The URL and the encoded run are kept whitespace-separated so each is
		// exercised independently: an encoded run glued directly onto a URL (as
		// its query string, say) is legitimately swallowed whole by the URL
		// match, a stricter redaction rather than a weaker one, so that overlap
		// is not what this case is proving.
		message := "Command execution failed: RuntimeException: upload to " +
			"https://evil.example.com/callback failed, token " + longRun +
			" was‮hidden‬\x01\x02 rejected"
		ce := &CommandError{
			Command:   "backup",
			Status:    500,
			Code:      commandFailedCode,
			Message:   message,
			Exception: "RuntimeException",
			At:        "includes/commands/class-backup-command.php:239",
		}
		got := ce.OperatorMessage("Backup")

		if strings.Contains(got, "evil.example.com") {
			t.Errorf("OperatorMessage() leaked the URL: %q", got)
		}
		if !strings.Contains(got, "[link]") {
			t.Errorf("OperatorMessage() did not replace the URL with [link]: %q", got)
		}
		if strings.Contains(got, longRun) {
			t.Errorf("OperatorMessage() leaked the long encoded run: %q", got)
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("OperatorMessage() did not replace the encoded run with [redacted]: %q", got)
		}
		if strings.ContainsRune(got, '‮') || strings.ContainsRune(got, '‬') {
			t.Errorf("OperatorMessage() leaked a bidi override character: %q", got)
		}
		if strings.ContainsRune(got, '\x01') || strings.ContainsRune(got, '\x02') {
			t.Errorf("OperatorMessage() leaked a raw control character: %q", got)
		}
	})

	t.Run("output is capped at 512 bytes", func(t *testing.T) {
		ce := &CommandError{
			Command:   "backup",
			Status:    500,
			Code:      commandFailedCode,
			Message:   "Command execution failed: RuntimeException: " + strings.Repeat("very long failure detail ", 100),
			Exception: "RuntimeException",
			At:        "includes/commands/class-backup-command.php:239",
		}
		got := ce.OperatorMessage("Backup")
		if len(got) > 512 {
			t.Errorf("OperatorMessage() = %d bytes, want <= 512", len(got))
		}
	})

	t.Run("old-agent shape (no exception/at/command) gives the update-the-agent copy", func(t *testing.T) {
		ce := &CommandError{
			Command: "backup",
			Status:  500,
			Code:    commandFailedCode,
			Message: "Command execution failed.",
			// Exception, At, DataCommand all zero-value: the pre-#756 shape.
		}
		got := ce.OperatorMessage("Backup")
		if !strings.Contains(got, "update the WPMgr agent on this site to see it") {
			t.Errorf("OperatorMessage() = %q, want the old-agent fallback copy", got)
		}
		if strings.Contains(got, "Agent error:") {
			t.Errorf("OperatorMessage() = %q, should not claim an agent-error detail it doesn't have", got)
		}
	})
}

// TestCommandError_ErrorByteIdentical asserts Error() is byte-for-byte the
// same string the untyped fmt.Errorf built before CommandError existed, for
// several body shapes — including the >512-byte truncation case, which is
// where a refactor is most likely to introduce a subtle format drift.
func TestCommandError_ErrorByteIdentical(t *testing.T) {
	tests := []struct {
		name    string
		command string
		status  int
		body    string
	}{
		{"empty body", "ping", 500, ""},
		{"short json body", "backup", 403, `{"code":"wpmgr_token_expired"}`},
		{"exactly 512 bytes", "scan", 500, strings.Repeat("x", 512)},
		{"over 512 bytes (truncated)", "scan", 500, strings.Repeat("y", 600)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mirrors postRaw's own snippet computation exactly (client.go),
			// independent of newCommandError, so this test would catch a
			// format drift in either place.
			snippet := tt.body
			if len(snippet) > 512 {
				snippet = snippet[:512] + "…(truncated)"
			}
			want := fmt.Sprintf("%s command rejected by agent: status %d body=%s", tt.command, tt.status, snippet)

			ce := newCommandError(tt.command, tt.status, snippet, []byte(tt.body))
			if got := ce.Error(); got != want {
				t.Errorf("Error() = %q, want %q", got, want)
			}
		})
	}
}

// forbidden403 builds the CommandError postRaw builds for a 403 reply with
// body, through the same snippet clamp and constructor.
func forbidden403(body string) *CommandError {
	snippet := body
	if len(snippet) > 512 {
		snippet = snippet[:512] + "…(truncated)"
	}
	return newCommandError("update", http.StatusForbidden, snippet, []byte(body))
}

// noticePrefix is what a site with display_errors and html_errors on prints
// in front of a REST reply.
const noticePrefix = "<br />\n<b>Deprecated</b>:  Function get_page_by_title is deprecated in <b>/var/www/html/wp-includes/functions.php</b> on line <b>6031</b><br />\n"

// TestForbiddenMessage covers every branch of the GH #679 classifier: who
// refused (a layer in front of the agent, something else on the site, or the
// agent with one of its codes) and the fixed copy each gets.
func TestForbiddenMessage(t *testing.T) {
	const (
		firewall  = "A firewall or security rule blocked the request (HTTP 403) before it reached the WPMgr agent."
		other     = "Something on the site other than the WPMgr agent refused the request (HTTP 403)"
		clock     = "because the site's server clock differs from WPMgr's"
		reconnect = "Reconnect the site to WPMgr, then run the update again."
		header    = "without its Authorization header"
	)
	tests := []struct {
		name    string
		body    string
		wantIn  []string
		wantOut []string
	}{
		// --- not from the agent: no code at all ---
		{"html firewall page", "<html><body><h1>Forbidden</h1><p>Request forbidden by administrative rules.</p></body></html>",
			[]string{"Update not started. ", firewall, "/wp-json/wpmgr/v1/"}, []string{"administrative rules", "<"}},
		{"empty body", "", []string{firewall}, nil},
		{"plain text", "Forbidden", []string{firewall}, nil},
		{"json without a code", `{"message":"denied by edge rule 1234"}`, []string{firewall}, []string{"edge rule"}},
		{"json null", "null", []string{firewall}, nil},
		{"json with a numeric code", `{"code":403,"message":"Forbidden"}`, []string{firewall}, nil},
		{"html page over 8 KiB", "<html>" + strings.Repeat("edgepage ", 1200) + "</html>", []string{firewall}, []string{"edgepage"}},

		// --- a code, but not one of the agent's: generic, never echoed ---
		{"rest api restricted by another plugin", `{"code":"rest_forbidden","message":"Sorry, you are not allowed to do that.","data":{"status":403}}`,
			[]string{"Update not started. ", other}, []string{"rest_forbidden", "Sorry"}},
		{"markup in the code", `{"code":"wpmgr_<script>alert(1)</script>","data":{"status":403}}`, []string{other}, []string{"script", "alert"}},
		{"overlong agent-shaped code", `{"code":"wpmgr_` + strings.Repeat("a", 41) + `","data":{"status":403}}`, []string{other}, []string{strings.Repeat("a", 41)}},
		{"non-ascii code", `{"code":"wpmgr_tökén_skew","data":{"status":403}}`, []string{other}, []string{"tökén", clock}},
		{"uppercase code", `{"code":"WPMGR_TOKEN_SKEW","data":{"status":403}}`, []string{other}, []string{"WPMGR", clock}},
		{"digit in the category", `{"code":"wpmgr_token2","data":{"status":403}}`, []string{other}, []string{"wpmgr_token2"}},
		{"empty category", `{"code":"wpmgr_","data":{"status":403}}`, []string{other}, []string{"wpmgr_)"}},
		{"newline in the code", `{"code":"wpmgr_token_skew\nInjected","data":{"status":403}}`, []string{other}, []string{"Injected", clock}},

		// --- the agent's own codes ---
		{"token expired: clock ahead", `{"code":"wpmgr_token_expired","message":"Forbidden.","data":{"status":403}}`,
			[]string{"(HTTP 403, wpmgr_token_expired)", clock, "sync the server time (NTP), then run the update again."}, []string{"Forbidden."}},
		{"token skew: clock behind", `{"code":"wpmgr_token_skew","message":"Forbidden.","data":{"status":403}}`,
			[]string{"(HTTP 403, wpmgr_token_skew)", clock}, nil},
		{"aud mismatch", `{"code":"wpmgr_aud_mismatch","message":"Forbidden.","data":{"status":403}}`,
			[]string{"(HTTP 403, wpmgr_aud_mismatch)", "missing or no longer valid", reconnect}, []string{clock}},
		{"site not enrolled", `{"code":"wpmgr_site_not_enrolled","message":"Forbidden.","data":{"status":403}}`, []string{reconnect}, nil},
		{"signature failed", `{"code":"wpmgr_sig_failed","message":"Forbidden.","data":{"status":403}}`, []string{reconnect}, nil},
		{"key not provisioned", `{"code":"wpmgr_key_not_provisioned","message":"Forbidden.","data":{"status":403}}`, []string{reconnect}, nil},
		{"missing token: header stripped", `{"code":"wpmgr_missing_token","message":"Forbidden.","data":{"status":403}}`,
			[]string{"(HTTP 403, wpmgr_missing_token)", header, "forward the Authorization header to PHP"}, []string{"Reconnect", clock}},
		{"invalid token: clock first, then reconnect", `{"code":"wpmgr_invalid_token","message":"Forbidden.","data":{"status":403}}`,
			[]string{"(HTTP 403, wpmgr_invalid_token)", "server clock", "reconnect the site to WPMgr", "Then run the update again."}, nil},
		{"other agent code: named, no invented remedy", `{"code":"wpmgr_token_replay","message":"Forbidden.","data":{"status":403}}`,
			[]string{"Update not started. The WPMgr agent refused the request (HTTP 403, wpmgr_token_replay)."}, []string{"Reconnect", clock, header}},
		{"longest allowed agent code", `{"code":"wpmgr_` + strings.Repeat("a", 40) + `","data":{"status":403}}`,
			[]string{"(HTTP 403, wpmgr_" + strings.Repeat("a", 40) + ")"}, nil},

		// --- the agent's reply behind PHP notices ---
		{"agent code behind a notice", noticePrefix + `{"code":"wpmgr_token_skew","message":"Forbidden.","data":{"status":403}}`,
			[]string{"(HTTP 403, wpmgr_token_skew)", clock}, []string{"Deprecated", "functions.php"}},
		{"rest code behind a notice", noticePrefix + `{"code":"rest_forbidden","message":"Sorry.","data":{"status":403}}`,
			[]string{other}, []string{"rest_forbidden"}},
		{"envelope behind a notice with another status", noticePrefix + `{"code":"wpmgr_token_skew","data":{"status":500}}`,
			[]string{firewall}, []string{clock}},
		{"truncated envelope behind a notice", noticePrefix + `{"code":"wpmgr_token_skew","data":{"sta`,
			[]string{firewall}, []string{clock}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ForbiddenMessage(forbidden403(tt.body), "Update")
			if !ok {
				t.Fatalf("ForbiddenMessage() ok = false for a 403")
			}
			if !strings.HasPrefix(got, "Update not started. ") {
				t.Errorf("message %q does not lead with %q", got, "Update not started. ")
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("message %q\n  does not contain %q", got, want)
				}
			}
			for _, leak := range tt.wantOut {
				if strings.Contains(got, leak) {
					t.Errorf("message %q\n  contains %q", got, leak)
				}
			}
		})
	}
}

// TestForbiddenMessage_DryRunAction proves the action word reaches both the
// lead-in and the closing step.
func TestForbiddenMessage_DryRunAction(t *testing.T) {
	got, ok := ForbiddenMessage(forbidden403(`{"code":"wpmgr_token_skew","data":{"status":403}}`), "Dry run")
	if !ok {
		t.Fatal("ok = false for a 403")
	}
	if !strings.HasPrefix(got, "Dry run not started. ") || !strings.HasSuffix(got, "then run the dry run again.") {
		t.Errorf("message = %q, want the dry-run lead-in and closing step", got)
	}
}

// TestForbiddenMessage_OnlyA403 proves the classifier describes nothing but a
// 403 CommandError, wrapped or not.
func TestForbiddenMessage_OnlyA403(t *testing.T) {
	agentBody := `{"code":"wpmgr_token_skew","data":{"status":403}}`
	for _, status := range []int{401, 404, 409, 500, 503} {
		if got, ok := ForbiddenMessage(newCommandError("update", status, agentBody, []byte(agentBody)), "Update"); ok || got != "" {
			t.Errorf("status %d: ForbiddenMessage() = (%q, %v), want (\"\", false)", status, got, ok)
		}
	}
	notCommandErrors := []error{
		nil,
		errors.New("dial tcp: connection refused"),
		&RedirectError{Command: "update", Status: 301, From: "https://example.com/wp-json/wpmgr/v1/command/update"},
	}
	for _, err := range notCommandErrors {
		if got, ok := ForbiddenMessage(err, "Update"); ok || got != "" {
			t.Errorf("%v: ForbiddenMessage() = (%q, %v), want (\"\", false)", err, got, ok)
		}
	}
	wrapped := fmt.Errorf("update: %w", forbidden403(agentBody))
	if got, ok := ForbiddenMessage(wrapped, "Update"); !ok || !strings.Contains(got, "wpmgr_token_skew") {
		t.Errorf("wrapped 403: ForbiddenMessage() = (%q, %v), want the clock copy", got, ok)
	}
}

// TestForbiddenMessage_ThroughPostRaw is the production path: a real 403
// reply served over HTTP comes back from postRaw as an error the classifier
// reads, including a reply with PHP notices in front of the agent's JSON.
func TestForbiddenMessage_ThroughPostRaw(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"host firewall page", "<html><body><h1>Forbidden</h1><p>Request forbidden by administrative rules.</p></body></html>", "firewall or security rule"},
		{"agent code", `{"code":"wpmgr_aud_mismatch","message":"Forbidden.","data":{"status":403}}`, "Reconnect the site to WPMgr"},
		{"agent code behind a notice", noticePrefix + `{"code":"wpmgr_token_expired","message":"Forbidden.","data":{"status":403}}`, "server clock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			client := buildTestAgentClient(t, srv)
			_, err := client.postRaw(context.Background(), uuid.New(), srv.URL, "update", struct{}{})
			if err == nil {
				t.Fatal("postRaw: expected an error for a 403 response, got nil")
			}
			got, ok := ForbiddenMessage(err, "Update")
			if !ok || !strings.Contains(got, tt.want) {
				t.Errorf("ForbiddenMessage() = (%q, %v), want it to contain %q", got, ok, tt.want)
			}
		})
	}
}

// TestCommandError_EmbeddedCodeChangesNoOtherClassifier proves the
// notice-prefixed parse is read by ForbiddenMessage alone: Error(), Code,
// AgentFailed and DescribeAttemptError see the reply exactly as before.
func TestCommandError_EmbeddedCodeChangesNoOtherClassifier(t *testing.T) {
	body := noticePrefix + `{"code":"wpmgr_token_skew","message":"Forbidden.","data":{"status":403}}`
	ce := forbidden403(body)
	if ce.Code != "" || ce.Message != "" || ce.DataStatus != 0 {
		t.Errorf("parsed fields = (%q, %q, %d), want all empty for a reply that is not JSON as a whole", ce.Code, ce.Message, ce.DataStatus)
	}
	if want := "update command rejected by agent: status 403 body=" + body; ce.Error() != want {
		t.Errorf("Error() = %q, want %q", ce.Error(), want)
	}
	if ce.AgentFailed() {
		t.Error("AgentFailed() = true for a 403")
	}
	if got, want := DescribeAttemptError(ce), "The site refused the request (HTTP 403)."; got != want {
		t.Errorf("DescribeAttemptError() = %q, want %q (unchanged)", got, want)
	}
}
