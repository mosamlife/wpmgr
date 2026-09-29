package audit

// TargetTypeAssistantCachePurgeRequest is the TargetType of every
// assistant.request.* row. TargetID is the request id.
const TargetTypeAssistantCachePurgeRequest = "assistant_cache_purge_request"

// SiteOriginMetadataKeys names the metadata keys, on the assistant request
// rows and on site.cache.purged when an AI request caused it, whose values
// came from a site or from an AI connection rather than from WPMgr: a site's
// name and host, a connection's name, the page address an AI chose, and text a
// site reported.
//
// Each value is stored already cleaned and capped, but it is still text a
// third party chose. Two rules follow for any reader of this log:
//
//   - anything that hands audit metadata to an AI model must fence every one
//     of these values as site-supplied text, never as an instruction;
//   - anything that renders them for a person must render them as plain text,
//     never as markup, a link target or a tooltip.
//
// The set is returned as a fresh map, so a caller cannot change it for anyone
// else.
func SiteOriginMetadataKeys() map[string]struct{} {
	return map[string]struct{}{
		"grant_label":        {},
		"site_label":         {},
		"site_host":          {},
		"url":                {},
		"site_reported_text": {},
	}
}

// IsSiteOriginMetadataKey reports whether key is in SiteOriginMetadataKeys.
func IsSiteOriginMetadataKey(key string) bool {
	_, ok := SiteOriginMetadataKeys()[key]
	return ok
}
