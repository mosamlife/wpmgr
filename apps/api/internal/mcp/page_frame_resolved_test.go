package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// frameAnswer is a designed page's precheck answer as the agent sends it,
// with builder_facts at the top level next to preview; facts is that
// member's JSON text, or "" for an answer without it.
func frameAnswer(facts string) agentcmd.AbilityRunResponse {
	raw := `{"ok":true,"mode":"precheck","valid":true,"preview":{}`
	if facts != "" {
		raw += `,"builder_facts":` + facts
	}
	return agentcmd.AbilityRunResponse{
		OK: true, Mode: agentcmd.AbilityRunModePrecheck, Valid: true, Raw: json.RawMessage(raw + `}`),
	}
}

func frameFacts(blockTheme bool, frame string) string {
	return fmt.Sprintf(`{"block_theme":%t,"page_frame":%q}`, blockTheme, frame)
}

// agentPrecheckDigest is the precheck digest the agent computes for the
// input bytes it is sent: sha256 of the JSON list of the entry hash, the
// input's sha256, the base fingerprint and the preview digest.
func agentPrecheckDigest(t *testing.T, entrySum string, input []byte, base, preview string) string {
	t.Helper()
	pre, ok := phpJSONStringArray(entrySum, sha256Hex(input), base, preview)
	if !ok {
		t.Fatal("encode the precheck digest list")
	}
	return sha256Hex(pre)
}

// withFrameMember is the AI's input with page_frame as its last member.
func withFrameMember(in []byte, frame string) []byte {
	body := bytes.TrimRight(in, " \t\r\n")
	return []byte(string(body[:len(body)-1]) + `,"page_frame":"` + frame + `"}`)
}

// TestPageFrameResolvedAndSent: a designed page is bound to the frame its
// precheck resolved. An input without page_frame is stored and sent naming
// that frame as its last member, its own bytes unchanged before it; the card
// and the presented digest bind the frame; the precheck digest stored is the
// one the site computes at the write for the bytes it is sent. A precheck
// that resolved no frame, or another than the rule gives, is refused.
func TestPageFrameResolvedAndSent(t *testing.T) {
	const entrySum = "e1a2b3"
	base, preview := "base-fingerprint", strings.Repeat("b", 64)
	heroOutline := `[` + v3Sec("top", "", v3Hero) + `,` + v3Sec("more", "", v3Para) + `]`
	hero := v3HeadInput(map[string]string{"outline": heroOutline})
	plain := v3HeadInput(nil)
	builder := &PageCardBuilder{Builder: pageBuilderElementor, Format: elementorFormatClassic, Version: "3.30.0", Layout: elementorLayoutBoxes}

	checked := func(t *testing.T, in []byte) (pageCreateFacts, checkedPrecheck) {
		t.Helper()
		f, refusal := checkPageCreateInput(in)
		if refusal.Code != "" || f.outlineVersion != pageOutlineVersionDesigned {
			t.Fatalf("not a designed page the grammar accepts: %+v", refusal)
		}
		return f, checkedPrecheck{
			precheckDigest: agentPrecheckDigest(t, entrySum, in, base, preview), previewDigest: preview,
			baseFingerprint: base, builder: builder,
		}
	}
	bind := func(t *testing.T, in []byte, facts string) ([]byte, pageCreateFacts, checkedPrecheck, *toolRefusal) {
		t.Helper()
		f, pc := checked(t, in)
		return bindPageFrame(frameAnswer(facts), entrySum, in, f, pc)
	}

	t.Run("the appended frame is the precheck's", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			in    []byte
			facts string
			frame string
		}{
			{"block theme, a hero first", hero, frameFacts(true, pageFrameCanvas), pageFrameCanvas},
			{"block theme, no hero", plain, frameFacts(true, pageFrameCanvas), pageFrameCanvas},
			{"classic theme, a hero first", hero, frameFacts(false, pageFrameFullWidth), pageFrameFullWidth},
			{"classic theme, no hero", plain, frameFacts(false, pageFrameTheme), pageFrameTheme},
		} {
			t.Run(c.name, func(t *testing.T) {
				sent, f, pc, r := bind(t, c.in, c.facts)
				if r != nil {
					t.Fatalf("refused: %+v", r.meta)
				}
				if want := withFrameMember(c.in, c.frame); !bytes.Equal(sent, want) {
					t.Fatalf("sent\n%s\nwant\n%s", sent, want)
				}
				if f.pageFrame != c.frame {
					t.Fatalf("the facts bind %q, the precheck resolved %q", f.pageFrame, c.frame)
				}
				// The site reads the frame from the bytes it is sent.
				site, refusal := checkPageCreateInput(sent)
				if refusal.Code != "" || !site.pageFrameSent || site.pageFrame != c.frame {
					t.Fatalf("the sent input names %q (sent %v, %+v)", site.pageFrame, site.pageFrameSent, refusal)
				}
				if pc.precheckDigest != agentPrecheckDigest(t, entrySum, sent, base, preview) {
					t.Fatal("the stored precheck digest is not the one the site computes for the bytes it is sent")
				}
				if pc.previewDigest != preview || pc.baseFingerprint != base || pc.builder != builder {
					t.Fatal("binding the frame changed the preview digest, the base fingerprint or the builder")
				}
			})
		}
	})

	t.Run("the digest changes with the resolved frame, the AI's bytes held fixed", func(t *testing.T) {
		auth := AuthorizedRequest{GrantID: uuid.New(), GrantName: "Laptop"}
		row := sqlc.Site{ID: uuid.New(), Name: "Site"}
		e := writeEntryRow()
		now := time.Unix(1_800_000_000, 0)
		request := func(t *testing.T, facts string) (string, []byte, string) {
			t.Helper()
			sent, f, pc, r := bind(t, bytes.Clone(hero), facts)
			if r != nil {
				t.Fatalf("refused: %+v", r.meta)
			}
			fa, err := buildAbilityRequestFacts(auth, row, "example.com", e, entrySum, sent, f, pc, now)
			if err != nil {
				t.Fatal(err)
			}
			fa.nonce = strings.Repeat("n", 64)
			_, frame, ok := storedCardDesign(fa.card)
			if !ok {
				t.Fatalf("the card is not a designed page's card: %s", fa.card)
			}
			return pageCreateDigest(auth, row, e, entrySum, sent, f, pc, fa), fa.card, frame
		}
		block, blockCard, blockFrame := request(t, frameFacts(true, pageFrameCanvas))
		classic, classicCard, classicFrame := request(t, frameFacts(false, pageFrameFullWidth))
		if blockFrame != pageFrameCanvas || classicFrame != pageFrameFullWidth {
			t.Fatalf("the cards show %q and %q; the prechecks resolved canvas and full_width", blockFrame, classicFrame)
		}
		if block == classic || bytes.Equal(blockCard, classicCard) {
			t.Fatal("two frames for the same input bytes give the same card and presented digest")
		}
		if again, _, _ := request(t, frameFacts(true, pageFrameCanvas)); again != block {
			t.Fatal("the digest is not deterministic")
		}
	})

	t.Run("a frame the AI sent is sent as it came", func(t *testing.T) {
		for _, c := range []struct {
			name        string
			sent, facts string
		}{
			{"canvas on a block theme", pageFrameCanvas, frameFacts(true, pageFrameCanvas)},
			{"theme on a classic theme, with a hero", pageFrameTheme, frameFacts(false, pageFrameTheme)},
		} {
			in := v3HeadInput(map[string]string{"outline": heroOutline, "page_frame": `"` + c.sent + `"`})
			f, pc := checked(t, in)
			sent, bound, bpc, r := bindPageFrame(frameAnswer(c.facts), entrySum, in, f, pc)
			if r != nil || !bytes.Equal(sent, in) || bound.pageFrame != c.sent || bpc.precheckDigest != pc.precheckDigest {
				t.Errorf("%s: sent %s, frame %q, refusal %v", c.name, sent, bound.pageFrame, r != nil)
			}
		}
	})

	t.Run("a precheck that resolved no frame is refused, never defaulted", func(t *testing.T) {
		themeSent := v3HeadInput(map[string]string{"outline": heroOutline, "page_frame": `"theme"`})
		for _, c := range []struct {
			name  string
			in    []byte
			facts string
		}{
			{"no builder_facts", hero, ""},
			{"builder_facts null", hero, `null`},
			{"no frame resolved", hero, `{"block_theme":false,"page_frame":null}`},
			{"no theme fact", hero, `{"page_frame":"full_width"}`},
			{"a theme fact that is not a boolean", hero, `{"block_theme":"no","page_frame":"full_width"}`},
			{"a frame the site cannot build", hero, frameFacts(false, "wide")},
			{"full_width on a block theme", hero, frameFacts(true, pageFrameFullWidth)},
			{"canvas on a classic theme the rule gives full_width", hero, frameFacts(false, pageFrameCanvas)},
			{"a block theme that accepted theme", themeSent, frameFacts(true, pageFrameTheme)},
		} {
			sent, _, _, r := bind(t, c.in, c.facts)
			if r == nil || r.meta["code"] != "precheck_unverifiable" || sent != nil {
				t.Errorf("%s: sent %s, refusal %+v", c.name, sent, r)
			}
		}
	})

	t.Run("an input that cannot carry the frame is refused", func(t *testing.T) {
		pad := abilityRunDefaultInputBytes - len(hero)
		full := append([]byte("{"+strings.Repeat(" ", pad)), hero[1:]...)
		sent, _, _, r := bind(t, full, frameFacts(true, pageFrameCanvas))
		if r == nil || sent != nil {
			t.Fatalf("an input at the size limit was sent with the frame added (%d bytes)", len(sent))
		}
		details, _ := json.Marshal(r.err.Details)
		if r.reason != reasonInvalidArguments || !strings.Contains(string(details), `"key":"page_frame"`) ||
			!strings.Contains(r.err.Message, "canvas") {
			t.Fatalf("the refusal does not name the frame the input must carry: %s %s", r.err.Message, details)
		}
		// Whitespace after the input's closing brace is not part of it.
		trailing := append(bytes.Clone(hero), strings.Repeat(" ", pad)...)
		sent, _, _, r = bind(t, trailing, frameFacts(true, pageFrameCanvas))
		if r != nil || !bytes.Equal(sent, withFrameMember(hero, pageFrameCanvas)) {
			t.Fatalf("an input at the limit by trailing whitespace: refusal %v, sent %d bytes", r != nil, len(sent))
		}
	})

	t.Run("a version 2 page is sent as it came", func(t *testing.T) {
		in := v3HeadInput(nil, "outline_version", "design")
		f, refusal := checkPageCreateInput(in)
		if refusal.Code != "" || f.outlineVersion != 0 || f.pageFrame != "" {
			t.Fatalf("not a version 2 page: %+v", refusal)
		}
		pc := checkedPrecheck{precheckDigest: agentPrecheckDigest(t, entrySum, in, base, preview), previewDigest: preview, baseFingerprint: base}
		sent, bound, bpc, r := bindPageFrame(frameAnswer(""), entrySum, in, f, pc)
		if r != nil || !bytes.Equal(sent, in) || bound.pageFrame != "" || bpc.precheckDigest != pc.precheckDigest {
			t.Fatalf("a version 2 page was changed: %s", sent)
		}
	})

	t.Run("the shared rows", func(t *testing.T) {
		b, err := os.ReadFile(pageV3CasesFixture)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Cases []struct {
				v3Case
				Site *struct {
					BlockTheme *bool `json:"block_theme"`
				} `json:"site"`
			} `json:"cases"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		var blockBuilt, blockRefused, classicHero int
		for _, c := range doc.Cases {
			if c.Agent != "ok" && c.Agent != pageFrameUnavailable {
				continue
			}
			f, refusal := checkPageCreateInput(c.body())
			if refusal.Code != "" || f.outlineVersion != pageOutlineVersionDesigned {
				continue
			}
			blockTheme := c.Site != nil && c.Site.BlockTheme != nil && *c.Site.BlockTheme
			frame, ok := resolvedPageFrame(f, blockTheme)
			switch {
			case c.Agent == pageFrameUnavailable:
				blockRefused++
				if ok || !blockTheme {
					t.Errorf("%s: the agent answers page_frame_unavailable, the control plane resolves %q", c.Name, frame)
				}
			case blockTheme:
				blockBuilt++
				if !ok || frame != pageFrameCanvas {
					t.Errorf("%s: a block theme builds canvas, the control plane resolves %q (%v)", c.Name, frame, ok)
				}
			default:
				if !ok || frame != f.pageFrame {
					t.Errorf("%s: a classic theme builds %q, the control plane resolves %q", c.Name, f.pageFrame, frame)
				}
				top, _ := jsonObjectOf(c.body())
				outline, _ := jsonArrayOf(top["outline"])
				if !f.pageFrameSent && pageOutlineHasHero(outline) {
					classicHero++
					if frame != pageFrameFullWidth {
						t.Errorf("%s: a hero first on a classic theme resolves %q, want full_width", c.Name, frame)
					}
				}
			}
			if ok {
				if got, answered := precheckPageFrame(frameAnswer(frameFacts(blockTheme, frame)).Raw, f); !answered || got != frame {
					t.Errorf("%s: the site's answer of %q is not read as that frame", c.Name, frame)
				}
			}
		}
		if blockBuilt < 2 || blockRefused < 2 || classicHero < 1 {
			t.Fatalf("judged %d block-theme rows built, %d refused, %d classic rows with a hero first; the table lost its frame rows",
				blockBuilt, blockRefused, classicHero)
		}
	})
}
