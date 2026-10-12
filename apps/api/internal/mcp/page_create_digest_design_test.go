package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// designLeafEdits returns, for every leaf of the design's JSON, a copy of the
// design with that one leaf changed, keyed by its dotted path. The edits are
// made on the JSON, so they follow the design's fields whatever their Go
// types are.
func designLeafEdits(t *testing.T, d pageDesign) map[string]pageDesign {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	out := map[string]pageDesign{}
	var walk func(path string, m map[string]any)
	walk = func(path string, m map[string]any) {
		for k, v := range m {
			p := path + "." + k
			if sub, ok := v.(map[string]any); ok {
				walk(p, sub)
				continue
			}
			m[k] = changedDesignLeaf(v)
			eb, err := json.Marshal(top)
			m[k] = v
			if err != nil {
				t.Fatal(err)
			}
			var edited pageDesign
			if err := json.Unmarshal(eb, &edited); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if edited == d {
				t.Fatalf("%s: the edit did not change the design", p)
			}
			out[p] = edited
		}
	}
	walk("design", top)
	return out
}

// changedDesignLeaf is another value of the same JSON type: a colour with
// its last digit changed, any other string extended, a number plus one.
func changedDesignLeaf(v any) any {
	switch x := v.(type) {
	case string:
		if len(x) == 7 && x[0] == '#' {
			if x[6] == '0' {
				return x[:6] + "1"
			}
			return x[:6] + "0"
		}
		return x + "-changed"
	case float64:
		return x + 1
	}
	return v
}

// designWithColours is the head design with each colour passed through fn.
func designWithColours(t *testing.T, fn func(role, hex string) string) string {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal([]byte(v3HeadDesign), &top); err != nil {
		t.Fatal(err)
	}
	colours := top["colors"].(map[string]any)
	for role, v := range colours {
		colours[role] = fn(role, v.(string))
	}
	b, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// storedCardDesign reads a stored card's design and page frame back the way
// every reader of the card does.
func storedCardDesign(card []byte) (*pageDesign, string, bool) {
	top, ok := readPageCardTop(card)
	if !ok {
		return nil, "", false
	}
	return readPageCardDesign(top)
}

// TestPageCreateDigestCoversDesignAndFrame: a designed page's card stores its
// design and the page frame it is built in, and the presented digest covers
// both. Every digest here is taken over the same input bytes, so a digest
// that differs differs by the facts the control plane bound, not by the
// input_json member.
func TestPageCreateDigestCoversDesignAndFrame(t *testing.T) {
	auth := AuthorizedRequest{GrantID: uuid.New(), GrantName: "Laptop"}
	row := sqlc.Site{ID: uuid.New(), Name: "Site"}
	e := writeEntryRow()
	now := time.Unix(1_800_000_000, 0)
	pc := checkedPrecheck{builder: &PageCardBuilder{
		Builder: pageBuilderElementor, Format: elementorFormatClassic, Version: "3.30.0", Layout: elementorLayoutBoxes,
	}}
	input := v3HeadInput(nil)
	request := func(t *testing.T, facts pageCreateFacts) (string, []byte) {
		t.Helper()
		f, err := buildAbilityRequestFacts(auth, row, "example.com", e, "sum", input, facts, pc, now)
		if err != nil {
			t.Fatal(err)
		}
		// The nonce is random per request; it is fixed here so only the
		// facts differ between two digests.
		f.nonce = strings.Repeat("n", 64)
		return pageCreateDigest(auth, row, e, "sum", input, facts, pc, f), f.card
	}
	accepted := func(t *testing.T, in []byte) pageCreateFacts {
		t.Helper()
		f, refusal := checkPageCreateInput(in)
		if refusal.Code != "" || f.design == nil || f.pageFrame == "" {
			t.Fatalf("not a designed page the grammar accepts: %+v (%s)", refusal, in)
		}
		return f
	}
	base := accepted(t, input)
	baseDigest, baseCard := request(t, base)
	if d, _ := request(t, base); d != baseDigest {
		t.Fatal("the digest is not deterministic")
	}

	t.Run("every design field", func(t *testing.T) {
		edits := designLeafEdits(t, *base.design)
		if len(edits) != len(designLeaves) {
			t.Fatalf("%d design fields edited, want the %d a design holds", len(edits), len(designLeaves))
		}
		seen := map[string]string{baseDigest: "the design as sent"}
		for path, d := range edits {
			facts := base
			facts.design = &d
			digest, card := request(t, facts)
			if prev, dup := seen[digest]; dup {
				t.Errorf("%s changed and %s give the same presented digest", path, prev)
			}
			seen[digest] = path
			if bytes.Equal(card, baseCard) {
				t.Errorf("%s changed: the card is the same", path)
			}
		}
	})

	t.Run("colours compare upper case", func(t *testing.T) {
		lower := accepted(t, v3HeadInput(map[string]string{"design": designWithColours(t, func(_, hex string) string {
			return strings.ToLower(hex)
		})}))
		digest, card := request(t, lower)
		if !bytes.Equal(card, baseCard) || digest != baseDigest {
			t.Fatalf("colours sent in lower case give another card:\n%s\n%s", card, baseCard)
		}
		design, _, ok := storedCardDesign(card)
		if !ok || design.Colors != base.design.Colors || design.Colors.Ground != strings.ToUpper(design.Colors.Ground) ||
			!strings.Contains(string(card), `"ground":"`+strings.ToUpper(design.Colors.Ground)+`"`) {
			t.Fatalf("the card's colours are not the upper-case colours: %s", card)
		}
		other := accepted(t, v3HeadInput(map[string]string{"design": designWithColours(t, func(role, hex string) string {
			if role == "accent" {
				return changedDesignLeaf(hex).(string)
			}
			return hex
		})}))
		if digest, card := request(t, other); digest == baseDigest || bytes.Equal(card, baseCard) {
			t.Fatal("another accent colour gives the same card and digest")
		}
	})

	t.Run("the resolved page frame", func(t *testing.T) {
		heroOutline := `[` + v3Sec("top", "", v3Hero) + `]`
		for _, c := range []struct {
			name  string
			in    []byte
			frame string
		}{
			{"no hero, none sent", input, pageFrameTheme},
			{"a hero, none sent", v3HeadInput(map[string]string{"outline": heroOutline}), pageFrameFullWidth},
			{"a hero, theme sent", v3HeadInput(map[string]string{"outline": heroOutline, "page_frame": `"theme"`}), pageFrameTheme},
			{"no hero, canvas sent", v3HeadInput(map[string]string{"page_frame": `"canvas"`}), pageFrameCanvas},
		} {
			_, card := request(t, accepted(t, c.in))
			if _, frame, ok := storedCardDesign(card); !ok || frame != c.frame {
				t.Errorf("%s: the card's page frame is %q (ok %v), want %q: %s", c.name, frame, ok, c.frame, card)
			}
		}
		// The input sends no page_frame, so a digest that binds only the
		// frame sent gives one digest for all three.
		seen := map[string]string{}
		for _, frame := range pageFrames {
			facts := base
			facts.pageFrame = frame
			digest, _ := request(t, facts)
			if prev, dup := seen[digest]; dup {
				t.Errorf("page frames %s and %s give the same presented digest", frame, prev)
			}
			seen[digest] = frame
		}
		if len(seen) != 3 {
			t.Fatalf("%d page frames, want 3", len(seen))
		}
	})

	t.Run("a version 2 page's card is unchanged", func(t *testing.T) {
		v2 := pageCreateFacts{postType: "page", editor: pageEditorBuilderElementor, builder: pageBuilderElementor,
			elementorFormat: elementorFormatClassic, title: "T"}
		_, card := request(t, v2)
		builder, err := json.Marshal(pc.builder)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"kind":"page_create","builder":` + string(builder) + `}`; string(card) != want {
			t.Fatalf("version 2 card %s, want %s", card, want)
		}
		if design, frame, ok := storedCardDesign(card); !ok || design != nil || frame != "" {
			t.Fatalf("a version 2 card reads as designed: %v %q %v", design, frame, ok)
		}
	})

	t.Run("the stored card reads back, and a damaged one does not", func(t *testing.T) {
		media := []pageMediaFact{{ID: 42, URL: "https://example.com/a.jpg", Filename: "a.jpg", Mime: "image/jpeg", Width: 4, Height: 3}}
		card, err := pageCardFactsJSON(media, pc.builder, base.design, base.pageFrame)
		if err != nil {
			t.Fatal(err)
		}
		design, frame, ok := storedCardDesign(card)
		if !ok || *design != *base.design || frame != base.pageFrame {
			t.Fatalf("the designed card reads back as %v %q %v: %s", design, frame, ok, card)
		}
		if b, ok := ReadPageCardBuilder(card); !ok || b != *pc.builder {
			t.Fatalf("the designed card's builder: %v %v", b, ok)
		}
		if m, ok := ReadPageCardFacts(card); !ok || len(m) != 1 || m[0].ID != 42 {
			t.Fatalf("the designed card's images: %v %v", m, ok)
		}
		var good map[string]json.RawMessage
		if err := json.Unmarshal(card, &good); err != nil {
			t.Fatal(err)
		}
		damage := func(edit func(m map[string]json.RawMessage)) []byte {
			m := map[string]json.RawMessage{}
			for k, v := range good {
				m[k] = v
			}
			edit(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		for name, bad := range map[string][]byte{
			"a design without a page frame": damage(func(m map[string]json.RawMessage) { delete(m, "page_frame") }),
			"a page frame without a design": damage(func(m map[string]json.RawMessage) { delete(m, "design") }),
			"a frame that is not one":       damage(func(m map[string]json.RawMessage) { m["page_frame"] = json.RawMessage(`"wide"`) }),
			"a frame that is not a string":  damage(func(m map[string]json.RawMessage) { m["page_frame"] = json.RawMessage(`3`) }),
			"a design without a builder":    damage(func(m map[string]json.RawMessage) { delete(m, "builder") }),
			"a design missing a field": damage(func(m map[string]json.RawMessage) {
				m["design"] = designEdit(t, designChange{path: "design", key: "density"})
			}),
			"a design with a value it may not hold": damage(func(m map[string]json.RawMessage) {
				m["design"] = designEdit(t, designChange{path: "design", key: "radius", raw: `"huge"`})
			}),
		} {
			if _, _, ok := storedCardDesign(bad); ok {
				t.Errorf("%s: read as a designed card: %s", name, bad)
			}
			if _, ok := ReadPageCardFacts(bad); ok {
				t.Errorf("%s: its images were read: %s", name, bad)
			}
			if _, ok := ReadPageCardBuilder(bad); ok {
				t.Errorf("%s: its builder was read: %s", name, bad)
			}
		}
	})
}
