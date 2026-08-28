package govcontext

// applyBudget enforces ADR-064 Decision 9's byte budget across layers,
// mutating them in place, and reports whether anything was cut. It assumes
// layers is in Decision 1 order (1..6, ascending index) and that each
// layer's .Bytes field already reflects its current (pre-truncation) size.
//
// Order: "truncation starts at the lowest surviving layer and works upward:
// session context first, then skill instructions, then any facts overflow,
// then site overrides, then organisation defaults; layer 1 is never
// truncated." That is layers 6, 5, 4, 3, 2 in that exact order — layer index
// descending from the last element to the second.
//
// Granularity: "truncation happens at a field or record boundary, never
// mid-field, and is marked explicitly". dropNextField (below) always removes
// one whole field/record at a time and never rewrites a string to a shorter
// one, so no field is ever partially truncated.
func applyBudget(layers []LayerContribution, budget int) bool {
	total := 0
	for _, l := range layers {
		total += l.Bytes
	}
	if total <= budget {
		return false
	}
	truncated := false
	// layers[5]=layer6, layers[4]=layer5, layers[3]=layer4, layers[2]=layer3,
	// layers[1]=layer2. layers[0]=layer1 is never touched — the loop stops
	// before it.
	for i := len(layers) - 1; i >= 1 && total > budget; i-- {
		for total > budget {
			before := layers[i].Bytes
			if !dropNextField(&layers[i]) {
				break // layer is fully empty; nothing left to drop
			}
			layers[i].Bytes = layerBytes(layers[i])
			total -= before - layers[i].Bytes
			layers[i].Truncated = true
			truncated = true
		}
	}
	return truncated
}

// dropNextField removes exactly one field or record from l, in the fixed
// order least-load-bearing first, and reports whether anything was removed
// (false means the layer is already fully empty). Order within a layer:
// session text, then guidance fields (style, terminology, audience, brand
// voice — arbitrary but fixed and deterministic), then facts (one record:
// the whole observed-facts set), then restriction list ITEMS one at a time
// from the end of each list (topics, then domains, then tools). Restriction
// items are dropped last within a layer, after every guidance field and the
// facts record, on the reasoning that a named boundary is more load-bearing
// than free-text guidance or an observational fact — the ADR fixes the
// cross-layer order only; this within-layer order is this package's own,
// documented choice.
func dropNextField(l *LayerContribution) bool {
	if l.Session != "" {
		l.Session = ""
		return true
	}
	switch {
	case l.Guidance.Style != "":
		l.Guidance.Style = ""
		return true
	case l.Guidance.Terminology != "":
		l.Guidance.Terminology = ""
		return true
	case l.Guidance.Audience != "":
		l.Guidance.Audience = ""
		return true
	case l.Guidance.BrandVoice != "":
		l.Guidance.BrandVoice = ""
		return true
	}
	if l.Facts != nil && !l.Facts.IsEmpty() {
		l.Facts = &SiteFacts{}
		return true
	}
	if n := len(l.Restrictions.ForbiddenTopics); n > 0 {
		l.Restrictions.ForbiddenTopics = l.Restrictions.ForbiddenTopics[:n-1]
		return true
	}
	if n := len(l.Restrictions.ForbiddenDomains); n > 0 {
		l.Restrictions.ForbiddenDomains = l.Restrictions.ForbiddenDomains[:n-1]
		return true
	}
	if n := len(l.Restrictions.ForbiddenTools); n > 0 {
		l.Restrictions.ForbiddenTools = l.Restrictions.ForbiddenTools[:n-1]
		return true
	}
	return false
}
