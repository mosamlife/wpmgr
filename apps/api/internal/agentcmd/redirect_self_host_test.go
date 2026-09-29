package agentcmd

import (
	"strings"
	"testing"
)

// TestRedirect_SelfRedirectComparesTheDialledHost: a redirect whose target is
// the saved address with its host spelt another way that dials the same
// HostKey is a redirect back to the saved address, suggests nothing, and says
// so. A target whose host only Unicode lowercasing matches is another host.
func TestRedirect_SelfRedirectComparesTheDialledHost(t *testing.T) {
	cases := []struct {
		name, saved, loc string
		self             bool
	}{
		{"non-ASCII letter case", "https://BÜCHER.de", "https://bücher.de" + pingRoute, true},
		{"Punycode spelling", "https://bücher.de", "https://xn--bcher-kva.de" + pingRoute, true},
		{"ASCII letter case", "https://Example.COM", "https://example.com" + pingRoute, true},
		{"dotted capital I vs its Unicode lowercase", "https://İstanbul.test", "https://istanbul.test" + pingRoute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			re := redirectFor(t, "ping", c.saved, c.loc, 301)
			if got := re.SelfRedirect(); got != c.self {
				t.Fatalf("SelfRedirect() = %v, want %v (From %q, To %q)", got, c.self, re.From, re.To)
			}
			if re.SuggestedSiteURL != "" {
				t.Errorf("SuggestedSiteURL = %q, want none", re.SuggestedSiteURL)
			}
			if back := strings.Contains(re.Explanation(), "back to itself"); back != c.self {
				t.Errorf("Explanation = %q, self-redirect copy %v, want %v", re.Explanation(), back, c.self)
			}
		})
	}
}
