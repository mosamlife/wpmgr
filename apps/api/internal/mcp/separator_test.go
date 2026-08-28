package mcp

import "testing"

// ---------------------------------------------------------------------------
// SEPARATORS. RFC 6749 section 3.3 defines the scope parameter as an
// ASCII-SPACE-delimited list:
//
//	scope = scope-token *( SP scope-token )
//
// SP is U+0020 and nothing else. `strings.Fields` splits on unicode.IsSpace,
// which ALSO treats TAB, LF, CR, VT, FF, NBSP (U+00A0), LINE SEPARATOR
// (U+2028), PARAGRAPH SEPARATOR (U+2029), EN QUAD (U+2000) and IDEOGRAPHIC
// SPACE (U+3000) as delimiters -- so malformed input was being NORMALISED INTO
// WELL-FORMED input before the gate ever saw it.
//
// That is this package's own thesis violated one layer earlier: the absence of
// a valid separator quietly becoming a valid separator. The gate refuses an
// unrecognised scope; it never got the chance, because the parser had already
// repaired the request into one it recognised.
//
// The fix needs no new refusal rule. A non-space whitespace character is not a
// separator, so it stays INSIDE the token: "mcp:read\tmcp:read" is one token,
// that token is not in the registry, and the existing closed-registry rule
// refuses the whole request.
// ---------------------------------------------------------------------------

func TestParseRequestedScopes_OnlyASCIISpaceSeparates(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"horizontal tab", "mcp:read\tmcp:read"},
		{"line feed", "mcp:read\nmcp:read"},
		{"carriage return", "mcp:read\rmcp:read"},
		{"vertical tab", "mcp:read\vmcp:read"},
		{"form feed", "mcp:read\fmcp:read"},
		{"no-break space U+00A0", "mcp:read mcp:read"},
		{"line separator U+2028", "mcp:read mcp:read"},
		{"paragraph separator U+2029", "mcp:read mcp:read"},
		{"en quad U+2000", "mcp:read mcp:read"},
		{"ideographic space U+3000", "mcp:read　mcp:read"},
		{"trailing tab on one valid scope", "mcp:read\t"},
		{"leading tab on one valid scope", "\tmcp:read"},
		{"CRLF between scopes", "mcp:read\r\nmcp:read"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRequestedScopes(tc.raw)
			if err == nil {
				t.Fatalf("ParseRequestedScopes(%q) accepted a non-ASCII-space separator "+
					"and returned %v; RFC 6749 section 3.3 delimits on U+0020 only, so this "+
					"input is malformed and must be REFUSED, never normalised into "+
					"something valid", tc.raw, got)
			}
			if len(got) != 0 {
				t.Fatalf("ParseRequestedScopes(%q) refused but still returned %v", tc.raw, got)
			}
		})
	}
}

// The separator rule must not over-fire. ASCII space in every reasonable
// arrangement still parses, including repeated, leading and trailing spaces.
//
// Tolerating EMPTY SEGMENTS between spaces coerces nothing: an empty segment
// names no scope, so skipping it cannot manufacture authority. That is the
// difference from treating a tab as a delimiter, which invents a token
// boundary the client never wrote and can turn one unrecognised token into two
// recognised ones.
func TestParseRequestedScopes_ASCIISpaceArrangementsStillParse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"single token", "mcp:read"},
		{"leading spaces", "   mcp:read"},
		{"trailing spaces", "mcp:read   "},
		{"both", "  mcp:read  "},
		{"repeated internal spaces", "mcp:read   mcp:read"},
		{"duplicate separated by one space", "mcp:read mcp:read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRequestedScopes(tc.raw)
			if err != nil {
				t.Fatalf("ParseRequestedScopes(%q) refused a well-formed request: %v", tc.raw, err)
			}
			if len(got) != 1 || got[0] != ScopeRead {
				t.Fatalf("ParseRequestedScopes(%q) = %v, want exactly [%s]", tc.raw, got, ScopeRead)
			}
		})
	}
}
