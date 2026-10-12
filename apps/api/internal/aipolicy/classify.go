package aipolicy

// Undo is what the checked precheck said about Undo for this request.
//
// The zero value is UndoUnknown, which moves the request onto the Ask list:
// a caller that forgets to say what it knows gets a request that waits.
type Undo uint8

// The Undo facts.
const (
	// UndoUnknown: nothing recorded. The request waits.
	UndoUnknown Undo = iota
	// UndoExact: the precheck found Undo would put things back exactly.
	UndoExact
	// UndoNotExact: the precheck found Undo would not be exact. The request
	// is on the Ask list whatever its stored class.
	UndoNotExact
	// UndoByAbility: no precheck reports exactness for this ability; its own
	// Undo applies (page creation trashes the draft it made).
	UndoByAbility
)

// ClassFacts are the stored facts one request is classed from. Every field
// comes from the control plane's own records.
type ClassFacts struct {
	// Stored is the catalogue entry's class, or for a REST write the route
	// row's class.
	Stored Class
	// Undo is the checked precheck's Undo fact.
	Undo Undo
	// TargetStatus is the raw post status the precheck checked, as recorded
	// on the request at creation (checked_target_status). Nil when the
	// ability has no target. It is compared exactly: "Draft" and "draft "
	// are not "draft".
	TargetStatus *string
	// AIDraft is true when this site holds a done, not-undone page creation
	// whose created post is the target.
	AIDraft bool
}

// Classification is the effective class of one request, or why it has none.
type Classification struct {
	// Class is the effective class. Empty when Ask is set to
	// AskUnknownTargetState.
	Class Class
	// Ask is set when classing alone puts the request in front of a person:
	// AskKindAlwaysAsks or AskUnknownTargetState.
	Ask AskReason
}

// Classify derives the effective class (§2.3). The rules, in order:
//
//  1. A stored always_ask is final.
//  2. A precheck that found Undo would not be exact (or said nothing) gives
//     always_ask, whatever the stored class. This can only move a request
//     onto the Ask list.
//  3. A stored by_target_status takes its class from the checked status.
//  4. Any other stored class is used as is; the target's status never
//     changes it.
//  5. Anything unknown asks.
//
// No rule compares two classes by strictness, so a checked fact can never
// lower a stored class.
func Classify(f ClassFacts) Classification {
	// 5 for the stored value: an unclassed or unknown entry is on the Ask
	// list, as the catalogue column's default says.
	if !f.Stored.Stored() || f.Stored == ClassAlwaysAsk {
		return Classification{Class: ClassAlwaysAsk, Ask: AskKindAlwaysAsks}
	}
	switch f.Undo {
	case UndoExact, UndoByAbility:
	default:
		return Classification{Class: ClassAlwaysAsk, Ask: AskKindAlwaysAsks}
	}
	if f.Stored != StoredByTargetStatus {
		return Classification{Class: f.Stored}
	}
	if f.TargetStatus == nil {
		return Classification{Ask: AskUnknownTargetState}
	}
	switch *f.TargetStatus {
	case "publish", "private":
		return Classification{Class: ClassLive}
	case "future":
		return Classification{Class: ClassPublish}
	case "pending":
		return Classification{Class: ClassUnpublished}
	case "draft":
		if f.AIDraft {
			return Classification{Class: ClassAIDraft}
		}
		return Classification{Class: ClassUnpublished}
	}
	return Classification{Ask: AskUnknownTargetState}
}
