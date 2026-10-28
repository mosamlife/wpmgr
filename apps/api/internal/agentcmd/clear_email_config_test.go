package agentcmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// clearEmailAgent serves one canned answer for clear_email_config and records
// the request it was sent.
func clearEmailAgent(t *testing.T, status int, body string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var gotReq http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = *r
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotReq, &gotBody
}

// TestClearEmailConfig_ConfirmedClearSucceeds pins the wire shape: the command
// is posted to its own endpoint with an empty JSON object, and the agent's
// answer decodes into the result.
func TestClearEmailConfig_ConfirmedClearSucceeds(t *testing.T) {
	srv, req, body := clearEmailAgent(t, http.StatusOK,
		`{"ok":true,"cleared":true,"detail":"email config cleared","existed":{"config":true,"secret":true,"connection_secrets":false}}`)

	res, err := buildTestAgentClient(t, srv).ClearEmailConfig(context.Background(), uuid.New(), srv.URL)
	if err != nil {
		t.Fatalf("ClearEmailConfig: %v", err)
	}
	if !strings.HasSuffix(req.URL.Path, "/wp-json/wpmgr/v1/command/clear_email_config") {
		t.Fatalf("posted to %s", req.URL.Path)
	}
	if req.Method != http.MethodPost || string(*body) != "{}" {
		t.Fatalf("sent %s %q, want POST {}", req.Method, *body)
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		t.Fatal("the command was sent unsigned")
	}
	if !res.Cleared || !res.OK || !res.Existed.Config || !res.Existed.Secret || res.Existed.ConnectionSecrets {
		t.Fatalf("result = %+v", res)
	}
}

// TestClearEmailConfig_AlreadyClearIsSuccess: the command is idempotent, and
// a site that held nothing answers cleared=true.
func TestClearEmailConfig_AlreadyClearIsSuccess(t *testing.T) {
	srv, _, _ := clearEmailAgent(t, http.StatusOK,
		`{"ok":true,"cleared":true,"detail":"email config already clear","existed":{"config":false,"secret":false,"connection_secrets":false}}`)
	if _, err := buildTestAgentClient(t, srv).ClearEmailConfig(context.Background(), uuid.New(), srv.URL); err != nil {
		t.Fatalf("an already-clear site was reported as a failure: %v", err)
	}
}

// TestClearEmailConfig_NotClearedIsAnError: the agent answers 200 whatever
// happened, so cleared=false must come back as an error carrying its detail.
func TestClearEmailConfig_NotClearedIsAnError(t *testing.T) {
	const detail = "email settings removed; the stored email secret could not be removed"
	srv, _, _ := clearEmailAgent(t, http.StatusOK,
		`{"ok":false,"cleared":false,"detail":"`+detail+`","existed":{"config":true,"secret":true,"connection_secrets":false}}`)

	res, err := buildTestAgentClient(t, srv).ClearEmailConfig(context.Background(), uuid.New(), srv.URL)
	if err == nil {
		t.Fatal("cleared=false was reported as success")
	}
	if !errors.Is(err, ErrEmailConfigNotCleared) || !strings.Contains(err.Error(), detail) {
		t.Fatalf("err = %v, want ErrEmailConfigNotCleared with the agent's detail", err)
	}
	if res.Cleared || res.Detail != detail {
		t.Fatalf("result = %+v", res)
	}
	if IsUnknownCommand(err) {
		t.Fatal("a partial clear was classified as an old agent")
	}
}

// TestClearEmailConfig_UnknownCommandIsTheOldAgentAnswer: an agent that
// predates the command answers 404 wpmgr_unknown_command, and only that 404
// is the old-agent answer.
func TestClearEmailConfig_UnknownCommandIsTheOldAgentAnswer(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"old agent", `{"code":"wpmgr_unknown_command","message":"Unknown command.","data":{"status":404}}`, true},
		{"plugin not active", `{"code":"rest_no_route","message":"No route was found matching the URL and request method.","data":{"status":404}}`, false},
		{"html 404 page", `<html><body>Not Found</body></html>`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := clearEmailAgent(t, http.StatusNotFound, tc.body)
			_, err := buildTestAgentClient(t, srv).ClearEmailConfig(context.Background(), uuid.New(), srv.URL)
			if err == nil {
				t.Fatal("a 404 was reported as success")
			}
			if got := IsUnknownCommand(err); got != tc.want {
				t.Fatalf("IsUnknownCommand = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}

// TestIsUnknownCommandNeedsTheStatusToo: the code alone, on another status,
// is not the old-agent answer.
func TestIsUnknownCommandNeedsTheStatusToo(t *testing.T) {
	if IsUnknownCommand(&CommandError{Command: ClearEmailCommand, Status: 500, Code: unknownCommandCode}) {
		t.Fatal("a 500 carrying the code was classified as an old agent")
	}
	if IsUnknownCommand(errors.New("clear_email_config command rejected by agent: status 404 body=")) {
		t.Fatal("an untyped error was classified as an old agent")
	}
	if IsUnknownCommand(nil) {
		t.Fatal("nil was classified as an old agent")
	}
}
