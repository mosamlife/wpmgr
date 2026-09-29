package agentcmd

import "testing"

// TestJoinCommandURL_TrimsTrailingSlashes: a saved address with one or more
// trailing slashes, at the root or under a path, builds the same command URL
// as the address without them, so the route never carries an empty segment.
func TestJoinCommandURL_TrimsTrailingSlashes(t *testing.T) {
	cases := []struct{ site, want string }{
		{"https://x.test", "https://x.test" + pingRoute},
		{"https://x.test/", "https://x.test" + pingRoute},
		{"https://x.test//", "https://x.test" + pingRoute},
		{"https://x.test/blog", "https://x.test/blog" + pingRoute},
		{"https://x.test/blog/", "https://x.test/blog" + pingRoute},
		{"  http://x.test:8080/blog///  ", "http://x.test:8080/blog" + pingRoute},
	}
	for _, c := range cases {
		got, err := joinCommandURL(c.site, "ping")
		if err != nil {
			t.Errorf("joinCommandURL(%q): %v", c.site, err)
			continue
		}
		if got != c.want {
			t.Errorf("joinCommandURL(%q) = %q, want %q", c.site, got, c.want)
		}
	}
}
