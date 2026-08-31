package audit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateTargetID_BoundsAndReportsHonestly pins the three properties the
// refusal-audit callers depend on.
//
// The bound exists because audit_log appends are serialised per tenant behind
// pg_advisory_xact_lock, so any caller who can choose target_id bytes chooses
// how much data goes through that lock. The properties below are what make the
// bound safe to apply to attacker-chosen input without either losing the
// evidence or corrupting the row.
func TestTruncateTargetID_BoundsAndReportsHonestly(t *testing.T) {
	t.Run("a value at or under the bound is returned untouched", func(t *testing.T) {
		for _, in := range []string{
			"",
			"list_sites",
			strings.Repeat("a", MaxTargetIDLen-1),
			strings.Repeat("a", MaxTargetIDLen), // exactly at the bound
		} {
			out, truncated, origLen := TruncateTargetID(in)
			if truncated {
				t.Errorf("len %d: reported truncated, want untouched", len(in))
			}
			if out != in {
				t.Errorf("len %d: value was rewritten (%d bytes out), want identical", len(in), len(out))
			}
			if origLen != len(in) {
				t.Errorf("len %d: originalLen = %d, want %d", len(in), origLen, len(in))
			}
		}
	})

	t.Run("an oversized value is bounded and says so, by value", func(t *testing.T) {
		in := strings.Repeat("a", 256*1024) // the full body limit
		out, truncated, origLen := TruncateTargetID(in)

		if !truncated {
			t.Fatal("a 256 KiB target was NOT reported as truncated; the row would claim this is the whole value")
		}
		if len(out) > MaxTargetIDLen {
			t.Errorf("bounded value is %d bytes, want <= %d — the bound did not bind", len(out), MaxTargetIDLen)
		}
		if origLen != len(in) {
			t.Errorf("originalLen = %d, want %d — the row cannot say how much was actually sent", origLen, len(in))
		}
		if !strings.HasPrefix(in, out) {
			t.Error("the bounded value is not a prefix of the input; the evidence is not merely shortened")
		}
	})

	// THE ONE THAT WOULD HAVE BEEN A 500. Postgres rejects an invalid byte
	// sequence for encoding "UTF8", so a byte-slice that splits a multi-byte
	// rune turns a bounded write into a failed one -- on the refusal path,
	// where the whole point is that the row gets written.
	t.Run("truncation never splits a rune", func(t *testing.T) {
		// Multi-byte runes chosen so the naive byte bound lands mid-rune.
		for _, r := range []string{"é", "€", "😀"} {
			in := strings.Repeat(r, 1024)
			out, truncated, _ := TruncateTargetID(in)

			if !truncated {
				t.Fatalf("%q: input of %d bytes was not truncated", r, len(in))
			}
			if !utf8.ValidString(out) {
				t.Errorf("%q: bounded value is not valid UTF-8 — Postgres would reject this row", r)
			}
			if len(out) > MaxTargetIDLen {
				t.Errorf("%q: bounded value is %d bytes, want <= %d", r, len(out), MaxTargetIDLen)
			}
			if !strings.HasPrefix(in, out) {
				t.Errorf("%q: bounded value is not a prefix of the input", r)
			}
		}
	})

	// A genuine U+FFFD in the input is a real rune and must survive; only an
	// INCOMPLETE trailing sequence is trimmed. This is what stops the
	// rune-boundary loop from eating valid content.
	t.Run("a real replacement char is not mistaken for a partial rune", func(t *testing.T) {
		in := strings.Repeat("�", 1024)
		out, _, _ := TruncateTargetID(in)

		if !utf8.ValidString(out) {
			t.Error("bounded value is not valid UTF-8")
		}
		if len(out) < MaxTargetIDLen-3 {
			t.Errorf("bounded value is %d bytes; the loop ate valid U+FFFD runes instead of stopping", len(out))
		}
	})
}
