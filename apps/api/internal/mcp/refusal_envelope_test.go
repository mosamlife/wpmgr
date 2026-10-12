package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// The refusal envelope (SP-14) is the control plane's twin of the agent's
// Refusal class: the message opens with the summary and spells out the first
// three failures, so a client that shows only the message still gets the fix,
// and failures[] holds up to twenty of them. The cases below are the same
// numbers the agent's class keeps.

// tail is the last n bytes of s, or all of it.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func rfail(path, key, reason string, allowed ...string) refusalFailure {
	return refusalFailure{Path: path, Key: key, Reason: reason, Allowed: allowed}
}

func TestRefusalMessageInlinesFirstThree(t *testing.T) {
	five := []refusalFailure{
		rfail("design.colors", "ink", "ink on ground has a contrast of 1.05:1"),
		rfail("design.fonts", "heading", "not a font family", "Manrope", "Inter"),
		rfail("design", "density", "missing", "compact", "normal", "airy"),
		rfail("design", "radius", "missing"),
		rfail("design", "scale", "missing"),
	}

	t.Run("the summary, then the first three failures, then the count of the rest", func(t *testing.T) {
		detail, listed := refusalEnvelope("The design is not usable.", five)
		want := "The design is not usable. 1. design.colors.ink: ink on ground has a contrast of 1.05:1; " +
			"2. design.fonts.heading: not a font family (allowed: Manrope, Inter); " +
			"3. design.density: missing (allowed: compact, normal, airy) (2 more in failures)"
		if detail != want {
			t.Errorf("detail =\n%s\nwant\n%s", detail, want)
		}
		if len(listed) != 5 {
			t.Errorf("failures = %d, want all 5", len(listed))
		}
	})

	t.Run("three failures, or fewer, leave no count", func(t *testing.T) {
		detail, listed := refusalEnvelope("Refused.", five[:3])
		if strings.Contains(detail, "more in failures") || len(listed) != 3 {
			t.Errorf("three failures: %q (%d listed)", detail, len(listed))
		}
		detail, _ = refusalEnvelope("Refused.", five[:1])
		if detail != "Refused. 1. design.colors.ink: ink on ground has a contrast of 1.05:1" {
			t.Errorf("one failure: %q", detail)
		}
		if detail, listed = refusalEnvelope("Refused.", nil); detail != "Refused." || len(listed) != 0 {
			t.Errorf("no failure: %q (%d listed)", detail, len(listed))
		}
	})

	t.Run("a failure with no path is named by its key, one with a ref by the ref", func(t *testing.T) {
		detail, _ := refusalEnvelope("Refused.", []refusalFailure{
			rfail("", "design", "missing"),
			{Ref: "n3", Key: "text", Reason: "too long"},
			{Ref: "n4", Path: "plans.1", Key: "price", Reason: "not text"},
		})
		want := "Refused. 1. design: missing; 2. ref n3.text: too long; 3. ref n4 plans.1.price: not text"
		if detail != want {
			t.Errorf("detail = %q, want %q", detail, want)
		}
	})

	t.Run("twenty failures are kept, and a cut list says so with a plus", func(t *testing.T) {
		many := make([]refusalFailure, 25)
		for i := range many {
			many[i] = rfail("outline", "anchor", "bad")
		}
		detail, listed := refusalEnvelope("Refused.", many)
		if len(listed) != 20 {
			t.Errorf("failures = %d, want 20", len(listed))
		}
		if !strings.HasSuffix(detail, " (17+ more in failures)") {
			t.Errorf("detail ends %q", tail(detail, 40))
		}
		detail, listed = refusalEnvelope("Refused.", many[:20])
		if len(listed) != 20 || !strings.HasSuffix(detail, " (17 more in failures)") {
			t.Errorf("twenty failures: %d kept, detail ends %q", len(listed), tail(detail, 40))
		}
	})

	t.Run("six allowed values are spelled out and the rest elided; twelve are kept", func(t *testing.T) {
		values := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n"}
		detail, listed := refusalEnvelope("Refused.", []refusalFailure{rfail("p", "k", "bad", values...)})
		if !strings.HasSuffix(detail, "1. p.k: bad (allowed: a, b, c, d, e, f, ...)") {
			t.Errorf("detail = %q", detail)
		}
		if len(listed[0].Allowed) != 12 || listed[0].Allowed[11] != "l" {
			t.Errorf("allowed kept = %v", listed[0].Allowed)
		}
		detail, _ = refusalEnvelope("Refused.", []refusalFailure{rfail("p", "k", "bad", values[:6]...)})
		if !strings.HasSuffix(detail, "(allowed: a, b, c, d, e, f)") {
			t.Errorf("six values: %q", detail)
		}
	})

	t.Run("the message holds at most 1200 characters and ends in three dots when cut", func(t *testing.T) {
		long := strings.Repeat("word ", 400)
		detail, _ := refusalEnvelope(long, five)
		if n := utf8.RuneCountInString(detail); n > 1200 || !strings.HasSuffix(detail, "...") {
			t.Errorf("a long summary: %d characters, ends %q", n, tail(detail, 10))
		}
		// Three failures that fill the room still end inside the limit.
		fat := strings.Repeat("x", 190)
		var fails []refusalFailure
		for i := 0; i < 3; i++ {
			fails = append(fails, rfail(strings.Repeat("p", 110), strings.Repeat("k", 50), fat, strings.Repeat("v", 40), strings.Repeat("w", 40), strings.Repeat("y", 40), strings.Repeat("z", 40), "e", "f", "g"))
		}
		detail, _ = refusalEnvelope(strings.Repeat("s", 300), fails)
		if n := utf8.RuneCountInString(detail); n != 1200 || !strings.HasSuffix(detail, "...") {
			t.Errorf("full message: %d characters, ends %q", n, tail(detail, 10))
		}
		// A message under the limit is not touched.
		short, _ := refusalEnvelope("Refused.", five[:1])
		if strings.HasSuffix(short, "...") {
			t.Errorf("short message was cut: %q", short)
		}
	})

	t.Run("each field is one line and bounded", func(t *testing.T) {
		detail, listed := refusalEnvelope("Two\nlines\tand   gaps.", []refusalFailure{
			rfail(strings.Repeat("p", 130), strings.Repeat("k", 70), "line one\nline two\r\n\tline three  "+strings.Repeat("r", 300), strings.Repeat("a", 60), "", "b\nc"),
		})
		if strings.ContainsAny(detail, "\n\r\t") || strings.Contains(detail, "  ") {
			t.Errorf("the message is not one line: %q", detail)
		}
		f := listed[0]
		if n := utf8.RuneCountInString(f.Path); n != 120 || !strings.HasSuffix(f.Path, "...") {
			t.Errorf("path: %d characters", n)
		}
		if n := utf8.RuneCountInString(f.Key); n != 60 || !strings.HasSuffix(f.Key, "...") {
			t.Errorf("key: %d characters", n)
		}
		if n := utf8.RuneCountInString(f.Reason); n != 200 || !strings.HasPrefix(f.Reason, "line one line two line three ") {
			t.Errorf("reason: %d characters, %q", n, f.Reason)
		}
		if len(f.Allowed) != 2 || utf8.RuneCountInString(f.Allowed[0]) != 40 || f.Allowed[1] != "b c" {
			t.Errorf("allowed: %q (an empty value is dropped, a long one cut to 40)", f.Allowed)
		}
	})

	t.Run("a failure that lacks a key or a reason is not listed", func(t *testing.T) {
		detail, listed := refusalEnvelope("Refused.", []refusalFailure{
			{Path: "p", Reason: "no key"}, {Path: "p", Key: "k"}, {Path: "p", Key: " ", Reason: "\n"},
			rfail("p", "k", "kept"),
		})
		if len(listed) != 1 || detail != "Refused. 1. p.k: kept" {
			t.Errorf("listed %d: %q", len(listed), detail)
		}
	})

	t.Run("failures are written as the spec has them", func(t *testing.T) {
		_, listed := refusalEnvelope("Refused.", []refusalFailure{rfail("design.colors", "ink", "too close", "a"), rfail("", "design", "missing")})
		b, err := json.Marshal(listed)
		if err != nil {
			t.Fatal(err)
		}
		want := `[{"path":"design.colors","key":"ink","reason":"too close","allowed":["a"]},{"key":"design","reason":"missing"}]`
		if string(b) != want {
			t.Errorf("failures = %s, want %s", b, want)
		}
	})
}

// The failures of a design reach the envelope as they were found, in the
// order they were found, and the code of an input refused over its design
// carries them.
func TestPageCreateInputRefusalCarriesDesignFailures(t *testing.T) {
	designWith := func(set string) []byte { return v3HeadInput(map[string]string{"design": set}) }
	incomplete := designWith(`{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},` +
		`"fonts":{"heading":"Manrope"},"scale":{"base_px":17,"ratio":1.25},"radius":"md"}`)
	closeInk := designWith(`{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#F0EFEA","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},` +
		`"fonts":{"heading":"Manrope","body":"Source Sans 3"},"scale":{"base_px":17,"ratio":1.25},"radius":"md","density":"normal"}`)
	pureBlack := designWith(`{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#000000","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},` +
		`"fonts":{"heading":"Manrope","body":"Source Sans 3"},"scale":{"base_px":17,"ratio":1.25},"radius":"md","density":"normal"}`)

	for _, c := range []struct {
		name  string
		input []byte
		code  string
		key   string
	}{
		{"a field left out", incomplete, pageDesignNotReady, "body"},
		{"two colours too close", closeInk, pageDesignContrastLow, "ink"},
		{"a value the design may not hold", pureBlack, pageCreateBadInput, "ink"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, refusal := checkPageCreateInput(c.input)
			if refusal.Code != c.code {
				t.Fatalf("code %q, want %q", refusal.Code, c.code)
			}
			if len(refusal.Failures) == 0 {
				t.Fatal("no failures carried")
			}
			var found bool
			for _, f := range refusal.Failures {
				if f.Key == c.key && strings.HasPrefix(f.Path, "design") && f.Reason != "" {
					found = true
				}
			}
			if !found {
				t.Errorf("no failure for %s: %+v", c.key, refusal.Failures)
			}
			if _, code := validatePageCreateInput(c.input); code != c.code {
				t.Errorf("validatePageCreateInput answers %q, want %q", code, c.code)
			}
		})
	}

	t.Run("a refusal of the outline names a code only", func(t *testing.T) {
		_, refusal := checkPageCreateInput(v3Anchors("one", "one"))
		if refusal.Code != pageCreateAnchorDuplicate || len(refusal.Failures) != 0 {
			t.Errorf("refusal = %+v", refusal)
		}
		f, refusal := checkPageCreateInput(v3HeadInput(nil))
		if refusal.Code != "" || len(refusal.Failures) != 0 || f.design == nil {
			t.Errorf("an accepted input: %+v, %+v", refusal, f)
		}
	})
}

// Through the run path: nothing reaches the site, and the AI is told the
// failing field in the message and in failures[].
func TestPageCreateRun_DesignRefusalNamesTheFailingPair(t *testing.T) {
	// The run path takes integers only, so the scale's ratio is written as 1
	// here: the design refuses it after the failure each case is about, which
	// stays the first.
	closeInk := string(v3HeadInput(map[string]string{"design": `{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#F0EFEA","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},` +
		`"fonts":{"heading":"Manrope","body":"Source Sans 3"},"scale":{"base_px":17,"ratio":1},"radius":"md","density":"normal"}`}))
	incomplete := string(v3HeadInput(map[string]string{"design": `{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},` +
		`"fonts":{"heading":"Manrope"},"scale":{"base_px":17,"ratio":1},"radius":"md"}`}))

	for _, c := range []struct{ name, input, code, inline string }{
		{"contrast", closeInk, pageDesignContrastLow, "1. design.colors.ink: ink on ground has a contrast of "},
		{"not ready", incomplete, pageDesignNotReady, "1. design.fonts.body: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, agent, site := pageCreateRunRouter(t, agentcmd.MinAgentVersionForPageLayout, "create_content_invalid")
			ref := runPageCreate(t, r, site, c.input)
			if ref.code != codeInvalidToolArguments || ref.data["code"] != c.code {
				t.Fatalf("refusal = %s", ref.raw)
			}
			if len(agent.calls) != 0 {
				t.Errorf("the site was asked %d times", len(agent.calls))
			}
			if ref.data["hint"] != precheckRefusalHints[c.code] || precheckRefusalHints[c.code] == "" {
				t.Errorf("hint = %v", ref.data["hint"])
			}
			if !strings.Contains(ref.msg, c.inline) {
				t.Errorf("the message does not spell out the failure %q: %s", c.inline, ref.msg)
			}
			if utf8.RuneCountInString(ref.msg) > 1200 {
				t.Errorf("message of %d characters", utf8.RuneCountInString(ref.msg))
			}
			failures, _ := ref.data["failures"].([]any)
			if len(failures) == 0 {
				t.Fatalf("no failures[] in %s", ref.raw)
			}
			first, _ := failures[0].(map[string]any)
			if first["path"] == nil || first["key"] == nil || first["reason"] == nil {
				t.Errorf("first failure = %v", first)
			}
			if _, known := agentcmd.AbilityRunRefusalCodes[c.code]; !known {
				t.Errorf("%s is not in agentcmd.AbilityRunRefusalCodes", c.code)
			}
		})
	}
}
