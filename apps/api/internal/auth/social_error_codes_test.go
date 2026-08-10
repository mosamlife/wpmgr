package auth

// social_error_codes_test.go: the ?social_error= contract, checked against the
// code that produces it rather than against a second copy of the list.
//
// THE BUG THIS EXISTS FOR was a passing test. The sign-in page's own test named
// this package as its source of truth and pinned three codes the server has
// never emitted, one of which (social_rate_limited) asserted a rate limit that
// did not exist anywhere. Nothing failed, because both ends of the "contract"
// were hand-written lists that only had to agree with each other.
//
// So the list is derived here from the handler source: every socialFail call
// site that names a code, plus actionableSocialCodes, which is the only other
// way a code reaches the redirect. socialErrorCodes must be exactly that set,
// and the web copy is then checked against socialErrorCodes
// (apps/web/src/features/auth/social-errors.test.ts).

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	socialFailCallSite   = regexp.MustCompile(`h\.socialFail\(c,\s*([^,]+),`)
	quotedCode           = regexp.MustCompile(`^"([a-z_]+)"$`)
	actionableCodesBlock = regexp.MustCompile(`(?s)var actionableSocialCodes = map\[string\]bool\{(.*?)\n\}`)
	mapKey               = regexp.MustCompile(`"([a-z_]+)":`)
)

// TestSocialErrorCodesAreExactlyWhatTheHandlerEmits refuses drift in either
// direction: a code the handler can emit that the table does not list (so the
// sign-in page was never asked for a sentence for it), and a code the table
// lists that nothing emits (which is how the web test came to describe a rate
// limiter that was not there).
func TestSocialErrorCodesAreExactlyWhatTheHandlerEmits(t *testing.T) {
	src, err := os.ReadFile("social_handler.go")
	if err != nil {
		t.Fatalf("read social_handler.go: %v", err)
	}
	source := string(src)

	emitted := map[string]bool{}
	for _, m := range socialFailCallSite.FindAllStringSubmatch(source, -1) {
		arg := strings.TrimSpace(m[1])
		switch {
		case quotedCode.MatchString(arg):
			emitted[quotedCode.FindStringSubmatch(arg)[1]] = true
		case arg == "de.Code":
			// The one indirect call site, and it is guarded by
			// actionableSocialCodes, which is folded in below.
		default:
			t.Errorf("socialFail is called with %q, which this test cannot resolve to a code. "+
				"Either name the code inline or add it to a table this test reads, or the "+
				"sign-in page can be handed a code nobody wrote a sentence for.", arg)
		}
	}

	block := actionableCodesBlock.FindStringSubmatch(source)
	if block == nil {
		t.Fatal("actionableSocialCodes not found in social_handler.go: the derivation below is now blind")
	}
	for _, m := range mapKey.FindAllStringSubmatch(block[1], -1) {
		emitted[m[1]] = true
	}
	if len(emitted) == 0 {
		t.Fatal("no codes were derived from the handler; the patterns in this test have gone stale")
	}

	for code := range emitted {
		if _, ok := socialErrorCodes[code]; !ok {
			t.Errorf("the handler can emit %q but socialErrorCodes does not list it, "+
				"so the sign-in page was never asked to answer it", code)
		}
	}
	for code := range socialErrorCodes {
		if !emitted[code] {
			t.Errorf("socialErrorCodes lists %q but no handler path emits it. "+
				"A code nobody sends is a contract nobody implements, which is exactly "+
				"how the web test came to assert a rate limit that did not exist.", code)
		}
	}
	if t.Failed() {
		t.Logf("derived from the handler: %v", sortedKeys(emitted))
		t.Logf("declared in socialErrorCodes: %v", sortedKeys(socialErrorCodes))
	}
}

// The actionable refusals are the ones a person can do something about, so each
// of them must have its own sentence rather than the generic one.
func TestActionableRefusalsAreNotCoarse(t *testing.T) {
	for code := range actionableSocialCodes {
		coarse, ok := socialErrorCodes[code]
		if !ok {
			t.Errorf("actionable refusal %q is missing from socialErrorCodes", code)
			continue
		}
		if coarse {
			t.Errorf("%q is an actionable refusal but is marked coarse, which renders it as "+
				"\"Sign-in failed. Please try again.\" and drops the next step the person needs", code)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
