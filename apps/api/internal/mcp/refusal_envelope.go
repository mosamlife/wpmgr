package mcp

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

// The envelope of a refusal that teaches the fix (SP-14), as the control plane
// writes it. It is the twin of the agent's Refusal class and keeps its numbers.
//
// A refusal carries its code, a message that opens with a summary and spells
// out the first three concrete failures, so a client that shows only the
// message still gets the fix, and failures[] with up to twenty of them. A
// failure is where it is (a node ref, or the dotted address of the field as
// path and key), why, and what would be accepted when the set is closed.
//
// Every string is one line and bounded, so a refusal cannot grow with its
// input. Failures hold WPMgr's own words and the names of the fields; none
// holds text a site or a page sent.

// The numbers of the envelope: the agent's Refusal constants.
const (
	// refusalInlineFailures failures are spelled out in the message, and
	// refusalMaxFailures are kept in failures[].
	refusalInlineFailures = 3
	refusalMaxFailures    = 20
	// refusalInlineAllowed values of a closed set are spelled out in the
	// message, and refusalMaxAllowed kept in the failure.
	refusalInlineAllowed = 6
	refusalMaxAllowed    = 12
	// The characters of the message and of each field.
	refusalMaxDetailChars  = 1200
	refusalMaxReasonChars  = 200
	refusalMaxPathChars    = 120
	refusalMaxKeyChars     = 60
	refusalMaxRefChars     = 32
	refusalMaxAllowedChars = 40
)

// refusalFailure is one failure of a refusal. Ref names a node of a page that
// is being edited, and Path the address of the field that holds the problem;
// Key is the field itself. Either of Ref and Path may be empty.
type refusalFailure struct {
	// Native input failures carry the whole value path and a separate control key.
	fullPath bool
	Ref      string   `json:"ref,omitempty"`
	Path     string   `json:"path,omitempty"`
	Key      string   `json:"key"`
	Reason   string   `json:"reason"`
	Allowed  []string `json:"allowed,omitempty"`
}

// refusalEnvelope is the message of a refusal and the failures it carries.
// The message is the summary, then the first refusalInlineFailures failures
// numbered from 1 and separated by "; ", then how many more are in failures
// (with a "+" when the list itself was cut). A failure without a key or a
// reason says nothing and is left out.
func refusalEnvelope(summary string, fails []refusalFailure) (string, []refusalFailure) {
	var listed []refusalFailure
	cut := false
	for _, f := range fails {
		if len(listed) == refusalMaxFailures {
			cut = true
			break
		}
		if c, ok := cleanRefusalFailure(f); ok {
			listed = append(listed, c)
		}
	}

	text := refusalLine(summary, refusalMaxDetailChars)
	var parts []string
	for i, f := range listed {
		if i == refusalInlineFailures {
			break
		}
		part := strconv.Itoa(i+1) + ". " + refusalWhere(f) + ": " + f.Reason
		if len(f.Allowed) > 0 {
			shown, more := f.Allowed, ""
			if len(shown) > refusalInlineAllowed {
				shown, more = shown[:refusalInlineAllowed], ", ..."
			}
			part += " (allowed: " + strings.Join(shown, ", ") + more + ")"
		}
		parts = append(parts, part)
	}
	if len(parts) > 0 {
		if text != "" {
			text += " "
		}
		text += strings.Join(parts, "; ")
	}
	if rest := len(listed) - len(parts); rest > 0 {
		text += " (" + strconv.Itoa(rest)
		if cut {
			text += "+"
		}
		text += " more in failures)"
	}
	return refusalLine(text, refusalMaxDetailChars), listed
}

// refusalWhere is where a failure is: a node ref, or the dotted address of
// the field.
func refusalWhere(f refusalFailure) string {
	if f.fullPath {
		return f.Path
	}
	at := f.Key
	if f.Path != "" {
		at = f.Path + "." + f.Key
	}
	switch {
	case f.Ref == "":
		return at
	case f.Path == "":
		return "ref " + f.Ref + "." + f.Key
	}
	return "ref " + f.Ref + " " + at
}

// cleanRefusalFailure is a failure with every field on one line and within
// its bound, and the empty allowed values dropped.
func cleanRefusalFailure(f refusalFailure) (refusalFailure, bool) {
	keyChars := refusalMaxKeyChars
	if f.fullPath {
		keyChars = 64
	}
	out := refusalFailure{
		fullPath: f.fullPath,
		Ref:      refusalLine(f.Ref, refusalMaxRefChars),
		Path:     refusalLine(f.Path, refusalMaxPathChars),
		Key:      refusalLine(f.Key, keyChars),
		Reason:   refusalLine(f.Reason, refusalMaxReasonChars),
	}
	if out.Key == "" || out.Reason == "" {
		return refusalFailure{}, false
	}
	for _, v := range f.Allowed {
		if len(out.Allowed) == refusalMaxAllowed {
			break
		}
		if v = refusalLine(v, refusalMaxAllowedChars); v != "" {
			out.Allowed = append(out.Allowed, v)
		}
	}
	return out, true
}

// refusalLine is s on one line: control characters and runs of white space
// become one space, and a text longer than max characters is cut to end in
// "..." at max.
func refusalLine(s string, max int) string {
	s = humantext.Clean(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return strings.TrimRight(string([]rune(s)[:max-3]), " ") + "..."
}

// refusalFailuresOfDesign is the failures of a design in the envelope's form,
// in the order they were found.
func refusalFailuresOfDesign(fails []designFailure) []refusalFailure {
	out := make([]refusalFailure, len(fails))
	for i, f := range fails {
		out[i] = refusalFailure{Path: f.Path, Key: f.Key, Reason: f.Reason, Allowed: f.Allowed}
	}
	return out
}
