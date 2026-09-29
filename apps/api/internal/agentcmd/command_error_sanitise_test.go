package agentcmd

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
)

// agentFailure builds an AgentFailed() command error with the given agent
// fields, the shape agents 0.61.150 and later send.
func agentFailure(message, exception, at string) *CommandError {
	return &CommandError{
		Command:     "backup",
		Status:      500,
		Code:        commandFailedCode,
		Message:     message,
		DataCommand: "backup",
		Exception:   exception,
		At:          at,
		DataStatus:  500,
	}
}

const goodAt = "includes/commands/class-backup-command.php:239"

// TestSanitizeReason_Redacts: every link, address, hostname, absolute path
// and separator the sanitiser must remove, each with the text that must not
// survive.
func TestSanitizeReason_Redacts(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		leak   string
		wantIn string
	}{
		{"https url", "see https://evil.example.com/x now", "evil.example.com", "[link]"},
		{"ftp url", "fetch ftp://evil.example/x now", "evil.example", "[link]"},
		{"defanged scheme", "open hxxps://evil.example/x now", "evil.example", "[link]"},
		{"protocol-relative", "load //evil.example/x now", "evil.example", "[link]"},
		{"bare www host", "restore it at www.wpmgr-keyhelp.com today", "wpmgr-keyhelp", "[link]"},
		{"bare host with path", "see wpmgr-keyhelp.com/restore now", "wpmgr-keyhelp", "[link]"},
		{"bare host alone", "mail evil-wpmgr.com now", "evil-wpmgr", "[link]"},
		{"email address", "mail billing@wpmgr-keyhelp.com please", "wpmgr-keyhelp", "[address]"},
		{"unc path", `share \\fileserver\share\x.php failed`, "fileserver", "[path]"},
		{"posix absolute path", "cannot open /var/www/html/wp-content/x.php", "/var/www", "[path]"},
		{"single-segment absolute path", "cannot open /etc now", "/etc", "[path]"},
		{"windows path", `cannot open C:\inetpub\wwwroot\x.php here`, "inetpub", "[path]"},
		{"windows forward-slash path", "cannot open D:/sites/x.php here", "sites", "[path]"},
		{"url with an ip host", "fetch http://203.0.113.9/payload now", "203.0.113.9", "[link]"},
		{"www host with a file-extension label", "open www.evil-help.php now", "evil-help", "[link]"},
		{"host with a path ending in .php", "see evil.com/x.php now", "evil.com", "[link]"},
		{"hex token", "key 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b rejected", "9f86d081", "[redacted]"},
		{"base64 token", "key dGhpcyBpcyBhIHNlY3JldCBrZXkgdmFsdWU= rejected", "dGhpcyBp", "[redacted]"},
		{"jwt-like token", "token eyJhbGciOiJFZERTQSJ9-eyJzdWIiOiJ4In0_abcDEF rejected", "eyJhbGci", "[redacted]"},
		// NFKC and the ideographic full stop.
		{"full-width dot host", "mail evil-wpmgr\uff0ecom/login now", "evil-wpmgr", "[link]"},
		{"ideographic dot host", "mail evil-wpmgr\u3002com now", "evil-wpmgr", "[link]"},
		{"halfwidth ideographic dot host", "mail evil-wpmgr\uff61com now", "evil-wpmgr", "[link]"},
		{"small full stop host", "mail evil-wpmgr\ufe52com now", "evil-wpmgr", "[link]"},
		{"full-width www host", "visit \uff57\uff57\uff57\uff0eevil-wpmgr\uff0ecom now", "evil-wpmgr", "[link]"},
		{"full-width at sign", "mail billing\uff20evil-wpmgr.com please", "evil-wpmgr", "[address]"},
		// Hosts in any script.
		{"unicode www host", "restore it at www.\u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444 today", "\u043f\u0440\u0438\u043c\u0435\u0440", "[link]"},
		{"unicode host with path", "see \u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444/login now", "\u043f\u0440\u0438\u043c\u0435\u0440", "[link]"},
		{"bare unicode host", "mail \u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444 now", "\u043f\u0440\u0438\u043c\u0435\u0440", "[link]"},
		{"unicode host after a hyphen", "see --\u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444 now", "\u043f\u0440\u0438\u043c\u0435\u0440", "[link]"},
		{"ideographic www host", "visit www\u3002\u4f8b\u3048\u3002\u307f\u3093\u306a now", "\u4f8b\u3048", "[link]"},
		// IPv4.
		{"bare ipv4", "connect to 203.0.113.9 failed", "203.0.113", "[link]"},
		{"ipv4 with a path", "reset at 185.199.108.153/reset now", "185.199", "[link]"},
		{"ipv4 path is swallowed with the address", "reset at 185.199.108.153/reset now", "/reset", "[link]"},
		{"ipv4 with a port", "connect to 203.0.113.9:8443 failed", "203.0.113", "[link]"},
		{"ipv4 with a port and path", "open 203.0.113.9:8443/login now", "203.0.113", "[link]"},
		{"ipv4 at the end", "connect to 203.0.113.9", "203.0.113", "[link]"},
		// An underscore, or a letter outside ASCII, in front of the name.
		{"host after an underscore", "see _evil.com now", "evil.com", "[link]"},
		{"ipv4 after an underscore", "_185.199.108.153/reset", "185.199", "[link]"},
		{"ipv4 path after an underscore", "_185.199.108.153/reset", "/reset", "[link]"},
		{"www host after an underscore", "_www.evil-wpmgr.com/login", "evil-wpmgr", "[link]"},
		{"www label after an underscore", "_www.evil-wpmgr.com/login", "www", "[link]"},
		{"ipv4 after a non-ascii letter", "\u00e9185.199.108.153/reset", "185.199", "[link]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeReason(tt.in, "")
			if strings.Contains(got, tt.leak) {
				t.Errorf("sanitizeReason(%q) = %q, leaked %q", tt.in, got, tt.leak)
			}
			if !strings.Contains(got, tt.wantIn) {
				t.Errorf("sanitizeReason(%q) = %q, want it to contain %q", tt.in, got, tt.wantIn)
			}
			if strings.Contains(got, "://") {
				t.Errorf("sanitizeReason(%q) = %q, a scheme survived", tt.in, got)
			}
		})
	}
}

// TestSanitizeReason_SeparatorsBecomeSpaces: U+2028, U+2029, NBSP and other
// Unicode spaces cannot start a new line or survive as themselves.
func TestSanitizeReason_SeparatorsBecomeSpaces(t *testing.T) {
	for _, r := range []rune{'\u2028', '\u2029', '\u00a0', '\u3000', '\u2003'} {
		in := "a" + string(r) + string(r) + "URGENT: your backups are deleted"
		got := sanitizeReason(in, "")
		if strings.ContainsRune(got, r) {
			t.Errorf("U+%04X survived: %q", r, got)
		}
		if got != "a URGENT: your backups are deleted" {
			t.Errorf("U+%04X: sanitizeReason = %q, want the run collapsed to one space", r, got)
		}
	}
}

// TestSanitizeReason_KeepsHonestText: relative paths and file names are what
// an operator needs to see, and must not be mangled.
func TestSanitizeReason_KeepsHonestText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"cannot open includes/commands/class-backup-command.php", "cannot open includes/commands/class-backup-command.php"},
		{"wp-config.php is not writable", "wp-config.php is not writable"},
		{"dump.sql.gz is truncated", "dump.sql.gz is truncated"},
		{"PHP 8.2 required, e.g. upgrade", "PHP 8.2 required, e.g. upgrade"},
		{"and/or retry", "and/or retry"},
		{"wp-content/plugins/fleet-agent-site-manager/includes/commands", "wp-content/plugins/fleet-agent-site-manager/includes/commands"},
		{"version 1.2.3 required", "version 1.2.3 required"},
		{"\u0444\u0430\u0439\u043b.php is missing", "\u0444\u0430\u0439\u043b.php is missing"},
		{"snake_case.php and other_file.json", "snake_case.php and other_file.json"},
		{"requires v1.2.3.4 or later", "requires v1.2.3.4 or later"},
		{"\uff46\uff55\uff4c\uff4c width text", "full width text"},
	}
	for _, tt := range tests {
		if got := sanitizeReason(tt.in, ""); got != tt.want {
			t.Errorf("sanitizeReason(%q) = %q, want it unchanged", tt.in, got)
		}
	}
}

// TestSanitizeReason_CapsAt200: the reason alone is capped at 200 bytes, on a
// rune boundary.
func TestSanitizeReason_CapsAt200(t *testing.T) {
	in := strings.Repeat("ab é ", 120) // 720 bytes, spaced so nothing is redacted
	got := sanitizeReason(in, "")
	if len(got) > 200 {
		t.Errorf("sanitizeReason = %d bytes, want <= 200", len(got))
	}
	if len(got) < 190 {
		t.Errorf("sanitizeReason = %d bytes, want the cap to keep close to 200", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeReason split a rune: %q", got)
	}
}

// TestSanitizeReason_RedactsAfterCap: however the 200-byte cap cuts a kept
// file name, what is left is never a bare hostname. The cut is moved across
// every byte of "wpmgr-help.com.js".
func TestSanitizeReason_RedactsAfterCap(t *testing.T) {
	const name = "wpmgr-help.com.js"
	for cut := 1; cut <= len(name); cut++ {
		// Filler of single letters and spaces, so the cap falls exactly
		// after name[:cut] and nothing in the filler is redacted.
		filler := strings.Repeat("a ", (maxReasonLen-cut)/2)
		if len(filler)+cut < maxReasonLen {
			filler = "b" + filler
		}
		in := filler + name + " tail"
		got := sanitizeReason(in, "")
		if len(got) > maxReasonLen {
			t.Fatalf("cut %d: %d bytes, want <= %d", cut, len(got), maxReasonLen)
		}
		if strings.Contains(got, "wpmgr-help.co") && !strings.Contains(got, name) {
			t.Errorf("cut %d: sanitizeReason left a bare host: %q", cut, got[len(got)-min(len(got), 30):])
		}
		if again := redactReason(got); again != got {
			t.Errorf("cut %d: output is not fully redacted:\n  got   %q\n  again %q", cut, got, again)
		}
	}
}

// TestSanitizeReason_OutputIsFullyRedacted: for many texts cut at many
// points, the output fits the cap, is valid UTF-8, and redacting it again
// changes nothing, so no link, address, host or path survived anywhere.
func TestSanitizeReason_OutputIsFullyRedacted(t *testing.T) {
	tokens := []string{
		"evil-wpmgr.com", "www.evil.example", "wpmgr-help.com.js", "dump.sql.gz",
		"203.0.113.9", "185.199.108.153/reset", "billing@evil.example", "https://evil.example/x",
		"/var/www/html/x.php", `C:\inetpub\x.php`, `\\srv\share`, "includes/commands/class-x.php",
		"\u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444", "evil\u3002com", "\uff57\uff57\uff57\uff0eevil\uff0ecom",
		"disk", "full", "\u00e9t\u00e9", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b", "PHP", "8.2", "e.g.",
	}
	// A fixed linear congruential sequence keeps the test deterministic.
	seed := uint32(791)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for i := 0; i < 2000; i++ {
		var b strings.Builder
		want := 150 + next(150)
		for b.Len() < want {
			b.WriteString(tokens[next(len(tokens))])
			b.WriteByte(" ,;:()"[next(6)])
		}
		in := b.String()
		got := sanitizeReason(in, "")
		if len(got) > maxReasonLen || !utf8.ValidString(got) {
			t.Fatalf("input %q: output %d bytes, valid UTF-8 %v", in, len(got), utf8.ValidString(got))
		}
		if again := redactReason(got); again != got {
			t.Fatalf("input %q:\n  got   %q\n  again %q", in, got, again)
		}
	}
}

// TestOperatorMessage_CapsAt512: with every field at its longest allowed
// size the composed message would pass 512 bytes; the cap holds it there.
func TestOperatorMessage_CapsAt512(t *testing.T) {
	exc := "E" + strings.Repeat("x", 127) // 128 bytes, the longest allowed class
	at := strings.Repeat("d/", 90) + "file.php:123456"
	if sanitizeException(exc) == "" || sanitizeAt(at) == "" {
		t.Fatalf("fixture fields rejected by the sanitiser: exc=%q at=%q", sanitizeException(exc), sanitizeAt(at))
	}
	ce := agentFailure("Command execution failed: "+exc+": "+strings.Repeat("word ", 80), exc, at)
	for _, got := range []string{ce.OperatorMessage("Backup"), ce.NotificationMessage("Backup")} {
		if len(got) > 512 {
			t.Errorf("message = %d bytes, want <= 512", len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("message is not valid UTF-8: %q", got)
		}
	}
	if len(ce.OperatorMessage("Backup")) < 500 {
		t.Errorf("OperatorMessage = %d bytes; the fixture is meant to hit the 512 cap", len(ce.OperatorMessage("Backup")))
	}
}

// TestSanitizeAt: only a relative "dir/file.php:line" of at most 200 bytes
// survives.
func TestSanitizeAt(t *testing.T) {
	keep := []string{
		goodAt,
		"x.php:1",
		"wp-content/plugins/fleet-agent/includes/class-x.y.php:12",
	}
	for _, at := range keep {
		if got := sanitizeAt(at); got != at {
			t.Errorf("sanitizeAt(%q) = %q, want it kept", at, got)
		}
	}
	drop := []string{
		"/var/www/html/wp-content/x.php:239",
		"includes/../../etc/x.php:1",
		"../x.php:1",
		"./x.php:1",
		"includes/./x.php:1",
		`C:\inetpub\x.php:1`,
		`includes\x.php:1`,
		"www.wpmgr-keyhelp.com/restore:1",
		"evil.example/x.php:1",
		"includes/x.js:1",
		"includes/x.php",
		"includes/x.php:1234567",
		"(unknown):0",
		"includes/x.php:1\nnext",
		strings.Repeat("d/", 100) + "x.php:1", // 207 bytes
	}
	for _, at := range drop {
		if got := sanitizeAt(at); got != "" {
			t.Errorf("sanitizeAt(%q) = %q, want it dropped", at, got)
		}
	}
}

// TestOperatorMessage_DropsBadException: an exception that is not a PHP class
// name never appears, in either message.
func TestOperatorMessage_DropsBadException(t *testing.T) {
	for _, exc := range []string{"Evil https://x.example", "<script>", "Runtime Exception", "...Exception", "Ex\u202eception"} {
		ce := agentFailure("Command execution failed: "+exc+": disk full", exc, goodAt)
		for _, got := range []string{ce.OperatorMessage("Backup"), ce.NotificationMessage("Backup")} {
			if strings.Contains(got, exc) {
				t.Errorf("exception %q shown verbatim: %q", exc, got)
			}
			if !strings.Contains(got, "Agent error at "+goodAt) {
				t.Errorf("exception %q: message %q lost the location", exc, got)
			}
		}
	}
}

// TestOperatorMessage_Copy covers the wording in the cases the review found
// awkward: no reason, a shortened class, and a relative path in the reason.
func TestOperatorMessage_Copy(t *testing.T) {
	const head = "Backup failed: the WPMgr agent on this site stopped with an error, so the backup was not retried. "
	const tail = "Fix the cause on the site, then run the backup again."
	tests := []struct {
		name string
		ce   *CommandError
		want string
	}{
		{
			"reason, class and location",
			agentFailure("Command execution failed: RuntimeException: disk full", "RuntimeException", goodAt),
			head + "Agent error: RuntimeException at " + goodAt + ": disk full. " + tail,
		},
		{
			"agent dropped the reason",
			agentFailure("Command execution failed: RuntimeException", "RuntimeException", goodAt),
			head + "Agent error: RuntimeException at " + goodAt + ". " + tail,
		},
		{
			"bare generic message",
			agentFailure("Command execution failed.", "RuntimeException", goodAt),
			head + "Agent error: RuntimeException at " + goodAt + ". " + tail,
		},
		{
			"class shortened in data.exception only",
			agentFailure(`Command execution failed: Vendor\Pkg\VeryLongException: disk full`, "...LongException", goodAt),
			head + "Agent error at " + goodAt + ": disk full. " + tail,
		},
		{
			"class shortened in the message",
			agentFailure("Command execution failed: ...LongException", "...LongException", goodAt),
			head + "Agent error at " + goodAt + ". " + tail,
		},
		{
			"relative path in the reason is kept",
			agentFailure("Command execution failed: RuntimeException: cannot read includes/commands/class-x.php", "RuntimeException", goodAt),
			head + "Agent error: RuntimeException at " + goodAt + ": cannot read includes/commands/class-x.php. " + tail,
		},
		{
			"nothing survives the sanitiser",
			agentFailure("Command execution failed.", "<bad>", "/abs/x.php:1"),
			head + tail,
		},
		{
			"reason only",
			agentFailure("Command execution failed: <bad>: disk full", "<bad>", "/abs/x.php:1"),
			head + "Agent error: disk full. " + tail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ce.OperatorMessage("Backup")
			if got != tt.want {
				t.Errorf("OperatorMessage =\n  %q\nwant\n  %q", got, tt.want)
			}
			if strings.Contains(got, ": .") || strings.Contains(got, "Command execution failed") {
				t.Errorf("awkward text in %q", got)
			}
		})
	}
}

// TestNotificationMessage_NoAgentProse: the email text carries the class and
// location but never the agent's reason, however harmless it looks.
func TestNotificationMessage_NoAgentProse(t *testing.T) {
	ce := agentFailure("Command execution failed: RuntimeException: plainprose disk full, visit www.evil.example", "RuntimeException", goodAt)
	got := ce.NotificationMessage("Backup")
	want := "Backup failed: the WPMgr agent on this site stopped with an error, so the backup was not retried. " +
		"Agent error: RuntimeException at " + goodAt + ". " +
		"Fix the cause on the site, then run the backup again. The WPMgr dashboard shows the agent's full message."
	if got != want {
		t.Errorf("NotificationMessage =\n  %q\nwant\n  %q", got, want)
	}
	if !strings.Contains(ce.OperatorMessage("Backup"), "plainprose") {
		t.Errorf("OperatorMessage lost the reason; the dashboard is where it belongs")
	}

	old := &CommandError{Command: "backup", Status: 500, Code: commandFailedCode, Message: "Command execution failed.", DataStatus: 500}
	if got := old.NotificationMessage("Backup"); !strings.Contains(got, "update the WPMgr agent on this site to see it") {
		t.Errorf("old-agent NotificationMessage = %q, want the update copy", got)
	}
}

// TestNotificationMessage_DropsForgedFields: the email text carries the
// exception and location only when each passes its allow-list. A forged
// value is dropped entirely, never partly shown.
func TestNotificationMessage_DropsForgedFields(t *testing.T) {
	const noHead = "Backup failed: the WPMgr agent on this site stopped with an error, so the backup was not retried. " +
		"Fix the cause on the site, then run the backup again. The WPMgr dashboard shows the agent's full message."
	tests := []struct {
		name, exception, at, want string
		leaks                     []string
	}{
		{
			"forged at and forged exception",
			"Evil www.evil-exc.example", "www.evil-at.example/login:1",
			noHead,
			[]string{"evil-exc", "evil-at", "www.", "login"},
		},
		{
			"forged at, good exception",
			"RuntimeException", "www.evil-at.example/restore.php:1",
			"Backup failed: the WPMgr agent on this site stopped with an error, so the backup was not retried. " +
				"Agent error: RuntimeException. " +
				"Fix the cause on the site, then run the backup again. The WPMgr dashboard shows the agent's full message.",
			[]string{"evil-at", "www.", "restore.php"},
		},
		{
			"good at, forged exception",
			"https://evil-exc.example/x", goodAt,
			"Backup failed: the WPMgr agent on this site stopped with an error, so the backup was not retried. " +
				"Agent error at " + goodAt + ". " +
				"Fix the cause on the site, then run the backup again. The WPMgr dashboard shows the agent's full message.",
			[]string{"evil-exc", "https"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := agentFailure("Command execution failed: x: disk full", tt.exception, tt.at)
			got := ce.NotificationMessage("Backup")
			if got != tt.want {
				t.Errorf("NotificationMessage =\n  %q\nwant\n  %q", got, tt.want)
			}
			for _, leak := range tt.leaks {
				if strings.Contains(got, leak) {
					t.Errorf("NotificationMessage = %q leaked %q", got, leak)
				}
			}
		})
	}
}

// TestAgentFailed_ShapeChecks kills the partial-shape cases: data.status must
// be 500, and a field of the wrong type makes the whole body not agent-shaped.
func TestAgentFailed_ShapeChecks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"canonical", `{"code":"wpmgr_command_failed","message":"m","data":{"status":500,"command":"backup"}}`, true},
		{"data.status 503", `{"code":"wpmgr_command_failed","message":"m","data":{"status":503,"command":"backup"}}`, false},
		{"data.status absent", `{"code":"wpmgr_command_failed","message":"m","data":{"command":"backup"}}`, false},
		{"message of the wrong type", `{"code":"wpmgr_command_failed","message":123,"data":{"status":500,"command":"backup"}}`, false},
		{"exception of the wrong type", `{"code":"wpmgr_command_failed","message":"m","data":{"status":500,"command":"backup","exception":["x"]}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := newCommandError("backup", 500, tt.body, []byte(tt.body))
			if got := ce.AgentFailed(); got != tt.want {
				t.Errorf("AgentFailed() = %v, want %v; ce = %+v", got, tt.want, ce)
			}
			if !tt.want && strings.Contains(tt.name, "wrong type") && ce.Code != "" {
				t.Errorf("a body that did not decode kept Code = %q", ce.Code)
			}
		})
	}
}

// timeoutErr is a net.Error whose Timeout() is true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestDescribeAttemptError: every branch, and never any raw response text.
func TestDescribeAttemptError(t *testing.T) {
	ce := func(status int, body string) error {
		return fmt.Errorf("wrapped: %w", newCommandError("backup", status, body, []byte(body)))
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"403 wpmgr code", ce(403, `{"code":"wpmgr_token_expired","message":"token expired","data":{"status":403}}`),
			"The WPMgr agent refused the request (wpmgr_token_expired)."},
		{"403 core rest code", ce(403, `{"code":"rest_forbidden","message":"Sorry","data":{"status":403}}`),
			"The site refused the request (HTTP 403, rest_forbidden)."},
		{"403 wpmgr code with unsafe characters", ce(403, `{"code":"wpmgr_x<b>evil</b>","message":"m"}`),
			"The site refused the request (HTTP 403)."},
		{"403 html", ce(403, `<html>Forbidden</html>`), "The site refused the request (HTTP 403)."},
		{"404", ce(404, `{"code":"rest_no_route"}`), "The site did not find the WPMgr agent (HTTP 404). Check that the plugin is active."},
		{"500 WordPress critical error", ce(500, `{"code":"internal_server_error","message":"<p>There has been a critical error on this website.</p>","data":{"status":500},"additional_errors":[]}`),
			"WordPress on the site hit a critical error (HTTP 500)."},
		{"500 web server page", ce(500, `<html><body><h1>500 Internal Server Error</h1>nginx</body></html>`), "The site returned a server error (HTTP 500)."},
		{"500 notice before the agent body", ce(500, "Notice: Undefined index in x.php on line 3\n"+`{"code":"wpmgr_command_failed","message":"m","data":{"status":500}}`),
			"The site returned a server error (HTTP 500)."},
		{"500 byte-order mark before a WordPress body", ce(500, "\uFEFF"+`{"code":"internal_server_error","data":{"status":500}}`), "The site returned a server error (HTTP 500)."},
		{"500 agent body for another command", ce(500, `{"code":"wpmgr_command_failed","message":"m","data":{"status":500,"command":"restore"}}`),
			"The site returned a server error (HTTP 500)."},
		{"500 other json code", ce(500, `{"code":"rest_error","data":{"status":500}}`), "The site returned a server error (HTTP 500)."},
		{"502", ce(502, `<html>Bad Gateway</html>`), "The site returned a server error (HTTP 502) that did not come from the WPMgr agent."},
		{"418", ce(418, `short and stout`), "The site answered HTTP 418."},
		{"401", ce(401, `{"code":"wpmgr_unauthorized"}`), "The site answered HTTP 401."},
		{"timeout", fmt.Errorf("backup command transport: %w", timeoutErr{}), "The site did not answer in time."},
		{"deadline", fmt.Errorf("backup command transport: %w", context.DeadlineExceeded), "The site did not answer in time."},
		{"tls", fmt.Errorf("backup command transport: %w", x509.UnknownAuthorityError{}), "The site's HTTPS certificate was not accepted."},
		{"refused", errors.New("backup command transport: dial tcp: connection refused"), "Could not connect to the site."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DescribeAttemptError(tt.err)
			if got != tt.want {
				t.Errorf("DescribeAttemptError = %q, want %q", got, tt.want)
			}
			for _, leak := range []string{"body=", "<", "rejected by agent", "Sorry", "stout", "Gateway"} {
				if strings.Contains(got, leak) {
					t.Errorf("DescribeAttemptError = %q leaked %q", got, leak)
				}
			}
		})
	}
}

// TestDescribeAttemptError_NoticeBeforeJSONOn200: a 200 reply with a PHP
// notice ahead of the JSON is an answer from the site, not a connection
// failure. Driven through the real client.
func TestDescribeAttemptError_NoticeBeforeJSONOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Notice: Undefined index: foo in x.php on line 3\n" + `{"ok":true}`))
	}))
	defer srv.Close()

	client := buildTestAgentClient(t, srv)
	_, err := client.Backup(context.Background(), uuid.New(), srv.URL, BackupRequest{})
	if err == nil {
		t.Fatal("Backup: expected a decode error, got nil")
	}
	const want = "The site answered, but the reply was not the WPMgr agent's response. A PHP notice or another plugin's output may be in the way."
	if got := DescribeAttemptError(err); got != want {
		t.Errorf("DescribeAttemptError = %q, want %q (err %v)", got, want, err)
	}
}
