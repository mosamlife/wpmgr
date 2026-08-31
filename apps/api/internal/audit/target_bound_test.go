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
			out, truncated, _, origLen := SafeTargetID(in)
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
		out, truncated, _, origLen := SafeTargetID(in)

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
			out, truncated, _, _ := SafeTargetID(in)

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

	// INVALID UTF-8 IS THE HAZARD TRUNCATION DOES NOT COVER. Postgres rejects
	// an invalid byte sequence for encoding "UTF8", so these bytes do not
	// produce a bounded row -- they produce a FAILED append on the refusal
	// path. A short invalid value never reaches the length bound at all, which
	// is why sanitising has to happen on the within-limit return too.
	t.Run("invalid UTF-8 is sanitized on the within-limit path", func(t *testing.T) {
		in := string([]byte{0xff, 0xfe, 'A'}) // 3 bytes: far under the bound
		out, truncated, sanitized, origLen := SafeTargetID(in)

		if truncated {
			t.Error("a 3-byte value was reported truncated")
		}
		if !sanitized {
			t.Fatal("invalid UTF-8 was NOT reported sanitized; Postgres would reject this row " +
				"and the refusal would fail to record")
		}
		if !utf8.ValidString(out) {
			t.Errorf("output %v is still invalid UTF-8", []byte(out))
		}
		if origLen != len(in) {
			t.Errorf("originalLen = %d, want %d", origLen, len(in))
		}
	})

	t.Run("invalid UTF-8 is sanitized on the truncated path too", func(t *testing.T) {
		// Each bad byte is separated by a valid one so it forms its own
		// invalid RUN: strings.ToValidUTF8 collapses a consecutive run to a
		// single replacement, so 4096 adjacent 0xff bytes would become three
		// bytes and never reach the bound.
		in := strings.Repeat(string([]byte{0xff, 'A'}), 2048)
		out, truncated, sanitized, _ := SafeTargetID(in)

		if !truncated || !sanitized {
			t.Fatalf("truncated=%v sanitized=%v, want both true", truncated, sanitized)
		}
		if !utf8.ValidString(out) {
			t.Error("output is still invalid UTF-8 after truncation")
		}
		// Sanitising expands each bad byte to a 3-byte U+FFFD, so the bound
		// must be applied AFTER sanitising or the result exceeds it.
		if len(out) > MaxTargetIDLen {
			t.Errorf("output is %d bytes, want <= %d — the bound was applied before sanitising",
				len(out), MaxTargetIDLen)
		}
	})

	t.Run("a valid value is never reported sanitized", func(t *testing.T) {
		for _, in := range []string{"list_sites", "2025-11-25", "naïve", "😀"} {
			out, _, sanitized, _ := SafeTargetID(in)
			if sanitized {
				t.Errorf("%q: reported sanitized, but it is valid UTF-8", in)
			}
			if out != in {
				t.Errorf("%q: rewritten to %q", in, out)
			}
		}
	})

	// A genuine U+FFFD in the input is a real rune and must survive; only an
	// INCOMPLETE trailing sequence is trimmed. This is what stops the
	// rune-boundary loop from eating valid content.
	t.Run("a real replacement char is not mistaken for a partial rune", func(t *testing.T) {
		in := strings.Repeat("�", 1024)
		out, _, _, _ := SafeTargetID(in)

		if !utf8.ValidString(out) {
			t.Error("bounded value is not valid UTF-8")
		}
		if len(out) < MaxTargetIDLen-3 {
			t.Errorf("bounded value is %d bytes; the loop ate valid U+FFFD runes instead of stopping", len(out))
		}
	})
}
