package mcp

import (
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Protocol revision negotiation (design §6, "Three cases the code must not
// conflate")
// ---------------------------------------------------------------------------

// ProtocolTarget is the revision this server prefers and announces.
const ProtocolTarget = "2025-11-25"

// ProtocolFloor is the compatibility floor. NOTHING below it is ever accepted,
// and 2024-11-05 is below it: every revision under 2025-03-26 drops the fields
// the approval flow needs. The floor is a property of the approval flow, not a
// compatibility preference, and it moves only when that flow does.
const ProtocolFloor = "2025-03-26"

// ProtocolHeader is the revision header clients send on requests AFTER
// initialization. Its ABSENCE is a supported case, not an error -- see
// NegotiateProtocol.
const ProtocolHeader = "MCP-Protocol-Version"

// supportedRevisions is the negotiation window, NEWEST FIRST. Membership is
// the whole test: a revision that is not in this list is refused even when it
// parses and even when it is newer than the target. "Probably newer, probably
// fine" is exactly the silent-downgrade this list exists to prevent.
var supportedRevisions = []string{
	"2025-11-25",
	"2025-06-18",
	ProtocolFloor,
}

// SupportedRevisions returns the negotiation window, newest first.
func SupportedRevisions() []string {
	out := make([]string, len(supportedRevisions))
	copy(out, supportedRevisions)
	return out
}

// NegotiationOutcome is which of the THREE distinct cases a revision string
// fell into. They have three different correct answers and collapsing any two
// of them is a defect, so the classifier returns the case rather than a bare
// (string, error) that would flatten two of them into one.
type NegotiationOutcome int

const (
	// NegotiationAssumedFloor: the header was ABSENT. The specification says a
	// server that receives no version header should assume 2025-03-26. This is
	// not leniency and it is not a favour to old clients: returning 400 here
	// rejects conforming clients, and since no surveyed client documents this
	// header at all, header-less is the case to EXPECT rather than the
	// exception. Version is the floor.
	NegotiationAssumedFloor NegotiationOutcome = iota

	// NegotiationAccepted: present, parseable, and in the supported window.
	// Version is the client's own revision.
	NegotiationAccepted

	// NegotiationBelowFloor: present, parseable, and older than the floor.
	// Refused with both numbers named. NEVER downgraded to, never quietly
	// served a reduced surface.
	NegotiationBelowFloor

	// NegotiationUnsupported: present but unparseable, or parseable and at or
	// above the floor yet not a revision this server speaks (an unknown future
	// revision included). The specification requires 400 here.
	NegotiationUnsupported
)

// revisionForm is the MCP revision shape. It is checked BEFORE time.Parse so
// that a value which is merely date-ish ("2025-3-26") is classified
// unparseable rather than being coerced into a real date and then compared.
var revisionForm = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Negotiation is the full result of classifying one revision string.
type Negotiation struct {
	// Outcome is which of the three cases this was.
	Outcome NegotiationOutcome

	// Version is the revision to speak. It is set ONLY for
	// NegotiationAssumedFloor and NegotiationAccepted; on either refusal it is
	// empty, so a caller that ignores Outcome and reads Version gets a value
	// that cannot be mistaken for a supported revision.
	Version string

	// Raw is exactly what the client sent, verbatim and untrimmed of meaning:
	// empty string means the header was absent. This is the value recorded
	// against the grant, where absence must stay distinguishable from a value.
	Raw string
}

// Present reports whether the client actually sent a revision. It is the
// distinction Decision 10 persists: "has never connected" is a NULL
// client_identity_recorded_at, while "connected and sent no header" is a
// stamped recorded_at with a NULL protocol_version. Collapsing the two hides a
// compatibility signal an operator needs.
func (n Negotiation) Present() bool { return n.Raw != "" }

// Refused reports whether this negotiation must be answered with a refusal
// rather than served.
func (n Negotiation) Refused() bool {
	return n.Outcome == NegotiationBelowFloor || n.Outcome == NegotiationUnsupported
}

// NegotiateProtocol classifies one client-supplied revision string into
// exactly one of the three cases.
//
// The ORDER of the branches is load-bearing. Absence is tested first and on
// its own; then parseability; then the floor; then window membership. Testing
// window membership first would fold "below the floor" into "unsupported" and
// lose the refusal message that names both numbers, and testing the floor
// before parseability would let a malformed string be string-compared against
// the floor and pass.
func NegotiateProtocol(raw string) Negotiation {
	v := strings.TrimSpace(raw)

	// CASE 1 -- absent. Assume the floor. Do NOT 400.
	if v == "" {
		return Negotiation{Outcome: NegotiationAssumedFloor, Version: ProtocolFloor, Raw: ""}
	}

	// Unparseable is refused before any comparison is attempted, so that no
	// malformed value ever reaches a lexicographic compare against the floor.
	if !revisionForm.MatchString(v) {
		return Negotiation{Outcome: NegotiationUnsupported, Raw: v}
	}
	parsed, err := time.Parse("2006-01-02", v)
	if err != nil {
		return Negotiation{Outcome: NegotiationUnsupported, Raw: v}
	}

	// CASE 2 -- parseable and below the floor. Refused, with both numbers
	// named by the caller. The compare is on parsed dates and not on strings:
	// the strings happen to sort correctly today, but that is a property of
	// this particular set of literals and not of the format.
	floor, err := time.Parse("2006-01-02", ProtocolFloor)
	if err != nil {
		// Unreachable while ProtocolFloor is a literal constant of the right
		// shape, and refusing is the only safe direction if it ever is not:
		// a floor we cannot parse is a floor we cannot enforce.
		return Negotiation{Outcome: NegotiationUnsupported, Raw: v}
	}
	if parsed.Before(floor) {
		return Negotiation{Outcome: NegotiationBelowFloor, Raw: v}
	}

	// CASE 3a -- at or above the floor AND in the window. Accepted, and we
	// speak the client's revision rather than our target.
	for _, s := range supportedRevisions {
		if v == s {
			return Negotiation{Outcome: NegotiationAccepted, Version: v, Raw: v}
		}
	}

	// CASE 3b -- at or above the floor but NOT a revision we speak. An unknown
	// future revision lands here and is refused rather than being assumed
	// compatible. Note it does NOT fall back to the target: answering a
	// revision we do not speak with a surface built for a different one is the
	// silent downgrade in the other direction.
	return Negotiation{Outcome: NegotiationUnsupported, Raw: v}
}
