package site

import "testing"

// TestReportedAddressKey_Distinct proves reportedAddressKey keeps two
// reported addresses apart whenever they differ in scheme, port or path -
// the property AdoptReportedURLArgs's River unique key depends on, so two
// genuinely different addresses from one site never collapse into one queued
// job - while still joining spellings the canonical form is defined to treat
// as one address (a written default port, or the escaping of the path).
func TestReportedAddressKey_Distinct(t *testing.T) {
	cases := []struct {
		name     string
		a, b     string
		wantSame bool
	}{
		{"scheme differs: https vs http", "https://example.com", "http://example.com", false},
		{"port differs: :80 (http default, dropped) vs :8080", "http://example.com:80", "http://example.com:8080", false},
		{"https default port :443 vs none written: same key", "https://example.com:443", "https://example.com", true},
		{"path differs: root vs /blog", "https://example.com", "https://example.com/blog", false},
		{"path differs: /blog vs /blog2", "https://example.com/blog", "https://example.com/blog2", false},
		{"path case is significant: /blog vs /Blog", "https://example.com/blog", "https://example.com/Blog", false},
		{"escaped slash vs literal slash in the path: one path, same key", "https://example.com/a%2Fb", "https://example.com/a/b", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ka, okA := reportedAddressKey(c.a)
			kb, okB := reportedAddressKey(c.b)
			if !okA || !okB {
				t.Fatalf("reportedAddressKey(%q)=%q ok=%v; reportedAddressKey(%q)=%q ok=%v: both must parse for this case to mean anything",
					c.a, ka, okA, c.b, kb, okB)
			}
			if got := ka == kb; got != c.wantSame {
				t.Errorf("reportedAddressKey(%q)=%q, reportedAddressKey(%q)=%q: same=%v, want %v",
					c.a, ka, c.b, kb, got, c.wantSame)
			}
		})
	}
}

// TestReportedAddressKey_SharersDecideAlike proves that for every pair of
// reports that land on one ReportedKey, planReportedURL (PlanStrict) - the
// rule AdoptReportedURL actually applies once its job runs - decides the
// same way against any one stored address for both. AdoptReportedURLArgs is
// unique by (SiteID, ReportedKey): inside one adoptURLUniqueWindow, only the
// first of two key-sharing reports gets a job at all, so a decision that
// differs between them would make the outcome depend on which one won the
// race to enqueue, invisibly.
func TestReportedAddressKey_SharersDecideAlike(t *testing.T) {
	reports := []string{
		"https://example.com", "https://EXAMPLE.com", "https://example.com:443/",
		"https://www.example.com", "https://WWW.EXAMPLE.COM", "https://www.ｅxample.com", "https://ｗｗｗ.example.com",
		"https://www.bücher.de", "https://www.BÜCHER.de", "https://www.xn--bcher-kva.de", "https://WWW.bücher.de",
		"https://bücher.de", "https://BÜCHER.de", "https://xn--bcher-kva.de",
		"https://example.com/a%2Fb", "https://example.com/a/b",
	}
	stored := []string{
		"http://example.com", "https://example.com", "http://www.example.com",
		"http://bücher.de", "http://BÜCHER.de", "http://xn--bcher-kva.de", "https://bücher.de",
		"http://www.bücher.de", "http://example.com/a/b", "http://example.com/a%2Fb",
	}

	byKey := map[string][]string{}
	for _, r := range reports {
		if k, ok := reportedAddressKey(r); ok {
			byKey[k] = append(byKey[k], r)
		}
	}

	name := map[enrollURLDecision]string{enrollURLSame: "Same", enrollURLAdopt: "Adopt", enrollURLMismatch: "Mismatch"}
	grouped := false
	for k, rs := range byKey {
		if len(rs) < 2 {
			continue // nothing to compare a lone key-holder against
		}
		grouped = true
		for _, s := range stored {
			p0 := planReportedURL(s, rs[0])
			for _, r := range rs[1:] {
				if p := planReportedURL(s, r); p != p0 {
					t.Errorf("key %q, stored %q: %q -> %s %q but %q -> %s %q",
						k, s, rs[0], name[p0.Decision], p0.To, r, name[p.Decision], p.To)
				}
			}
		}
	}
	if !grouped {
		t.Fatal("positive control: no ReportedKey was shared by more than one report; the property this test exists to check was never exercised")
	}
}
