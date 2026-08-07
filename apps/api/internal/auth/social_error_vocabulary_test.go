package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A ?social_error= code is only half a feature. The endpoint puts it in the
// address bar; apps/web/src/features/auth/social-errors.ts is what turns it
// into a sentence, and a code with no case there falls through to "Sign-in
// failed. Please try again." The API-side work is then invisible: the refusal
// still reads as the generic failure it was supposed to replace.
//
// That is exactly how social_provider_already_linked shipped inert, so the two
// sides are held together here rather than by remembering.

var (
	socialFailLiteral = regexp.MustCompile(`socialFail\(c, "([a-z_]+)"\)`)
	webErrorCase      = regexp.MustCompile(`case "([a-z_]+)":`)
)

// socialErrorCodesFromSource returns every code this package can put in
// social_error: the literals socialFail is called with, plus the service codes
// the callback passes through.
func socialErrorCodesFromSource(t *testing.T) []string {
	t.Helper()

	codes := map[string]bool{}
	for code := range actionableSocialCodes {
		codes[code] = true
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range socialFailLiteral.FindAllStringSubmatch(string(src), -1) {
			codes[m[1]] = true
		}
	}

	out := make([]string, 0, len(codes))
	for c := range codes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func TestSocialErrorCodes_AllHaveAWebSentence(t *testing.T) {
	const webFile = "../../../web/src/features/auth/social-errors.ts"

	src, err := os.ReadFile(webFile)
	if err != nil {
		// Not skipped. If this file moves, the guard has to be moved with it,
		// not silently switched off.
		abs, _ := filepath.Abs(webFile)
		t.Fatalf("cannot read the sign-in page's error vocabulary at %s: %v", abs, err)
	}

	handled := map[string]bool{}
	for _, m := range webErrorCase.FindAllStringSubmatch(string(src), -1) {
		handled[m[1]] = true
	}

	codes := socialErrorCodesFromSource(t)
	if len(codes) == 0 {
		t.Fatal("found no social_error codes in this package; the scan is broken, not the vocabulary")
	}

	var missing []string
	for _, code := range codes {
		if !handled[code] {
			missing = append(missing, code)
		}
	}
	if len(missing) > 0 {
		t.Errorf("these codes reach the browser with no sentence in %s, so the user sees the generic failure:\n  %s",
			webFile, strings.Join(missing, "\n  "))
	}
}
