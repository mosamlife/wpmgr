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
// A pattern's container carries its Navigator label after its last key and
// draws its id before its children's. Node ids come from the page's
// elementorIDs; a tab's repeater id is drawn from it with item, in the set of
// taken ids the node ids share.

import (
	"encoding/json"
	"slices"
	"strconv"
)

const (
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

func init() {
	designedPatternBuilders["testimonials"] = func(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
		return designedPatternsC{b: b}.testimonials(n, path, tone)
	}
	designedPatternBuilders["faq"] = func(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
		return designedPatternsC{b: b}.faq(n, path, tone)
	}
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

// designedPatternsC compiles the patterns of this file for one page build.
type designedPatternsC struct {
	b *designedBuild
}

// variant is the variant of a pattern node of the given name: the node names
// the pattern and one of that pattern's variants of the grammar.
func (designedPatternsC) variant(n map[string]json.RawMessage, name string) (string, bool) {
	spec, known := patternSpecs[name]
	pattern, _ := jsonStringOf(n["pattern"])
	variant, ok := jsonStringOf(n["variant"])
	if !known || pattern != name || !ok || !slices.Contains(spec.variants, variant) {
		return "", false
	}
	return variant, true
}

// label is the Navigator label of a pattern's container: the label prefix, the
// pattern's name and its variant.
func (designedPatternsC) label(name, variant string) string {
	const prefix = "WPMgr · "
	titles := map[string]string{"testimonials": "Testimonials", "faq": "FAQ"}
	return prefix + titles[name] + " " + variant
}

// items is the items of an item pattern, counted by the grammar's caps.
func (designedPatternsC) items(n map[string]json.RawMessage, name string) ([]json.RawMessage, bool) {
	spec, known := patternSpecs[name]
	if !known || spec.array == "" {
		return nil, false
	}
	items, ok := jsonArrayOf(n[spec.array])
	if !ok || len(items) < spec.min || len(items) > spec.max {
		return nil, false
	}
	return items, true
}

// member is the value of a key of a built node, nil when it has none.
func (designedPatternsC) member(o phpObject, key string) any {
	if at := slices.IndexFunc(o, func(p phpPair) bool { return p.key == key }); at >= 0 {
		return o[at].value
	}
	return nil
}

// withSetting is a container with one settings member set the way PHP assigns
// an array key: replaced where the settings have it already, added after the
// last otherwise.
func (designedPatternsC) withSetting(container phpObject, key string, value any) phpObject {
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

// withMissingSettings is a container with the members of more that its
// settings do not have yet added after the last, as PHP's += does.
func (p designedPatternsC) withMissingSettings(container phpObject, more phpObject) phpObject {
	out := container
	for _, add := range more {
		settings, _ := p.member(out, "settings").(phpObject)
		if !slices.ContainsFunc(settings, func(q phpPair) bool { return q.key == add.key }) {
			out = p.withSetting(out, add.key, add.value)
		}
	}
	return out
}

// labelled is a container with its Navigator label after the last key.
func (p designedPatternsC) labelled(container phpObject, label string) phpObject {
	return p.withSetting(container, "_title", label)
}

// libraryImage is an image of the media library as a media control stores it:
// the address and mime type the site resolved for the attachment id.
func (p designedPatternsC) libraryImage(raw json.RawMessage) (phpObject, bool) {
	id, ok := jsonIntOf(raw)
	if !ok || id < 1 || id > pageCreateMaxAttachmentID {
		return nil, false
	}
	fact, known := p.b.media[id]
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
func (p designedPatternsC) testimonialSettings(raw json.RawMessage, colours designTone) (phpObject, bool) {
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
		picture, ok := p.libraryImage(rawPicture)
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
func (p designedPatternsC) testimonial(path string, raw json.RawMessage, colours designTone) (phpObject, bool) {
	settings, ok := p.testimonialSettings(raw, colours)
	if !ok {
		return nil, false
	}
	widget, ok := p.b.widget(path, "testimonial", settings)
	if !ok {
		return nil, false
	}
	built, _ := widget[0].(phpObject)
	return built, true
}

// testimonials compiles a testimonials pattern: the group's id, the featured
// testimonial's, then for each row its columns' and widgets' and last the row's
// own.
func (p designedPatternsC) testimonials(n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	variant, ok := p.variant(n, "testimonials")
	if !ok {
		return nil, false
	}
	items, ok := p.items(n, "testimonials")
	if !ok {
		return nil, false
	}
	colours, err := p.b.sections.tokens.tone(tone)
	if err != nil {
		return nil, false
	}
	id, ok := p.b.ids.next(path)
	if !ok {
		return nil, false
	}
	at := func(i int) string { return path + ".testimonials[" + strconv.Itoa(i) + "]" }
	featured, ok := p.testimonial(at(0), items[0], colours)
	if !ok {
		return nil, false
	}
	parts := phpList{featured}
	width := 100 / designedTestimonialsPerRow
	for r, first := 0, 1; first < len(items); r, first = r+1, first+designedTestimonialsPerRow {
		last := min(first+designedTestimonialsPerRow, len(items))
		columns := phpList{}
		for i := first; i < last; i++ {
			columnID, ok := p.b.ids.next(at(i) + "#column")
			if !ok {
				return nil, false
			}
			widget, ok := p.testimonial(at(i), items[i], colours)
			if !ok {
				return nil, false
			}
			column, ok := p.b.sections.column(columnID, width, phpList{widget})
			if !ok {
				return nil, false
			}
			columns = append(columns, column)
		}
		rowID, ok := p.b.ids.next(path + "#row" + strconv.Itoa(r))
		if !ok {
			return nil, false
		}
		row := p.b.sections.row(rowID, columns)
		if len(columns) == 1 {
			row = p.withSetting(row, "flex_justify_content", "center")
		}
		parts = append(parts, row)
	}
	group := p.withMissingSettings(p.b.sections.group(id, parts), p.b.sections.gapSettings())
	return p.labelled(group, p.label("testimonials", variant)), true
}

// tab is one tab of an accordion, in the key order Elementor returns: the
// question as its title, the answer as one paragraph and the repeater id.
func (designedPatternsC) tab(raw json.RawMessage, id string) (phpObject, bool) {
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

// faq compiles a faq pattern: the group's id, a repeater id for each tab and
// last the accordion widget's. The titles are in the tone's heading colour, the
// open one in its accent colour and the answers in its body colour.
func (p designedPatternsC) faq(n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	variant, ok := p.variant(n, "faq")
	if !ok {
		return nil, false
	}
	items, ok := p.items(n, "faq")
	if !ok {
		return nil, false
	}
	colours, err := p.b.sections.tokens.tone(tone)
	if err != nil {
		return nil, false
	}
	id, ok := p.b.ids.next(path)
	if !ok {
		return nil, false
	}
	at := path + ".faq"
	tabs := phpList{}
	for i, item := range items {
		tabID, ok := p.b.ids.item(at, "tabs", i)
		if !ok {
			return nil, false
		}
		tab, ok := p.tab(item, tabID)
		if !ok {
			return nil, false
		}
		tabs = append(tabs, tab)
	}
	accordion, ok := p.b.widget(at, "accordion", phpObject{
		{"tabs", tabs},
		{"faq_schema", "yes"},
		{"title_color", colours.Heading},
		{"tab_active_color", colours.Accent},
		{"content_color", colours.Body},
	})
	if !ok {
		return nil, false
	}
	return p.labelled(p.b.sections.group(id, accordion), p.label("faq", variant)), true
}
