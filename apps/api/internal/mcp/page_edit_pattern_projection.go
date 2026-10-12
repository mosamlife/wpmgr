package mcp

// ElementorPatternReader reads a labelled container by its nesting and widget
// types. Styling and text do not determine its shape. Item patterns offer
// their text as paths; composed patterns project their ordinary children.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const elementorPatternMaxDepth = 8

var (
	elementorPatternWidgetType = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	elementorPatternNumber     = regexp.MustCompile(`^[0-9]{1,9}$`)
)

func elementorPatternOf(settings any) (string, string) {
	label, _ := phpStringAt(settings, "_title")
	for name, spec := range patternSpecs {
		for _, variant := range spec.variants {
			var expected string
			switch name {
			case "features":
				expected = designedCardsLabel("Features", variant)
			case "pricing":
				expected = designedCardsLabel("Pricing", variant)
			case "testimonials", "faq":
				expected = (designedPatternsC{}).label(name, variant)
			default:
				expected = (designedPatternsA{}).label(name, variant)
			}
			if label == expected {
				return name, variant
			}
		}
	}
	return "", ""
}

// elementorPatternList is ArrayShape::isList over an associative JSON decode.
func elementorPatternList(v any) ([]any, bool) {
	if list, ok := v.(phpList); ok {
		return list, true
	}
	if obj, ok := v.(phpObject); ok {
		list := make([]any, 0, len(obj))
		for i, pair := range obj {
			if pair.key != strconv.Itoa(i) {
				return nil, false
			}
			list = append(list, pair.value)
		}
		return list, true
	}
	return nil, false
}

func elementorPatternShape(node any, depth int) string {
	if !phpIsArray(node) || depth > elementorPatternMaxDepth {
		return "?"
	}
	if _, ok := phpStringAt(node, "id"); !ok {
		return "?"
	}
	settings, has := phpGet(node, "settings")
	if !has {
		settings = phpList{}
	}
	children, has := phpGet(node, "elements")
	if !has {
		children = phpList{}
	}
	list, ok := elementorPatternList(children)
	if !phpIsArray(settings) || !ok {
		return "?"
	}
	elType, _ := phpStringAt(node, "elType")
	if elType == "widget" {
		typ, ok := phpStringAt(node, "widgetType")
		if ok && len(list) == 0 && elementorPatternWidgetType.MatchString(typ) {
			return typ
		}
		return "?"
	}
	direction := "column"
	if raw, has := phpGet(settings, "flex_direction"); has {
		direction, _ = raw.(string)
	}
	if elType != "container" || (direction != "row" && direction != "column") {
		return "?"
	}
	parts := make([]string, 0, len(list))
	for _, child := range list {
		parts = append(parts, elementorPatternShape(child, depth+1))
	}
	prefix := "C"
	if direction == "row" {
		prefix = "R"
	}
	return prefix + "[" + strings.Join(parts, ",") + "]"
}

func elementorPatternWidgets(node any, typ string, depth int) []any {
	var out []any
	if depth > elementorPatternMaxDepth {
		return out
	}
	children, _ := phpGet(node, "elements")
	for _, child := range phpValues(children) {
		if !phpIsArray(child) {
			continue
		}
		elType, _ := phpStringAt(child, "elType")
		widgetType, _ := phpStringAt(child, "widgetType")
		if elType == "widget" && widgetType == typ {
			out = append(out, child)
		} else {
			out = append(out, elementorPatternWidgets(child, typ, depth+1)...)
		}
	}
	return out
}

type elementorPatternField struct {
	projField
	form   string
	widget any
	at     []string
}

func elementorPatternFields(node any, name, variant string) ([]elementorPatternField, bool) {
	shape := elementorPatternShape(node, 0)
	var out []elementorPatternField
	put := func(path string, widget any, form string, at ...string) {
		value, _ := phpGet(widget, "settings")
		for _, step := range at {
			value, _ = phpGet(value, step)
		}
		text, ok := value.(string)
		if _, idOK := phpStringAt(widget, "id"); !ok || !idOK {
			return
		}
		switch form {
		case "paragraph":
			if !elementorTextTemplates[0].re.MatchString(text) {
				return
			}
			text = phpHTMLEntityDecode(text[3 : len(text)-4])
		case "number":
			if !elementorPatternNumber.MatchString(text) {
				return
			}
		default:
			text = phpHTMLEntityDecode(text)
		}
		out = append(out, elementorPatternField{projField: projField{path, text}, form: form, widget: widget, at: at})
	}
	within := func(n int) bool { spec := patternSpecs[name]; return n >= spec.min && n <= spec.max }
	repeated := func(part string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = part
		}
		return strings.Join(parts, ",")
	}
	children, _ := phpGet(node, "elements")
	list, listOK := elementorPatternList(children)
	switch name {
	case "hero", "cta":
		if strings.Contains(shape, "?") || shape == "C[]" || shape == "R[]" {
			return nil, false
		}
		if strings.HasPrefix(shape, "C") {
			return out, true
		}
		if variant != "split" || len(list) != 2 {
			return nil, false
		}
		for _, col := range list {
			inner := elementorPatternShape(col, 0)
			if !strings.HasPrefix(inner, "C") || inner == "C[]" {
				return nil, false
			}
		}
	case "features":
		widgets := elementorPatternWidgets(node, "icon-box", 0)
		n := len(widgets)
		if !within(n) {
			return nil, false
		}
		var expected string
		if variant == "split" {
			left := (n + 1) / 2
			expected = "R[C[" + repeated("icon-box", left) + "],C[" + repeated("icon-box", n-left) + "]]"
		} else {
			var rows []string
			for _, shares := range designedRowShares(variant, n) {
				row := "R[" + repeated("C[icon-box]", len(shares)) + "]"
				if len(shares) == 1 && shares[0] == designedCardAlone {
					row = "C[icon-box]"
				}
				rows = append(rows, row)
			}
			expected = "C[" + strings.Join(rows, ",") + "]"
		}
		if shape != expected {
			return nil, false
		}
		for k, widget := range widgets {
			put(fmt.Sprintf("features.%d.title", k), widget, "text", "title_text")
			put(fmt.Sprintf("features.%d.text", k), widget, "text", "description_text")
		}
	case "testimonials":
		widgets := elementorPatternWidgets(node, "testimonial", 0)
		if !within(len(widgets)) {
			return nil, false
		}
		parts := []string{"testimonial"}
		for left := len(widgets) - 1; left > 0; left -= designedTestimonialsPerRow {
			parts = append(parts, "R["+repeated("C[testimonial]", min(left, designedTestimonialsPerRow))+"]")
		}
		if shape != "C["+strings.Join(parts, ",")+"]" {
			return nil, false
		}
		for k, widget := range widgets {
			for _, f := range []struct{ path, key string }{{"quote", "testimonial_content"}, {"name", "testimonial_name"}, {"role", "testimonial_job"}} {
				put(fmt.Sprintf("testimonials.%d.%s", k, f.path), widget, "text", f.key)
			}
		}
	case "pricing":
		if !listOK || !within(len(list)) || !strings.HasPrefix(shape, "R") {
			return nil, false
		}
		for k, card := range list {
			cardShape := elementorPatternShape(card, 0)
			period := cardShape == "C[heading,heading,text-editor,icon-list,R[button]]"
			if !period && cardShape != "C[heading,heading,icon-list,R[button]]" {
				return nil, false
			}
			partsRaw, _ := phpGet(card, "elements")
			parts, _ := elementorPatternList(partsRaw)
			listAt := 2
			if period {
				listAt = 3
			}
			settings, _ := phpGet(parts[listAt], "settings")
			linesRaw, _ := phpGet(settings, "icon_list")
			lines, ok := elementorPatternList(linesRaw)
			if !ok || len(lines) == 0 || len(lines) > pageCreateMaxPlanBullets {
				return nil, false
			}
			buttons, _ := phpGet(parts[len(parts)-1], "elements")
			button := phpValues(buttons)[0]
			prefix := fmt.Sprintf("plans.%d.", k)
			put(prefix+"name", parts[0], "text", "title")
			put(prefix+"price", parts[1], "text", "title")
			if period {
				put(prefix+"period", parts[2], "paragraph", "editor")
			}
			for j := range lines {
				put(prefix+"features."+strconv.Itoa(j), parts[listAt], "text", "icon_list", strconv.Itoa(j), "text")
			}
			put(prefix+"cta.text", button, "text", "text")
			put(prefix+"cta.url", button, "url", "link", "url")
		}
	case "faq":
		if shape != "C[accordion]" {
			return nil, false
		}
		accordion := list[0]
		settings, _ := phpGet(accordion, "settings")
		tabsRaw, _ := phpGet(settings, "tabs")
		tabs, ok := elementorPatternList(tabsRaw)
		if !ok || !within(len(tabs)) {
			return nil, false
		}
		for k := range tabs {
			put(fmt.Sprintf("faq.%d.question", k), accordion, "text", "tabs", strconv.Itoa(k), "tab_title")
			put(fmt.Sprintf("faq.%d.answer", k), accordion, "paragraph", "tabs", strconv.Itoa(k), "tab_content")
		}
	case "stats":
		widgets := elementorPatternWidgets(node, "counter", 0)
		if !within(len(widgets)) || shape != "R["+repeated("C[counter]", len(widgets))+"]" {
			return nil, false
		}
		for k, widget := range widgets {
			put(fmt.Sprintf("stats.%d.value", k), widget, "number", "ending_number")
			put(fmt.Sprintf("stats.%d.suffix", k), widget, "text", "suffix")
			put(fmt.Sprintf("stats.%d.label", k), widget, "text", "title")
		}
	case "logos":
		n := len(elementorPatternWidgets(node, "image", 0))
		if !within(n) || shape != "R["+repeated("image", n)+"]" {
			return nil, false
		}
	default:
		return nil, false
	}
	return out, true
}

func elementorPatternSubtreeDynamic(node any, depth int) bool {
	children, _ := phpGet(node, "elements")
	for _, child := range phpValues(children) {
		if !phpIsArray(child) {
			continue
		}
		settings, _ := phpGet(child, "settings")
		if depth > elementorPatternMaxDepth || (phpIsArray(settings) && elementorHoldsDynamicKey(settings, 1)) || elementorPatternSubtreeDynamic(child, depth+1) {
			return true
		}
	}
	return false
}

func elementorProjectPattern(node any, id, parent, name, variant string, p *projection) error {
	fields, ok := elementorPatternFields(node, name, variant)
	if !ok {
		return p.addLocked(id, parent, elementorLabelPatternEdited)
	}
	composed := name == "hero" || name == "cta"
	if !composed && elementorPatternSubtreeDynamic(node, 0) {
		return p.addLocked(id, parent, elementorLabelDynamic)
	}
	n := projNode{ref: id, parent: parent, kind: "pattern", variant: variant, pattern: name}
	for _, field := range fields {
		n.editable = append(n.editable, field.field)
		n.text = append(n.text, field.projField)
		if field.form == "url" && strings.HasPrefix(field.text, "#") {
			n.links = append(n.links, field.text)
		}
	}
	if err := p.add(n); err != nil {
		return err
	}
	if composed {
		settings, _ := phpGet(node, "settings")
		kind := "pattern"
		if direction, _ := phpStringAt(settings, "flex_direction"); direction == "row" {
			kind = "columns"
		}
		children, _ := phpGet(node, "elements")
		return elementorProjectNodes(phpValues(children), id, kind, p)
	}
	return nil
}
