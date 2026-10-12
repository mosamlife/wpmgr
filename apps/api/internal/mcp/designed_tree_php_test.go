package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// Helpers of the designed-page twin that the golden cases do not reach yet,
// pinned to what the agent's own classes write. The vectors below are the
// json_encode of PHP 8.5 calling ElementorSectionCompiler::cardSettings, and
// ElementorClassicMapper::leafList on a mapper set up as mapDesignedLeaves sets
// it up (the design's look, the section's tone and alignment, containers on),
// over the classes in apps/agent/includes/abilities/builders, for the design
// below. A change to those classes changes the vectors; recompute them the
// same way.

const designedVectorDesign = `{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},"fonts":{"heading":"Manrope","body":"Source Sans 3"},"scale":{"base_px":17,"ratio":"major_third"},"radius":"md","density":"normal"}`

const designedVectorRequestID = "11111111-2222-4333-8444-777777777777"

const designedVectorLeaves = `[{"type":"heading","level":1,"text":"A level one"},{"type":"paragraph","text":"The lead paragraph"},{"type":"paragraph","text":"Body after the lead"},{"type":"buttons","align":"center","buttons":[{"text":"Go","url":"#top","style":"fill"},{"text":"More","url":"/more","style":"outline"}]}]`

var designedCardVectors = []struct {
	breakpoints []string
	edge        string
	edgePx      int
	raised      bool
	want        string
}{
	{nil, "rgba(90,96,108,0.2)", 1, false, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":true},"border_color":"rgba(90,96,108,0.2)","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true}}`},
	{nil, "#0D7A63", 2, true, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"2","right":"2","bottom":"2","left":"2","isLinked":true},"border_color":"#0D7A63","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"box_shadow_box_shadow_type":"yes","box_shadow_box_shadow":{"horizontal":"0","vertical":"8","blur":"24","spread":"0","color":"rgba(21,23,28,0.08)"}}`},
	{[]string{"tablet"}, "rgba(90,96,108,0.2)", 1, false, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"padding_tablet":{"unit":"px","top":"28","right":"28","bottom":"28","left":"28","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":true},"border_color":"rgba(90,96,108,0.2)","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true}}`},
	{[]string{"tablet"}, "#0D7A63", 2, true, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"padding_tablet":{"unit":"px","top":"28","right":"28","bottom":"28","left":"28","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"2","right":"2","bottom":"2","left":"2","isLinked":true},"border_color":"#0D7A63","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"box_shadow_box_shadow_type":"yes","box_shadow_box_shadow":{"horizontal":"0","vertical":"8","blur":"24","spread":"0","color":"rgba(21,23,28,0.08)"}}`},
	{[]string{"mobile"}, "rgba(90,96,108,0.2)", 1, false, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"padding_mobile":{"unit":"px","top":"24","right":"24","bottom":"24","left":"24","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":true},"border_color":"rgba(90,96,108,0.2)","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true}}`},
	{[]string{"mobile"}, "#0D7A63", 2, true, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"padding_mobile":{"unit":"px","top":"24","right":"24","bottom":"24","left":"24","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"2","right":"2","bottom":"2","left":"2","isLinked":true},"border_color":"#0D7A63","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"box_shadow_box_shadow_type":"yes","box_shadow_box_shadow":{"horizontal":"0","vertical":"8","blur":"24","spread":"0","color":"rgba(21,23,28,0.08)"}}`},
	{[]string{"mobile", "tablet"}, "rgba(90,96,108,0.2)", 1, false, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"padding_tablet":{"unit":"px","top":"28","right":"28","bottom":"28","left":"28","isLinked":true},"padding_mobile":{"unit":"px","top":"24","right":"24","bottom":"24","left":"24","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":true},"border_color":"rgba(90,96,108,0.2)","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true}}`},
	{[]string{"mobile", "tablet"}, "#0D7A63", 2, true, `{"padding":{"unit":"px","top":"32","right":"32","bottom":"32","left":"32","isLinked":true},"padding_tablet":{"unit":"px","top":"28","right":"28","bottom":"28","left":"28","isLinked":true},"padding_mobile":{"unit":"px","top":"24","right":"24","bottom":"24","left":"24","isLinked":true},"border_border":"solid","border_width":{"unit":"px","top":"2","right":"2","bottom":"2","left":"2","isLinked":true},"border_color":"#0D7A63","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"box_shadow_box_shadow_type":"yes","box_shadow_box_shadow":{"horizontal":"0","vertical":"8","blur":"24","spread":"0","color":"rgba(21,23,28,0.08)"}}`},
}

var designedBlockVectors = []struct {
	tone, align string
	want        string
}{
	{"inverse", "center", `[{"id":"9f6caac","elType":"widget","settings":{"title":"A level one","header_size":"h1","align":"center","title_color":"#F6F5F2","typography_typography":"custom","typography_font_family":"Manrope","typography_font_size":{"unit":"px","size":"52"},"typography_font_weight":"700","typography_line_height":{"unit":"em","size":"1.05"},"typography_letter_spacing":{"unit":"em","size":"-0.02"}},"elements":[],"widgetType":"heading"},{"id":"ee46ca9","elType":"widget","settings":{"editor":"<p>The lead paragraph<\/p>","align":"center","text_color":"rgba(246,245,242,0.8)","typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"21"},"typography_font_weight":"400","typography_line_height":{"unit":"em","size":"1.5"}},"elements":[],"widgetType":"text-editor"},{"id":"5827624","elType":"widget","settings":{"editor":"<p>Body after the lead<\/p>","align":"center","text_color":"rgba(246,245,242,0.8)","typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"17"},"typography_font_weight":"400","typography_line_height":{"unit":"em","size":"1.6"}},"elements":[],"widgetType":"text-editor"},{"id":"75a8092","elType":"container","settings":{"content_width":"full","flex_direction":"row","flex_wrap":"wrap","flex_gap":{"column":"16","row":"16","isLinked":true,"unit":"px"},"flex_justify_content":"center"},"elements":[{"id":"01ea572","elType":"widget","settings":{"text":"Go","link":{"url":"#top","is_external":"","nofollow":""},"size":"md","button_type":"","background_background":"classic","background_color":"#0D7A63","button_text_color":"#FFFFFF","button_background_hover_background":"classic","button_background_hover_color":"#095545","hover_color":"#FFFFFF","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"16"},"typography_font_weight":"600","typography_line_height":{"unit":"em","size":"1"}},"elements":[],"widgetType":"button"},{"id":"d505575","elType":"widget","settings":{"text":"More","link":{"url":"\/more","is_external":"","nofollow":""},"size":"md","button_type":"","background_background":"classic","background_color":"transparent","button_text_color":"#0D7A63","border_border":"solid","border_width":{"unit":"px","top":"1.5","right":"1.5","bottom":"1.5","left":"1.5","isLinked":true},"border_color":"#0D7A63","button_background_hover_background":"classic","button_background_hover_color":"#0D7A63","hover_color":"#FFFFFF","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"16"},"typography_font_weight":"600","typography_line_height":{"unit":"em","size":"1"}},"elements":[],"widgetType":"button"}],"isInner":true}]`},
	{"ground", "start", `[{"id":"9f6caac","elType":"widget","settings":{"title":"A level one","header_size":"h1","align":"left","title_color":"#15171C","typography_typography":"custom","typography_font_family":"Manrope","typography_font_size":{"unit":"px","size":"52"},"typography_font_weight":"700","typography_line_height":{"unit":"em","size":"1.05"},"typography_letter_spacing":{"unit":"em","size":"-0.02"}},"elements":[],"widgetType":"heading"},{"id":"ee46ca9","elType":"widget","settings":{"editor":"<p>The lead paragraph<\/p>","text_color":"#5A606C","typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"21"},"typography_font_weight":"400","typography_line_height":{"unit":"em","size":"1.5"}},"elements":[],"widgetType":"text-editor"},{"id":"5827624","elType":"widget","settings":{"editor":"<p>Body after the lead<\/p>","text_color":"#5A606C","typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"17"},"typography_font_weight":"400","typography_line_height":{"unit":"em","size":"1.6"}},"elements":[],"widgetType":"text-editor"},{"id":"75a8092","elType":"container","settings":{"content_width":"full","flex_direction":"row","flex_wrap":"wrap","flex_gap":{"column":"16","row":"16","isLinked":true,"unit":"px"},"flex_justify_content":"center"},"elements":[{"id":"01ea572","elType":"widget","settings":{"text":"Go","link":{"url":"#top","is_external":"","nofollow":""},"size":"md","button_type":"","background_background":"classic","background_color":"#0D7A63","button_text_color":"#FFFFFF","button_background_hover_background":"classic","button_background_hover_color":"#095545","hover_color":"#FFFFFF","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"16"},"typography_font_weight":"600","typography_line_height":{"unit":"em","size":"1"}},"elements":[],"widgetType":"button"},{"id":"d505575","elType":"widget","settings":{"text":"More","link":{"url":"\/more","is_external":"","nofollow":""},"size":"md","button_type":"","background_background":"classic","background_color":"transparent","button_text_color":"#0D7A63","border_border":"solid","border_width":{"unit":"px","top":"1.5","right":"1.5","bottom":"1.5","left":"1.5","isLinked":true},"border_color":"#0D7A63","button_background_hover_background":"classic","button_background_hover_color":"#0D7A63","hover_color":"#FFFFFF","border_radius":{"unit":"px","top":"12","right":"12","bottom":"12","left":"12","isLinked":true},"typography_typography":"custom","typography_font_family":"Source Sans 3","typography_font_size":{"unit":"px","size":"16"},"typography_font_weight":"600","typography_line_height":{"unit":"em","size":"1"}},"elements":[],"widgetType":"button"}],"isInner":true}]`},
}

func designedVectorTokens(t *testing.T) *designTokens {
	t.Helper()
	design, problem := validatePageDesign(json.RawMessage(designedVectorDesign))
	if problem != nil {
		t.Fatalf("the vector design was refused: %+v", problem)
	}
	tokens, err := newDesignTokens(design)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

// TestDesignedCardSettingsArePHPs: the settings of a card, for every set of
// active breakpoints, a flat and a raised card.
func TestDesignedCardSettingsArePHPs(t *testing.T) {
	tokens := designedVectorTokens(t)
	for _, v := range designedCardVectors {
		name := strings.Join(v.breakpoints, ",") + " " + v.edge
		settings, ok := newDesignSections(tokens, v.breakpoints, nil).cardSettings(v.edge, v.edgePx, v.raised)
		if !ok {
			t.Errorf("%s: the card was refused", name)
			continue
		}
		got, ok := phpEncodeBytes(settings)
		if !ok || string(got) != v.want {
			t.Errorf("%s (edge %d px, raised %v)\n got: %s\nwant: %s", name, v.edgePx, v.raised, got, v.want)
		}
	}
	if _, ok := newDesignSections(tokens, nil, nil).cardSettings("#0D7A63", 0, false); ok {
		t.Error("a card with no border was built")
	}
}

// TestDesignedBlocksArePHPs: the blocks of a hero or a call to action, built
// as a section's leaves are in the tone and alignment the pattern asks for: the
// first paragraph after the level 1 heading is the lead.
func TestDesignedBlocksArePHPs(t *testing.T) {
	tokens := designedVectorTokens(t)
	var leaves []json.RawMessage
	if err := json.Unmarshal([]byte(designedVectorLeaves), &leaves); err != nil {
		t.Fatal(err)
	}
	for _, v := range designedBlockVectors {
		sections := newDesignSections(tokens, nil, nil)
		b := &designedBuild{
			ids:      &elementorIDs{requestID: designedVectorRequestID, taken: map[string]bool{}},
			sections: sections, look: designLeafStyle{tokens: tokens}, media: sections.media,
			// The section's own state must not leak into the blocks.
			tone: "surface", align: "start", leadNext: true,
		}
		built, ok := b.blocks(leaves, v.tone, v.align, "outline[0].children[0].children")
		if !ok {
			t.Errorf("%s %s: the blocks were refused", v.tone, v.align)
			continue
		}
		got, ok := phpEncodeBytes(built)
		if !ok || string(got) != v.want {
			t.Errorf("%s %s\n got: %s\nwant: %s", v.tone, v.align, got, v.want)
		}
		if b.tone != "surface" || b.align != "start" || !b.leadNext {
			t.Errorf("%s %s: building the blocks changed the section's state", v.tone, v.align)
		}
	}
	b := &designedBuild{sections: newDesignSections(tokens, nil, nil)}
	if _, ok := b.blocks(nil, "ground", "start", "x"); ok {
		t.Error("no blocks were built from an empty list")
	}
	if _, ok := b.blocks(leaves, "plum", "start", "x"); ok {
		t.Error("blocks were built in a tone that is not one")
	}
}
