package agentcmd

import (
	"context"
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
