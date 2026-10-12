package mcp

// The features and pricing patterns of a designed page, and the closed list of
// icons the features show: the control plane's twin of the agent's
// ElementorPatternCompiler::features and ::pricing and of ElementorPatternCards,
// registered with the walk in designed_tree.go.
//
//	features split   the icon boxes cut in two columns of equal width, the left
//	                 column the longer by one when the count is odd; the icon
//	                 stands to the left of its text
//	features grid    cards of one share in rows of three, a row of two where a
//	                 row of one would be left (4 items make 2 and 2, 7 make 3, 2
//	                 and 2); the icon stands above its text
//	features bento   cards of mixed widths in rows of two, 60 and 40 percent and
//	                 then 40 and 60, a last card alone in its row at the full
//	                 width
//	pricing          a centred row with one card per plan, each of equal width
//	                 (the share of two cards when there is one plan)
//
// A plan card holds its name (an h3 heading), its price (a heading in the h2
// style that is a div), its period when it has one, a checklist and its button;
// the highlighted plan has an accent border and a fill button, the others a
// hairline border and an outline button, and every plan casts the card shadow.
//
// Node ids are drawn in the order the agent draws them: an icon box's id before
// the containers around it, a group's or a row's before its cards', a card's
// after its elements', and a checklist's item ids before the checklist's own.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

func init() {
	designedPatternBuilders["features"] = designedFeatures
	designedPatternBuilders["pricing"] = designedPricing
}

const (
	// designedIconLibrary is the Elementor icon library every icon of the list
	// is in.
	designedIconLibrary = "fa-solid"

	designedCardsLabelPrefix = "WPMgr · "
	designedCardsWideShare   = 60
	designedCardsNarrowShare = 40
	designedCardsGridPerRow  = 3
	designedCardsCheckIcon   = "check"
	designedCardsCheckPx     = "18"
	designedCardsEdgePx      = 2

	// designedCardAlone is the share of a card that stands alone in its row.
	designedCardAlone = 0
)

// designedIcons is the closed list of icons (the grammar's pageIcons): the name
// of each, then the icon of Elementor's solid library it stands for.
var designedIcons = map[string]string{
	"arrow-right": "fas fa-arrow-right",
	"award":       "fas fa-award",
	"bell":        "fas fa-bell",
	"bolt":        "fas fa-bolt",
	"book":        "fas fa-book",
	"briefcase":   "fas fa-briefcase",
	"calendar":    "fas fa-calendar",
	"chart":       "fas fa-chart-line",
	"check":       "fas fa-check",
	"clock":       "fas fa-clock",
	"cloud":       "fas fa-cloud",
	"code":        "fas fa-code",
	"cog":         "fas fa-cog",
	"comment":     "fas fa-comment",
	"compass":     "fas fa-compass",
	"credit-card": "fas fa-credit-card",
	"cube":        "fas fa-cube",
	"database":    "fas fa-database",
	"envelope":    "fas fa-envelope",
	"eye":         "fas fa-eye",
	"flag":        "fas fa-flag",
	"gift":        "fas fa-gift",
	"globe":       "fas fa-globe",
	"heart":       "fas fa-heart",
	"home":        "fas fa-home",
	"image":       "fas fa-image",
	"key":         "fas fa-key",
	"laptop":      "fas fa-laptop",
	"layers":      "fas fa-layer-group",
	"leaf":        "fas fa-leaf",
	"lightbulb":   "fas fa-lightbulb",
	"lock":        "fas fa-lock",
	"map-pin":     "fas fa-map-marker-alt",
	"phone":       "fas fa-phone",
	"rocket":      "fas fa-rocket",
	"search":      "fas fa-search",
	"shield":      "fas fa-shield-alt",
	"star":        "fas fa-star",
	"tag":         "fas fa-tag",
	"users":       "fas fa-users",
}

// designedIcon is the icon value a settings control stores for a name of the
// list: the Elementor icon and its library.
func designedIcon(name string) (phpObject, bool) {
	value, known := designedIcons[name]
	if !known {
		return nil, false
	}
	return phpObject{{"value", value}, {"library", designedIconLibrary}}, true
}

// designedCards compiles the patterns built from icon boxes and cards, in the
// build of the page they stand in.
type designedCards struct {
	b *designedBuild
}

// designedCardsItems reads a features or pricing node: its variant, one of the
// pattern's, and its items, as many as the grammar allows.
func designedCardsItems(n map[string]json.RawMessage, name string) (string, []json.RawMessage, bool) {
	spec := patternSpecs[name]
	if pattern, _ := jsonStringOf(n["pattern"]); pattern != name {
		return "", nil, false
	}
	variant, ok := jsonStringOf(n["variant"])
	if !ok || !slices.Contains(spec.variants, variant) {
		return "", nil, false
	}
	items, ok := jsonArrayOf(n[spec.array])
	if !ok || len(items) < spec.min || len(items) > spec.max {
		return "", nil, false
	}
	return variant, items, true
}

// designedCardsText is a member of an item that is text that is not empty.
func designedCardsText(item map[string]json.RawMessage, field string) (string, bool) {
	value, ok := jsonStringOf(item[field])
	return value, ok && value != ""
}

// designedCardsLabel is the Navigator label of a pattern's container: the
// label prefix, the pattern's name and its variant.
func designedCardsLabel(name, variant string) string {
	return designedCardsLabelPrefix + name + " " + variant
}

// designedCardsWidget is a widget with the id it was given.
func designedCardsWidget(id, kind string, settings phpObject) phpObject {
	return phpObject{{"id", id}, {"elType", "widget"}, {"settings", settings}, {"elements", phpList{}}, {"widgetType", kind}}
}

// designedCardsSettings is the index of a node's settings and a copy of them.
func designedCardsSettings(node phpObject) (int, phpObject, bool) {
	for i, pair := range node {
		if pair.key == "settings" {
			settings, ok := pair.value.(phpObject)
			return i, slices.Clone(settings), ok
		}
	}
	return 0, nil, false
}

// designedCardsSet sets one key of a node's settings as an assignment to a PHP
// array key does: the value of a key the settings have is replaced, a key they
// have not is added after the last.
func designedCardsSet(node phpObject, key string, value any) phpObject {
	at, settings, ok := designedCardsSettings(node)
	if !ok {
		return node
	}
	if i := slices.IndexFunc(settings, func(p phpPair) bool { return p.key == key }); i >= 0 {
		settings[i].value = value
	} else {
		settings = append(settings, phpPair{key, value})
	}
	out := slices.Clone(node)
	out[at].value = settings
	return out
}

// designedCardsUnion adds keys to a node's settings as PHP's array union does:
// a key the settings already have keeps its value, the others follow the last.
func designedCardsUnion(node phpObject, extra phpObject) phpObject {
	at, settings, ok := designedCardsSettings(node)
	if !ok {
		return node
	}
	for _, pair := range extra {
		if !slices.ContainsFunc(settings, func(p phpPair) bool { return p.key == pair.key }) {
			settings = append(settings, pair)
		}
	}
	out := slices.Clone(node)
	out[at].value = settings
	return out
}

// ---------------------------------------------------------------------------
// Features
// ---------------------------------------------------------------------------

// designedFeatures compiles a features pattern: icon boxes as a two-column list
// (split) or as cards in rows (bento, grid), under the pattern's label.
func designedFeatures(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	c := designedCards{b}
	variant, items, ok := designedCardsItems(n, "features")
	if !ok {
		return nil, false
	}
	position := "top"
	if variant == "split" {
		position = "left"
	}
	boxes := make(phpList, 0, len(items))
	for i, raw := range items {
		id, ok := b.ids.next(fmt.Sprintf("%s.features[%d]", path, i))
		if !ok {
			return nil, false
		}
		box, ok := c.iconBox(id, raw, position, tone)
		if !ok {
			return nil, false
		}
		boxes = append(boxes, box)
	}
	var built phpObject
	if variant == "split" {
		built, ok = c.halves(path, boxes)
	} else {
		built, ok = c.grid(path, variant, boxes)
	}
	if !ok {
		return nil, false
	}
	return designedCardsSet(built, "_title", designedCardsLabel("Features", variant)), true
}

// iconBox is one icon box: the icon in the tone's accent, the title in its
// heading colour set in the h3 style of the type ramp, the text in its body
// colour.
func (c designedCards) iconBox(id string, raw json.RawMessage, position, tone string) (phpObject, bool) {
	item, ok := jsonObjectOf(raw)
	if !ok {
		return nil, false
	}
	name, ok := jsonStringOf(item["icon"])
	if !ok {
		return nil, false
	}
	icon, ok := designedIcon(name)
	if !ok {
		return nil, false
	}
	title, ok1 := designedCardsText(item, "title")
	text, ok2 := designedCardsText(item, "text")
	colours, err := c.b.sections.tokens.tone(tone)
	look, ok3 := c.b.look.headingStyle(3, tone, "start")
	if !ok1 || !ok2 || err != nil || !ok3 {
		return nil, false
	}
	settings := phpObject{
		{"selected_icon", icon},
		{"title_text", elementorHTMLText.Replace(title)},
		{"description_text", elementorHTMLText.Replace(text)},
		{"position", position},
		{"title_size", "h3"},
		{"primary_color", colours.Accent},
		{"title_color", colours.Heading},
		{"description_color", colours.Body},
	}
	// The title is set in the ramp's h3, written whole by the heading style and
	// moved to the title's group.
	for _, pair := range look {
		if strings.HasPrefix(pair.key, "typography_") {
			settings = append(settings, phpPair{"title_" + pair.key, pair.value})
		}
	}
	return designedCardsWidget(id, "icon-box", settings), true
}

// halves cuts the icon boxes in two columns of equal width, the left one the
// longer by one when the count is odd.
func (c designedCards) halves(path string, boxes phpList) (phpObject, bool) {
	rowID, ok1 := c.b.ids.next(path)
	leftID, ok2 := c.b.ids.next(path + "#left")
	rightID, ok3 := c.b.ids.next(path + "#right")
	if !ok1 || !ok2 || !ok3 {
		return nil, false
	}
	left := (len(boxes) + 1) / 2
	l, ok1 := c.b.sections.column(leftID, 50, slices.Clone(boxes[:left]))
	r, ok2 := c.b.sections.column(rightID, 50, slices.Clone(boxes[left:]))
	if !ok1 || !ok2 {
		return nil, false
	}
	return c.b.sections.row(rowID, phpList{l, r}), true
}

// grid puts the icon boxes in cards in rows, in a group that spaces its rows
// with the design's row gap.
func (c designedCards) grid(path, variant string, boxes phpList) (phpObject, bool) {
	groupID, ok := c.b.ids.next(path)
	if !ok {
		return nil, false
	}
	edge, err := c.b.sections.tokens.hairline()
	if err != nil {
		return nil, false
	}
	chrome, ok := c.b.sections.cardSettings(edge.Color, edge.Width, false)
	if !ok {
		return nil, false
	}
	rows := phpList{}
	at := 0
	for r, shares := range designedRowShares(variant, len(boxes)) {
		cards := phpList{}
		for _, share := range shares {
			id, ok := c.b.ids.next(fmt.Sprintf("%s#card%d", path, at))
			if !ok {
				return nil, false
			}
			card, ok := c.card(id, share, phpList{boxes[at]}, chrome)
			if !ok {
				return nil, false
			}
			cards = append(cards, card)
			at++
		}
		// A card alone in its row stands straight in the group, which gives it
		// the full width.
		if len(shares) == 1 && shares[0] == designedCardAlone {
			rows = append(rows, cards[0])
			continue
		}
		rowID, ok := c.b.ids.next(fmt.Sprintf("%s#row%d", path, r))
		if !ok {
			return nil, false
		}
		rows = append(rows, c.b.sections.row(rowID, cards))
	}
	return designedCardsUnion(c.b.sections.group(groupID, rows), c.b.sections.gapSettings()), true
}

// designedRowShares is the share of each card in percent for each row of a grid
// or a bento; designedCardAlone is a card alone in its row.
func designedRowShares(variant string, count int) [][]int {
	var rows [][]int
	if variant == "bento" {
		for left, wide := count, true; left > 0; left, wide = left-2, !wide {
			if left == 1 {
				rows = append(rows, []int{designedCardAlone})
				break
			}
			if wide {
				rows = append(rows, []int{designedCardsWideShare, designedCardsNarrowShare})
			} else {
				rows = append(rows, []int{designedCardsNarrowShare, designedCardsWideShare})
			}
		}
		return rows
	}
	for left := count; left > 0; {
		// Four cards make two rows of two; a row of one is never left.
		take := min(designedCardsGridPerRow, left)
		if left == 4 {
			take = 2
		}
		row := make([]int, take)
		for i := range row {
			row[i] = 100 / take
		}
		rows = append(rows, row)
		left -= take
	}
	return rows
}

// card is one card: a column of the row, or a group when it stands alone,
// holding its elements, with the card settings after the layout keys.
func (c designedCards) card(id string, share int, elements phpList, chrome phpObject) (phpObject, bool) {
	if share == designedCardAlone {
		return designedCardsUnion(c.b.sections.group(id, elements), chrome), true
	}
	column, ok := c.b.sections.column(id, share, elements)
	if !ok {
		return nil, false
	}
	return designedCardsUnion(column, chrome), true
}

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

// designedPricing compiles a pricing pattern: a centred row with one card per
// plan, under the pattern's label.
func designedPricing(b *designedBuild, n map[string]json.RawMessage, path, tone string) (phpObject, bool) {
	c := designedCards{b}
	variant, plans, ok := designedCardsItems(n, "pricing")
	if !ok {
		return nil, false
	}
	rowID, ok := b.ids.next(path)
	if !ok {
		return nil, false
	}
	// One plan takes the share of two, and the row centres it.
	share := 100 / max(len(plans), 2)
	cards := make(phpList, 0, len(plans))
	for i, raw := range plans {
		card, ok := c.plan(fmt.Sprintf("%s.plans[%d]", path, i), raw, share, tone)
		if !ok {
			return nil, false
		}
		cards = append(cards, card)
	}
	row := designedCardsSet(b.sections.row(rowID, cards), "flex_justify_content", "center")
	return designedCardsSet(row, "_title", designedCardsLabel("Pricing", variant)), true
}

// plan is one plan as a card.
func (c designedCards) plan(at string, raw json.RawMessage, share int, tone string) (phpObject, bool) {
	plan, ok := jsonObjectOf(raw)
	if !ok {
		return nil, false
	}
	name, ok1 := designedCardsText(plan, "name")
	price, ok2 := designedCardsText(plan, "price")
	if !ok1 || !ok2 {
		return nil, false
	}
	// A period the plan names must be text that is not empty, null included.
	period := ""
	_, hasPeriod := plan["period"]
	if hasPeriod {
		if period, ok = designedCardsText(plan, "period"); !ok {
			return nil, false
		}
	}
	lines, ok := designedCardsLines(plan)
	cta, ok1 := jsonObjectOf(plan["cta"])
	if !ok || !ok1 {
		return nil, false
	}
	// An absent highlight and a null one are both no highlight.
	highlighted := false
	if value, has := present(plan, "highlighted"); has {
		if highlighted, ok = jsonBoolOf(value); !ok {
			return nil, false
		}
	}
	tokens := c.b.sections.tokens
	colours, err := tokens.tone(tone)
	if err != nil {
		return nil, false
	}

	elements := phpList{}
	for _, h := range []struct {
		field, text, tag string
		level            int64
	}{{"name", name, "h3", 3}, {"price", price, "div", 2}} {
		id, ok := c.b.ids.next(at + "." + h.field)
		if !ok {
			return nil, false
		}
		heading, ok := c.heading(id, h.text, h.level, h.tag, tone)
		if !ok {
			return nil, false
		}
		elements = append(elements, heading)
	}
	if hasPeriod {
		id, ok1 := c.b.ids.next(at + ".period")
		look, ok2 := c.b.look.textStyle("body", tone, "start")
		if !ok1 || !ok2 {
			return nil, false
		}
		elements = append(elements, designedCardsWidget(id, "text-editor",
			append(phpObject{{"editor", "<p>" + elementorHTMLText.Replace(period) + "</p>"}}, look...)))
	}
	checklist, ok := c.checklist(at+".features", lines, colours)
	if !ok {
		return nil, false
	}
	elements = append(elements, checklist)

	style, edgeColour, edgePx := "outline", "", 0
	if highlighted {
		style, edgeColour, edgePx = "fill", colours.Accent, designedCardsEdgePx
	} else {
		edge, err := tokens.hairline()
		if err != nil {
			return nil, false
		}
		edgeColour, edgePx = edge.Color, edge.Width
	}
	rowID, ok1 := c.b.ids.next(at + ".cta#row")
	buttonID, ok2 := c.b.ids.next(at + ".cta")
	if !ok1 || !ok2 {
		return nil, false
	}
	button, ok := c.button(buttonID, cta, style, tone)
	if !ok {
		return nil, false
	}
	row, ok := c.b.look.buttonRow(rowID, phpList{button}, "left")
	if !ok {
		return nil, false
	}
	elements = append(elements, row)

	chrome, ok := c.b.sections.cardSettings(edgeColour, edgePx, true)
	cardID, ok1 := c.b.ids.next(at + "#card")
	if !ok || !ok1 {
		return nil, false
	}
	return c.card(cardID, share, elements, chrome)
}

// heading is a heading of the type ramp in the section's tone, aligned to the
// start: level 2 is the h2 style and level 3 the h3 style, written as tag.
func (c designedCards) heading(id, text string, level int64, tag, tone string) (phpObject, bool) {
	look, ok := c.b.look.headingStyle(level, tone, "start")
	if !ok {
		return nil, false
	}
	return designedCardsWidget(id, "heading", append(phpObject{{"title", elementorHTMLText.Replace(text)}, {"header_size", tag}}, look...)), true
}

// designedCardsLines are the feature lines of a plan: 1 to the grammar's most
// lines of text that is not empty.
func designedCardsLines(plan map[string]json.RawMessage) ([]string, bool) {
	list, ok := jsonArrayOf(plan["features"])
	if !ok || len(list) == 0 || len(list) > pageCreateMaxPlanBullets {
		return nil, false
	}
	lines := make([]string, 0, len(list))
	for _, raw := range list {
		line, ok := jsonStringOf(raw)
		if !ok || line == "" {
			return nil, false
		}
		lines = append(lines, line)
	}
	return lines, true
}

// checklist is the icon-list widget of a plan's features, a check in front of
// each. The repeater ids of its lines are drawn before the widget's own id.
func (c designedCards) checklist(at string, lines []string, colours designTone) (phpObject, bool) {
	check, ok := designedIcon(designedCardsCheckIcon)
	if !ok {
		return nil, false
	}
	items := make(phpList, 0, len(lines))
	for k, line := range lines {
		id, ok := c.b.ids.item(at, "icon_list", k)
		if !ok {
			return nil, false
		}
		items = append(items, phpObject{{"text", elementorHTMLText.Replace(line)}, {"selected_icon", check}, {"_id", id}})
	}
	id, ok := c.b.ids.next(at)
	if !ok {
		return nil, false
	}
	return designedCardsWidget(id, "icon-list", phpObject{
		{"icon_list", items},
		{"icon_color", colours.Accent},
		{"text_color", colours.Body},
		{"icon_size", phpObject{{"unit", "px"}, {"size", designedCardsCheckPx}}},
	}), true
}

// button is a plan's button as the leaf compiler writes a button; a link that
// holds "&" is refused, as it is anywhere on a designed page.
func (c designedCards) button(id string, cta map[string]json.RawMessage, style, tone string) (phpObject, bool) {
	text, ok1 := designedCardsText(cta, "text")
	url, ok2 := designedCardsText(cta, "url")
	if !ok1 || !ok2 || strings.Contains(url, "&") {
		return nil, false
	}
	look, ok := c.b.look.buttonStyle(style, tone)
	if !ok {
		return nil, false
	}
	return designedCardsWidget(id, "button", append(phpObject{
		{"text", elementorHTMLText.Replace(text)},
		{"link", phpObject{{"url", url}, {"is_external", ""}, {"nofollow", ""}}},
	}, look...)), true
}
