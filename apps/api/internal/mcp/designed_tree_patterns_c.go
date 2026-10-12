package mcp

// The testimonials and faq patterns of a designed page, compiled as the
// agent's ElementorPatternCompiler compiles them (testimonials, faq, and the
// testimonial and tab it builds from an item). Every table, order of keys and
// order of node ids below does what that class does, and the golden cases of
// elementor-designed-containers.json hold the bytes.
//
//	testimonials featured  a group of the design's row gap: the first
//	                       testimonial widget across the full width, then the
//	                       others in rows of two equal columns, one widget in
//	                       each; a row left with one column is centred
//	faq accordion          a group holding one accordion widget with FAQ schema
//	                       on, a tab for each question with a repeater id of its
//	                       own
//
// A pattern's container carries its Navigator label after its last key, and
// draws its id before its children's. The pattern compilers register
// themselves in designedPatternBuilders, one file per area.

import (
	"encoding/json"
	"slices"
	"strconv"
)

const (
	// designedLabelPrefix opens the Navigator label of every container the
	// builder names.
	designedLabelPrefix = "WPMgr · "

	// designedTestimonialsPerRow is the testimonials, after the featured one,
	// that stand in one row.
	designedTestimonialsPerRow = 2

	// elementorItemIDDomain and elementorItemIDLength shape the _id of a
	// repeater item: the first 7 hex characters of the sha256 of the JSON list
	// of this label, the request id, the node path, the control, the index and
	// a counter.
	elementorItemIDDomain = "wpmgr.builder.item.v1"
	elementorItemIDLength = 7
)

// designedPatternNames is the Navigator name of each pattern, as written after
// the label prefix.
var designedPatternNames = map[string]string{
	"hero": "Hero", "cta": "CTA", "features": "Features", "testimonials": "Testimonials",
	"pricing": "Pricing", "faq": "FAQ", "logos": "Logos", "stats": "Stats",
}

func init() {
	designedPatternBuilders["testimonials"] = designedTestimonials
	designedPatternBuilders["faq"] = designedFAQ
}

// item issues the _id of item index of the repeater control on the node at
// path. It draws from the same set of taken ids as next, so no id is issued
// twice, and the counter goes up while the id is taken.
func (s *elementorIDs) item(path, control string, index int) (string, bool) {
	dom, ok1 := phpJSONString(elementorItemIDDomain)
	req, ok2 := phpJSONString(s.requestID)
	at, ok3 := phpJSONString(path)
	ctl, ok4 := phpJSONString(control)
	if !ok1 || !ok2 || !ok3 || !ok4 || s.requestID == "" || control == "" || index < 0 {
		return "", false
	}
	head := "[" + dom + "," + req + "," + at + "," + ctl + "," + strconv.Itoa(index) + ","
	for counter := 0; counter < elementorIDMaxAttempts; counter++ {
		id := sha256Hex([]byte(head + strconv.Itoa(counter) + "]"))[:elementorItemIDLength]
		if !s.taken[id] {
			s.taken[id] = true
			return id, true
		}
	}
	return "", false
}

// designedPatternLabel is the Navigator label of a pattern's container: the
// label prefix, the pattern's name and its variant. ok is false for a pattern
// or a variant the grammar does not have.
func designedPatternLabel(pattern, variant string) (string, bool) {
	name, known := designedPatternNames[pattern]
	spec, hasSpec := patternSpecs[pattern]
	if !known || !hasSpec || !slices.Contains(spec.variants, variant) {
		return "", false
	}
	return designedLabelPrefix + name + " " + variant, true
}

// designedPatternVariant is the variant of a pattern node of the given name:
// one of that pattern's variants of the grammar.
func designedPatternVariant(n map[string]json.RawMessage, pattern string) (string, bool) {
	spec, known := patternSpecs[pattern]
	name, _ := jsonStringOf(n["pattern"])
	variant, ok := jsonStringOf(n["variant"])
	if !known || name != pattern || !ok || !slices.Contains(spec.variants, variant) {
		return "", false
	}
	return variant, true
}

// designedPatternItems is the items of an item pattern, counted by the
// grammar's caps.
func designedPatternItems(n map[string]json.RawMessage, pattern string) ([]json.RawMessage, bool) {
	spec, known := patternSpecs[pattern]
	if !known || spec.array == "" {
		return nil, false
	}
	items, ok := jsonArrayOf(n[spec.array])
	if !ok || len(items) < spec.min || len(items) > spec.max {
		return nil, false
	}
	return items, true
}

// designedWithSetting is the container with one settings member set: replaced
// where the settings have it already, added after the last otherwise.
func designedWithSetting(container phpObject, key string, value any) phpObject {
	out := slices.Clone(container)
	for i, member := range out {
		if member.key != "settings" {
			continue
		}
		settings, _ := member.value.(phpObject)
		settings = slices.Clone(settings)
		if at := slices.IndexFunc(settings, func(p phpPair) bool { return p.key == key }); at >= 0 {
			settings[at].value = value
		} else {
			settings = append(settings, phpPair{key, value})
		}
		out[i].value = settings
	}
	return out
}

// designedWithMissingSettings is the container with the members of more that
// its settings do not have yet added after the last, as PHP's += does.
func designedWithMissingSettings(container phpObject, more phpObject) phpObject {
	out := container
	for _, p := range more {
		if has := slices.ContainsFunc(designedSettingsOf(out), func(q phpPair) bool { return q.key == p.key }); !has {
			out = designedWithSetting(out, p.key, p.value)
		}
	}
	return out
}

// designedSettingsOf is a container's settings.
func designedSettingsOf(container phpObject) phpObject {
	settings, _ := phpMemberOf(container, "settings").(phpObject)
	return settings
}

// phpMemberOf is the value of a key of a PHP object, nil when it has none.
func phpMemberOf(o phpObject, key string) any {
	if at := slices.IndexFunc(o, func(p phpPair) bool { return p.key == key }); at >= 0 {
		return o[at].value
	}
	return nil
}

// designedLabelled is the container with its Navigator label after its last
// key.
func designedLabelled(container phpObject, label string) phpObject {
	return designedWithSetting(container, "_title", label)
}

// libraryImage is an image of the media library as a media control stores it:
// the address and mime type the site resolved for the attachment id.
func (b *designedBuild) libraryImage(raw json.RawMessage) (phpObject, bool) {
	id, ok := jsonIntOf(raw)
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return nil, false
	}
	fact, known := b.media[id]
	if !known || fact.ID != id || !pageImageURLUsable(fact.URL) {
		return nil, false
	}
	if _, ok := pageImageMimes[fact.Mime]; !ok {
		return nil, false
	}
	return phpObject{{"url", fact.URL}, {"id", strconv.FormatInt(id, 10)}, {"source", "library"}}, true
}

// testimonialSettings are the settings of one testimonial widget, in the key
// order Elementor returns: the picture and the role are written only when the
// item has them. The quote is in the tone's heading colour, the name in its
// accent colour and the role in its body colour.
func (b *designedBuild) testimonialSettings(raw json.RawMessage, colours designTone) (phpObject, bool) {
	item, ok := jsonObjectOf(raw)
	if !ok {
		return nil, false
	}
	quote, ok1 := jsonStringOf(item["quote"])
	name, ok2 := jsonStringOf(item["name"])
	if !ok1 || quote == "" || !ok2 || name == "" {
		return nil, false
	}
	role, hasRole := "", false
	if rawRole, has := item["role"]; has {
		var ok bool
		if role, ok = jsonStringOf(rawRole); !ok || role == "" {
			return nil, false
		}
		hasRole = true
	}
	settings := phpObject{{"testimonial_content", elementorHTMLText.Replace(quote)}}
	if rawPicture, has := item["attachment_id"]; has {
		picture, ok := b.libraryImage(rawPicture)
		if !ok {
			return nil, false
		}
		settings = append(settings, phpPair{"testimonial_image", picture})
	}
	settings = append(settings, phpPair{"testimonial_name", elementorHTMLText.Replace(name)})
	if hasRole {
		settings = append(settings, phpPair{"testimonial_job", elementorHTMLText.Replace(role)})
	}
	return append(settings,
		phpPair{"testimonial_image_position", "aside"},
		phpPair{"testimonial_alignment", "left"},
		phpPair{"content_content_color", colours.Heading},
		phpPair{"name_text_color", colours.Accent},
		phpPair{"job_text_color", colours.Body},
	), true
}

// testimonial is one testimonial widget with the id of its path.
func (b *designedBuild) testimonial(path string, raw json.RawMessage, colours designTone) (phpObject, bool) {
	settings, ok := b.testimonialSettings(raw, colours)
	if !ok {
		return nil, false
	}
	widget, ok := b.widget(path, "testimonial", settings)
	if !ok {
		return nil, false
	}
	built, _ := widget[0].(phpObject)
	return built, true
}

// designedTestimonials compiles a testimonials pattern: the group's id, the
// featured testimonial's, then for each row its columns' and widgets' and last
// the row's own.
func designedTestimonials(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	variant, ok := designedPatternVariant(n, "testimonials")
	if !ok {
		return nil, false
	}
	label, ok := designedPatternLabel("testimonials", variant)
	if !ok {
		return nil, false
	}
	items, ok := designedPatternItems(n, "testimonials")
	if !ok {
		return nil, false
	}
	colours, err := b.sections.tokens.tone(tone)
	if err != nil {
		return nil, false
	}
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	at := func(i int) string { return path + ".testimonials[" + strconv.Itoa(i) + "]" }
	featured, ok := b.testimonial(at(0), items[0], colours)
	if !ok {
		return nil, false
	}
	parts := phpList{featured}
	width := 100 / designedTestimonialsPerRow
	for r, first := 0, 1; first < len(items); r, first = r+1, first+designedTestimonialsPerRow {
		last := min(first+designedTestimonialsPerRow, len(items))
		columns := phpList{}
		for i := first; i < last; i++ {
			columnID, ok := b.ids.next(at(i) + "#column")
			if !ok {
				return nil, false
			}
			widget, ok := b.testimonial(at(i), items[i], colours)
			if !ok {
				return nil, false
			}
			column, ok := b.sections.column(columnID, width, phpList{widget})
			if !ok {
				return nil, false
			}
			columns = append(columns, column)
		}
		rowID, ok := b.ids.next(path + "#row" + strconv.Itoa(r))
		if !ok {
			return nil, false
		}
		row := b.sections.row(rowID, columns)
		if len(columns) == 1 {
			row = designedWithSetting(row, "flex_justify_content", "center")
		}
		parts = append(parts, row)
	}
	group := designedWithMissingSettings(b.sections.group(id, parts), b.sections.gapSettings())
	return designedLabelled(group, label), true
}

// tab is one tab of an accordion, in the key order Elementor returns: the
// question as its title, the answer as one paragraph and the repeater id.
func designedTab(raw json.RawMessage, id string) (phpObject, bool) {
	item, ok := jsonObjectOf(raw)
	if !ok {
		return nil, false
	}
	question, ok1 := jsonStringOf(item["question"])
	answer, ok2 := jsonStringOf(item["answer"])
	if !ok1 || question == "" || !ok2 || answer == "" {
		return nil, false
	}
	return phpObject{
		{"tab_title", elementorHTMLText.Replace(question)},
		{"tab_content", "<p>" + elementorHTMLText.Replace(answer) + "</p>"},
		{"_id", id},
	}, true
}

// designedFAQ compiles a faq pattern: the group's id, a repeater id for each
// tab and last the accordion widget's. The titles are in the tone's heading
// colour, the open one in its accent colour and the answers in its body
// colour.
func designedFAQ(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	variant, ok := designedPatternVariant(n, "faq")
	if !ok {
		return nil, false
	}
	label, ok := designedPatternLabel("faq", variant)
	if !ok {
		return nil, false
	}
	items, ok := designedPatternItems(n, "faq")
	if !ok {
		return nil, false
	}
	colours, err := b.sections.tokens.tone(tone)
	if err != nil {
		return nil, false
	}
	id, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	at := path + ".faq"
	tabs := phpList{}
	for i, item := range items {
		tabID, ok := b.ids.item(at, "tabs", i)
		if !ok {
			return nil, false
		}
		tab, ok := designedTab(item, tabID)
		if !ok {
			return nil, false
		}
		tabs = append(tabs, tab)
	}
	accordion, ok := b.widget(at, "accordion", phpObject{
		{"tabs", tabs},
		{"faq_schema", "yes"},
		{"title_color", colours.Heading},
		{"tab_active_color", colours.Accent},
		{"content_color", colours.Body},
	})
	if !ok {
		return nil, false
	}
	return designedLabelled(b.sections.group(id, accordion), label), true
}
