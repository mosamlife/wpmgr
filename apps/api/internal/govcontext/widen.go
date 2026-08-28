package govcontext

import (
	"fmt"
	"sort"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// checkNoWiden is ADR-064 Decision 4's write-time rejection: "A widening
// attempt on a restriction field is rejected at the write path, not silently
// dropped at read time — at every writable layer, against every layer above
// it, not only against the nearest one."
//
// It runs ONLY over RestrictionSet. There is deliberately no guidance
// equivalent — Decision 1 and the Consequences section are explicit that
// "wider" and "narrower" are not defined relations over free text, and a
// checker that always passed without ever testing anything would be worse
// than the honest absence of one. Do not add one; see model.go's GuidanceSet
// doc comment.
//
// higherLayers is every layer ABOVE the one being written, outermost first
// (e.g. for a site write: [{1, "WPMgr security policy", layer1Restrictions},
// {2, "organisation default", org.Restrictions}]). For each higher layer, and
// for each of RestrictionSet's three deny-list fields, the proposed value must
// be a SUPERSET of that layer's value for the same field — dropping an item
// the higher layer forbade is exactly the widen this function exists to catch.
// The first violation found is returned; the reason names the field, the
// missing items, and the layer that set them, per Decision 4's "the reason
// names the restriction and the layer that blocked it".
func checkNoWiden(proposed RestrictionSet, higherLayers []namedLayer) *domain.Error {
	for _, hl := range higherLayers {
		if missing := missingItems(hl.Restrictions.ForbiddenTools, proposed.ForbiddenTools); len(missing) > 0 {
			return widenError("forbidden_tools", hl, missing)
		}
		if missing := missingItems(hl.Restrictions.ForbiddenDomains, proposed.ForbiddenDomains); len(missing) > 0 {
			return widenError("forbidden_domains", hl, missing)
		}
		if missing := missingItems(hl.Restrictions.ForbiddenTopics, proposed.ForbiddenTopics); len(missing) > 0 {
			return widenError("forbidden_topics", hl, missing)
		}
	}
	return nil
}

// namedLayer pairs a restriction set with the layer identity that set it, so
// a rejection can name "the layer that blocked it" (Decision 13's error
// contract) rather than just the field.
type namedLayer struct {
	Layer        int
	Name         string
	Restrictions RestrictionSet
}

// missingItems returns the elements of required that are absent from
// proposed — the items a lower-layer write would silently drop (widen) if
// accepted as-is. Order-independent; the returned slice is sorted for a
// deterministic, testable error message.
func missingItems(required, proposed []string) []string {
	if len(required) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(proposed))
	for _, v := range proposed {
		have[v] = struct{}{}
	}
	var missing []string
	for _, r := range required {
		if _, ok := have[r]; !ok {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	return missing
}

func widenError(field string, hl namedLayer, missing []string) *domain.Error {
	return domain.Conflict("context_widen_forbidden", fmt.Sprintf(
		"this write would remove %v from %s, which was set by %s (layer %d) — a lower layer may narrow or add to a restriction but never remove what a higher layer set",
		missing, field, hl.Name, hl.Layer,
	)).WithDetails(map[string]any{
		"field":         field,
		"layer":         hl.Layer,
		"layer_name":    hl.Name,
		"removed_items": missing,
	})
}
