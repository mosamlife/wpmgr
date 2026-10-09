package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// The agent writes these fixtures (apps/agent/tests/PageCreateLayoutTest.php);
// they are the contract between the two halves and are never hand-edited.
const (
	agentAbilityFixtures   = "../../../agent/tests/fixtures/ability-run/"
	pageLayoutCasesFixture = agentAbilityFixtures + "page-create-layout-cases.json"
	pageLayoutFixture      = agentAbilityFixtures + "page-create-layout.json"
	pageSchemaFixture      = agentAbilityFixtures + "page-create-schema.json"
	m162Migration          = "20261009000000_m162_page_create_layout_copy.sql"
)

type layoutCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Agent string `json:"agent"`
	Go    string `json:"go"`
}

func readLayoutCases(t *testing.T) []layoutCase {
	t.Helper()
	b, err := os.ReadFile(pageLayoutCasesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageLayoutCasesFixture, err)
	}
	var doc struct {
		Cases []layoutCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode %s: %v", pageLayoutCasesFixture, err)
	}
	// A table that reads as empty, or lost a class of case, proves nothing.
	classes := map[string]int{}
	for _, c := range doc.Cases {
		switch {
		case c.Go == "accept" && c.Agent == "ok":
			classes["accept"]++
		case c.Go == "accept":
			classes["text rule"]++
		case c.Go == "refuse":
			classes["refuse"]++
		default:
			t.Fatalf("%s: unknown go verdict %q", c.Name, c.Go)
		}
	}
	for _, k := range []string{"accept", "text rule", "refuse"} {
		if classes[k] == 0 {
			t.Fatalf("the case table has no %q case: %v", k, classes)
		}
	}
	return doc.Cases
}

// TestPageCreateLayoutCasesFixture: the control plane answers every shared
// case as the table says. A structural refusal carries the agent's own code
// (an unknown block type is the agent's create_content_invalid and the
// control plane's bad_input), and a text-rule problem passes to the agent.
func TestPageCreateLayoutCasesFixture(t *testing.T) {
	for _, c := range readLayoutCases(t) {
		input := []byte(c.Input)
		facts, code := validatePageCreateInput(input)
		switch c.Go {
		case "accept":
			if code != "" {
				t.Errorf("%s: refused %q, the table says accept", c.Name, code)
				continue
			}
			if !checkIntegersOnly(input) {
				t.Errorf("%s: the run entry's integer check refuses an accepted case", c.Name)
			}
			// The floor the run path and the worker read agrees with the
			// facts the grammar found.
			if facts.usesLayout != pageCreateUsesLayout(input) {
				t.Errorf("%s: usesLayout %v, the floor reads %v", c.Name, facts.usesLayout, pageCreateUsesLayout(input))
			}
		case "refuse":
			want := c.Agent
			if want == "create_content_invalid" {
				want = pageCreateBadInput
			}
			if code != want {
				t.Errorf("%s: code %q, want %q (agent %q)", c.Name, code, want, c.Agent)
			}
		}
	}
}

func TestPageCreateUsesLayout(t *testing.T) {
	v1 := `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"heading","level":2,"text":"H"},{"type":"list","ordered":false,"items":["a"]}]}`
	if pageCreateUsesLayout([]byte(v1)) || PageCreateAgentFloor([]byte(v1)) != agentcmd.MinAgentVersionForPageCreate {
		t.Fatal("a text-only outline needs the layout floor")
	}
	for _, in := range []string{
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"},{"type":"separator"}]}`,
		`{"post_type":"page","editor":"wordpress_classic","title":"T","outline":[{"type":"image","attachment_id":1,"alt":""}]}`,
		`not json`, `{"outline":"x"}`, `{"outline":[]}`, `{"outline":[{"type":7}]}`, `{"outline":["paragraph"]}`,
	} {
		if !pageCreateUsesLayout([]byte(in)) || PageCreateAgentFloor([]byte(in)) != agentcmd.MinAgentVersionForPageLayout {
			t.Errorf("%s: not held to the layout floor", in)
		}
	}
}

// The strict reads: encoding/json alone would take null for a string, a
// bool or a number, and a key in any letter case.
func TestValidatePageCreateInput_StrictJSONTypes(t *testing.T) {
	for _, in := range []string{
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":null}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"list","ordered":null,"items":["a"]}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"heading","level":null,"text":"x"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"Type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","Text":"x"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[null]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":null,"outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"columns","widths":[null,100],"columns":[{"children":[{"type":"separator"}]},{"children":[{"type":"separator"}]}]}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"image","attachment_id":4.2e1,"alt":""}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"table","rows":[[null]]}]}`,
	} {
		if _, code := validatePageCreateInput([]byte(in)); code == "" {
			t.Errorf("accepted %s", in)
		}
	}
}

// TestPageCreateSchemaIsTheAgentFixture: describe and an input refusal
// serve the agent's schema bytes.
func TestPageCreateSchemaIsTheAgentFixture(t *testing.T) {
	want, err := os.ReadFile(pageSchemaFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageSchemaFixture, err)
	}
	if !bytes.Equal(pageCreateInputSchemaFile, want) {
		t.Fatalf("internal/mcp/page_create_schema.json differs from %s; copy the agent's fixture over it", pageSchemaFixture)
	}
	if !bytes.Equal(pageCreateInputSchema, bytes.TrimSuffix(want, []byte("\n"))) || !json.Valid(pageCreateInputSchema) {
		t.Fatal("the served schema is not the fixture's JSON")
	}
	if !bytes.Equal(ownAbilityInputSchemas[AbilityPageCreate], pageCreateInputSchema) {
		t.Fatal("describe serves another schema for wpmgr/page-create")
	}
}

// sqlConstantText is the text of a PL/pgSQL constant written as SQL string
// literals joined with ||, as m162 writes its copy.
func sqlConstantText(t *testing.T, sql, name string) string {
	t.Helper()
	i := strings.Index(sql, name+" constant ")
	if i < 0 {
		t.Fatalf("%s: no constant %s", m162Migration, name)
	}
	rest := sql[i:]
	rest = rest[strings.Index(rest, ":=")+2:]
	var out strings.Builder
	in := false
	for j := 0; j < len(rest); j++ {
		c := rest[j]
		switch {
		case in && c == '\'' && j+1 < len(rest) && rest[j+1] == '\'':
			out.WriteByte('\'')
			j++
		case c == '\'':
			in = !in
		case in:
			out.WriteByte(c)
		case c == ';':
			return out.String()
		}
	}
	t.Fatalf("%s: constant %s is not terminated", m162Migration, name)
	return ""
}

func readM162(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m162Migration)
	if err != nil {
		t.Fatalf("read %s: %v", m162Migration, err)
	}
	return string(b)
}

// TestPageCreateM162UsageNamesTheLayoutFloor ties the floor constant to the
// release the published usage text names.
func TestPageCreateM162UsageNamesTheLayoutFloor(t *testing.T) {
	usage := sqlConstantText(t, readM162(t), "v_usage")
	if !strings.HasPrefix(usage, "Build the page as an outline.") || len(usage) > 2000 {
		t.Fatalf("m162 usage did not read back whole: %q", usage)
	}
	want := "Layout blocks need the WPMgr plugin " + agentcmd.MinAgentVersionForPageLayout + " or later on the site."
	if !strings.Contains(usage, want) {
		t.Fatalf("m162 usage does not name MinAgentVersionForPageLayout (%s): %q", agentcmd.MinAgentVersionForPageLayout, usage)
	}
	if !strings.Contains(msgAbilityLayoutOutdated, agentcmd.MinAgentVersionForPageLayout) {
		t.Fatalf("the refusal does not name the floor: %q", msgAbilityLayoutOutdated)
	}
}

// TestPageCreateLimitsMatchAgentAndM162: the grammar's limits are the
// agent's, and m162 publishes the agent's limits whole.
func TestPageCreateLimitsMatchAgentAndM162(t *testing.T) {
	b, err := os.ReadFile(pageLayoutFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageLayoutFixture, err)
	}
	var doc struct {
		Limits map[string]int64 `json:"limits"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	enforced := map[string]int64{
		"max_top_level_nodes": pageCreateMaxTopLevelNodes, "max_nodes": pageCreateMaxNodes,
		"max_columns": pageCreateMaxColumns, "max_children": pageCreateMaxChildren,
		"max_images": pageCreateMaxImages, "max_buttons": pageCreateMaxButtons, "max_tables": pageCreateMaxTables,
		"max_table_rows": pageCreateMaxTableRows, "max_table_columns": pageCreateMaxTableColumns,
		"max_input_bytes": abilityRunDefaultInputBytes,
	}
	for k, v := range enforced {
		if got, ok := doc.Limits[k]; !ok || got != v {
			t.Errorf("%s: the agent says %d (present %v), the control plane enforces %d", k, got, ok, v)
		}
	}
	var m162 map[string]int64
	if err := json.Unmarshal([]byte(sqlConstantText(t, readM162(t), "v_limits")), &m162); err != nil {
		t.Fatalf("m162 limits: %v", err)
	}
	if len(m162) != len(doc.Limits) || len(m162) == 0 {
		t.Fatalf("m162 publishes %d limits, the agent %d", len(m162), len(doc.Limits))
	}
	for k, v := range doc.Limits {
		if m162[k] != v {
			t.Errorf("%s: m162 publishes %d, the agent enforces %d", k, m162[k], v)
		}
	}
	// Describe keeps every published limit (projectLimits keeps integers).
	raw, _ := json.Marshal(m162)
	var projected map[string]int64
	_ = json.Unmarshal(projectLimits(raw), &projected)
	if len(projected) != len(m162) {
		t.Fatalf("describe drops limits: %v", projected)
	}
}

type layoutScenario struct {
	Name            string          `json:"name"`
	Input           string          `json:"input"`
	MediaIDs        []int64         `json:"media_ids"`
	BaseFingerprint string          `json:"base_fingerprint"`
	PreviewDigest   string          `json:"preview_digest"`
	PrecheckDigest  string          `json:"precheck_digest"`
	Preview         json.RawMessage `json:"preview"`
}

type layoutDoc struct {
	Entry       string           `json:"entry"`
	EntrySHA256 string           `json:"entry_sha256"`
	Cases       []layoutScenario `json:"cases"`
}

func readLayoutScenarios(t *testing.T) layoutDoc {
	t.Helper()
	b, err := os.ReadFile(pageLayoutFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageLayoutFixture, err)
	}
	var doc layoutDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	withMedia, without := 0, 0
	for _, c := range doc.Cases {
		if len(c.MediaIDs) > 0 {
			withMedia++
		} else {
			without++
		}
	}
	if withMedia == 0 || without == 0 {
		t.Fatalf("the scenario fixture needs a case with images and one without: %d / %d", withMedia, without)
	}
	return doc
}

func (c layoutScenario) response() agentcmd.AbilityRunResponse {
	return agentcmd.AbilityRunResponse{
		OK: true, Mode: agentcmd.AbilityRunModePrecheck, Valid: true, BaseFingerprint: c.BaseFingerprint,
		PreviewDigest: c.PreviewDigest, PrecheckDigest: c.PrecheckDigest, Preview: c.Preview,
	}
}

// TestPageCreateLayoutFixtureReplays: the agent's precheck answers, byte
// for byte, verify here, with the digests and the base fingerprint
// recomputed from what the control plane holds.
func TestPageCreateLayoutFixtureReplays(t *testing.T) {
	doc := readLayoutScenarios(t)
	if sha256Hex([]byte(doc.Entry)) != doc.EntrySHA256 {
		t.Fatal("entry_sha256 is not the entry's hash")
	}
	for _, c := range doc.Cases {
		input := []byte(c.Input)
		facts, code := validatePageCreateInput(input)
		if code != "" {
			t.Fatalf("%s: refused %q", c.Name, code)
		}
		if fmt.Sprint(facts.mediaIDs) != fmt.Sprint(c.MediaIDs) && !(len(facts.mediaIDs) == 0 && len(c.MediaIDs) == 0) {
			t.Fatalf("%s: media ids %v, the agent's %v", c.Name, facts.mediaIDs, c.MediaIDs)
		}
		checked, ok := verifyPageCreatePrecheck(c.response(), doc.EntrySHA256, input, facts)
		if !ok {
			t.Fatalf("%s: the agent's own precheck answer was refused", c.Name)
		}
		if len(checked.media) != len(c.MediaIDs) {
			t.Fatalf("%s: %d facts verified, want %d", c.Name, len(checked.media), len(c.MediaIDs))
		}
		card, err := pageCardFactsJSON(checked.media)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.MediaIDs) == 0 {
			if card != nil {
				t.Fatalf("%s: a page without images has card facts: %s", c.Name, card)
			}
			continue
		}
		back, ok := ReadPageCardFacts(card)
		if !ok || len(back) != len(c.MediaIDs) {
			t.Fatalf("%s: card facts do not read back: %s", c.Name, card)
		}
		for i, m := range back {
			if m.ID != c.MediaIDs[i] || m.Filename != checked.media[i].Filename || m.Mime != checked.media[i].Mime {
				t.Fatalf("%s: card image %d = %+v, fact %+v", c.Name, i, m, checked.media[i])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Image facts: a dishonest or inconsistent precheck answer is refused.
// ---------------------------------------------------------------------------

// testBaseFingerprint encodes rows as PHP's json_encode does, whatever
// their values, so a test can sign an answer the verifier must refuse.
func testBaseFingerprint(t *testing.T, postType string, media []any) string {
	t.Helper()
	pt, _ := phpJSONString(postType)
	var b strings.Builder
	b.WriteString(`["new_post",` + pt)
	if len(media) > 0 {
		b.WriteString(",[")
		for i, raw := range media {
			m := raw.(map[string]any)
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('[')
			for j, k := range []string{"id", "url", "filename", "mime", "width", "height", "modified_gmt"} {
				if j > 0 {
					b.WriteByte(',')
				}
				switch v := m[k].(type) {
				case string:
					s, _ := phpJSONString(v)
					b.WriteString(s)
				default:
					fmt.Fprint(&b, v)
				}
			}
			b.WriteByte(']')
		}
		b.WriteByte(']')
	}
	b.WriteByte(']')
	return sha256Hex([]byte(b.String()))
}

// signPrecheck builds a precheck answer for pv whose digests are all
// consistent, with base as its base fingerprint.
func signPrecheck(t *testing.T, entrySum string, input []byte, pv map[string]any, base string) agentcmd.AbilityRunResponse {
	t.Helper()
	prev, ok := phpJSONStringArray(pv["editor"].(string), pv["post_type"].(string), "draft", pv["title"].(string), pv["content"].(string))
	if !ok {
		t.Fatal("preview is not encodable")
	}
	pre, _ := phpJSONStringArray(entrySum, sha256Hex(input), base, sha256Hex(prev))
	raw, err := json.Marshal(pv)
	if err != nil {
		t.Fatal(err)
	}
	return agentcmd.AbilityRunResponse{
		OK: true, Mode: agentcmd.AbilityRunModePrecheck, Valid: true, BaseFingerprint: base,
		PreviewDigest: sha256Hex(prev), PrecheckDigest: sha256Hex(pre), Preview: raw,
	}
}

func decodePreview(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var pv map[string]any
	if err := dec.Decode(&pv); err != nil {
		t.Fatal(err)
	}
	return pv
}

func scenarioNamed(t *testing.T, doc layoutDoc, name string) layoutScenario {
	t.Helper()
	for _, c := range doc.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no scenario %q", name)
	return layoutScenario{}
}

func TestVerifyPageCreatePrecheck_ImageFacts(t *testing.T) {
	doc := readLayoutScenarios(t)
	c := scenarioNamed(t, doc, "blocks-every-node")
	input := []byte(c.Input)
	facts, code := validatePageCreateInput(input)
	if code != "" || len(facts.mediaIDs) < 3 {
		t.Fatalf("scenario: %q %v", code, facts.mediaIDs)
	}
	verify := func(resp agentcmd.AbilityRunResponse) bool {
		_, ok := verifyPageCreatePrecheck(resp, doc.EntrySHA256, input, facts)
		return ok
	}
	// resign re-signs an edited answer consistently: only the property under
	// test can refuse it.
	resign := func(edit func(pv map[string]any, media []any)) agentcmd.AbilityRunResponse {
		pv := decodePreview(t, c.Preview)
		media, _ := pv["media"].([]any)
		edit(pv, media)
		media, _ = pv["media"].([]any)
		return signPrecheck(t, doc.EntrySHA256, input, pv, testBaseFingerprint(t, pv["post_type"].(string), media))
	}
	fact := func(media []any, i int) map[string]any { return media[i].(map[string]any) }
	// swapURL changes a fact's address and the content's image tags with it.
	swapURL := func(pv map[string]any, media []any, i int, url string) {
		old := fact(media, i)["url"].(string)
		fact(media, i)["url"] = url
		pv["content"] = strings.ReplaceAll(pv["content"].(string),
			`src="`+strings.ReplaceAll(old, "&", "&amp;")+`"`, `src="`+strings.ReplaceAll(url, "&", "&amp;")+`"`)
	}

	if !verify(c.response()) || !verify(resign(func(map[string]any, []any) {})) {
		t.Fatal("positive control: the honest answer, and the same answer re-signed, must verify")
	}
	refused := map[string]agentcmd.AbilityRunResponse{}
	// The base fingerprint is recomputed from the facts: a fact the site
	// changed under an unchanged fingerprint is refused.
	edited := c.response()
	pv := decodePreview(t, c.Preview)
	fact(pv["media"].([]any), 0)["filename"] = "someone-else.jpg"
	edited.Preview, _ = json.Marshal(pv)
	prev, _ := phpJSONStringArray(pv["editor"].(string), pv["post_type"].(string), "draft", pv["title"].(string), pv["content"].(string))
	edited.PreviewDigest = sha256Hex(prev)
	pre, _ := phpJSONStringArray(doc.EntrySHA256, sha256Hex(input), edited.BaseFingerprint, edited.PreviewDigest)
	edited.PrecheckDigest = sha256Hex(pre)
	refused["fact edited under the old fingerprint"] = edited
	// The content's image tags carry exactly the facts' addresses.
	refused["fact address not in the content"] = resign(func(_ map[string]any, m []any) {
		fact(m, 0)["url"] = "https://example.com/wp-content/uploads/other.jpg"
	})
	refused["an image tag no fact names"] = resign(func(pv map[string]any, _ []any) {
		pv["content"] = pv["content"].(string) + "\n\n<!-- wp:image -->\n<figure><img src=\"https://tracker.example/p.gif\" alt=\"\" /></figure>"
	})
	refused["an upper-case image tag no fact names"] = resign(func(pv map[string]any, _ []any) {
		pv["content"] = pv["content"].(string) + "<IMG SRC=\"https://tracker.example/p.gif\">"
	})
	// The facts are the outline's ids, in order, and only those.
	refused["facts missing"] = resign(func(pv map[string]any, _ []any) { delete(pv, "media") })
	refused["facts null"] = resign(func(pv map[string]any, _ []any) { pv["media"] = nil })
	refused["facts out of order"] = resign(func(pv map[string]any, m []any) { m[0], m[1] = m[1], m[0] })
	refused["a fact dropped"] = resign(func(pv map[string]any, m []any) { pv["media"] = m[:len(m)-1] })
	refused["a fact for another id"] = resign(func(_ map[string]any, m []any) { fact(m, 2)["id"] = json.Number("10") })
	// Each fact must be usable, even when everything else is consistent.
	refused["svg"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["mime"] = "image/svg+xml" })
	refused["javascript address"] = resign(func(pv map[string]any, m []any) { swapURL(pv, m, 0, "javascript:alert(1)") })
	refused["address with a user name"] = resign(func(pv map[string]any, m []any) {
		swapURL(pv, m, 0, "https://bank.example@evil.example/x.jpg")
	})
	refused["upper-case scheme"] = resign(func(pv map[string]any, m []any) { swapURL(pv, m, 0, "HTTPS://example.com/x.jpg") })
	refused["empty file name"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["filename"] = "" })
	refused["negative width"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["width"] = json.Number("-1") })
	refused["height out of range"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["height"] = json.Number("100001") })
	refused["fractional width"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["width"] = json.Number("1200.0") })
	refused["id as text"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["id"] = "42" })
	refused["date in another shape"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["modified_gmt"] = "2026-10-01T09:00:00Z" })
	refused["an extra fact field"] = resign(func(_ map[string]any, m []any) { fact(m, 0)["path"] = "/var/www/x.jpg" })
	refused["a fact field missing"] = resign(func(_ map[string]any, m []any) { delete(fact(m, 0), "modified_gmt") })
	for name, resp := range refused {
		if verify(resp) {
			t.Errorf("%s: accepted", name)
		}
	}

	// A text-only outline: media is absent, never empty or null.
	text := scenarioNamed(t, doc, "text-only-unchanged")
	tin := []byte(text.Input)
	tf, _ := validatePageCreateInput(tin)
	if _, ok := verifyPageCreatePrecheck(text.response(), doc.EntrySHA256, tin, tf); !ok {
		t.Fatal("positive control: the text-only answer must verify")
	}
	for name, media := range map[string]any{"empty media": []any{}, "null media": nil} {
		pv := decodePreview(t, text.Preview)
		pv["media"] = media
		if _, ok := verifyPageCreatePrecheck(signPrecheck(t, doc.EntrySHA256, tin, pv, text.BaseFingerprint), doc.EntrySHA256, tin, tf); ok {
			t.Errorf("text-only with %s: accepted", name)
		}
	}
	// The no-image fingerprint is recomputed too.
	other := text.response()
	other.BaseFingerprint = strings.Repeat("a", 64)
	pre, _ = phpJSONStringArray(doc.EntrySHA256, sha256Hex(tin), other.BaseFingerprint, other.PreviewDigest)
	other.PrecheckDigest = sha256Hex(pre)
	if _, ok := verifyPageCreatePrecheck(other, doc.EntrySHA256, tin, tf); ok {
		t.Error("text-only answer with another base fingerprint: accepted")
	}
}

// ---------------------------------------------------------------------------
// What the request stores and its digest covers
// ---------------------------------------------------------------------------

func TestPageCreateRequestFacts_CardAndCopyVersion(t *testing.T) {
	auth := AuthorizedRequest{GrantID: uuid.New(), GrantName: "Laptop"}
	row := sqlc.Site{ID: uuid.New(), Name: "Site"}
	e := writeEntryRow()
	media := []pageMediaFact{{ID: 42, URL: "https://example.com/a.jpg", Filename: "team‮photo.jpg",
		Mime: "image/jpeg", Width: 1200, Height: 800, ModifiedGMT: "2026-10-01 09:00:00"}}
	layout := pageCreateFacts{postType: "page", editor: pageEditorBlocks, title: "T", usesLayout: true, mediaIDs: []int64{42}}
	f, err := buildAbilityRequestFacts(auth, row, "example.com", e, "sum", []byte(`{}`), layout,
		checkedPrecheck{media: media}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.copyVersion != AbilityCardCopyVersionLayout {
		t.Fatalf("layout copy version = %d", f.copyVersion)
	}
	got, ok := ReadPageCardFacts(f.card)
	if !ok || len(got) != 1 || got[0].ID != 42 || got[0].Width != 1200 || got[0].Mime != "image/jpeg" {
		t.Fatalf("card facts: %s", f.card)
	}
	if strings.ContainsRune(got[0].Filename, '‮') {
		t.Fatalf("site text reached the card uncleaned: %q", got[0].Filename)
	}
	text := pageCreateFacts{postType: "page", editor: pageEditorBlocks, title: "T"}
	f, err = buildAbilityRequestFacts(auth, row, "example.com", e, "sum", []byte(`{}`), text, checkedPrecheck{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.copyVersion != AbilityCardCopyVersion || f.card != nil {
		t.Fatalf("text-only: copy version %d, card %s", f.copyVersion, f.card)
	}
}

func TestPageCreateDigestCoversCardFactsAndCopyVersion(t *testing.T) {
	auth := AuthorizedRequest{GrantID: uuid.New(), GrantName: "Laptop"}
	row := sqlc.Site{ID: uuid.New(), Name: "Site"}
	e := writeEntryRow()
	facts := pageCreateFacts{postType: "page", editor: pageEditorBlocks, title: "T"}
	base := abilityRequestFacts{nonce: strings.Repeat("n", 64), copyVersion: AbilityCardCopyVersion, expiresAt: time.Unix(0, 0)}
	digest := func(f abilityRequestFacts) string {
		return pageCreateDigest(auth, row, e, "sum", []byte(`{}`), facts, checkedPrecheck{}, f)
	}
	withCard, otherCard, v2 := base, base, base
	withCard.card = []byte(`{"kind":"page_create","media":[{"id":42,"filename":"a.jpg","mime":"image/jpeg","width":1,"height":1}]}`)
	otherCard.card = []byte(`{"kind":"page_create","media":[{"id":42,"filename":"b.jpg","mime":"image/jpeg","width":1,"height":1}]}`)
	v2.copyVersion = AbilityCardCopyVersionLayout
	if digest(base) != digest(base) {
		t.Fatal("the digest is not deterministic")
	}
	seen := map[string]string{}
	for name, f := range map[string]abilityRequestFacts{"no card": base, "card": withCard, "other card": otherCard, "copy version 2": v2} {
		d := digest(f)
		if prev, dup := seen[d]; dup {
			t.Fatalf("%s and %s give the same presented digest", name, prev)
		}
		seen[d] = name
	}
}

// ---------------------------------------------------------------------------
// The run path, through the real transport: refusals before the site, the
// per-input floor, and fixed hints in place of the site's words.
// ---------------------------------------------------------------------------

// refusingPrecheckAgent answers every call with a refusal carrying site
// words that must never reach the AI.
type refusingPrecheckAgent struct {
	code  string
	calls []agentcmd.AbilityRunCall
}

func (a *refusingPrecheckAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls = append(a.calls, call)
	return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: a.code, Detail: plantedInstruction}
}

// untouchedRequestStore fails the test if the write branch reaches storage.
type untouchedRequestStore struct{ t *testing.T }

func (s untouchedRequestStore) RunAbilityRequestTx(context.Context, domain.Principal, func(pgx.Tx, abilityRequestQueries) error) error {
	s.t.Error("the request store was reached")
	return fmt.Errorf("unexpected store use")
}

func (s untouchedRequestStore) ReadAbilityRequestStatus(context.Context, domain.Principal, uuid.UUID, uuid.UUID) (AbilityStatusRow, bool, error) {
	return AbilityStatusRow{}, false, nil
}

func (s untouchedRequestStore) ListOpenAbilityRequestStatus(context.Context, domain.Principal, uuid.UUID, int32) ([]AbilityStatusRow, error) {
	return nil, nil
}

// railReadyStore is the fake store with the cache-purge request rail's
// methods, which the write switch requires and this path never calls.
type railReadyStore struct{ *fakeStore }

func (railReadyStore) ListSiteAddressesInScope(context.Context, domain.Principal) ([]siteAddressRow, error) {
	return nil, nil
}

func (railReadyStore) RunRequestTx(context.Context, domain.Principal, func(pgx.Tx, requestQueries) error) error {
	return fmt.Errorf("the cache-purge rail is not used here")
}

func (railReadyStore) ReadRequestStatus(context.Context, domain.Principal, uuid.UUID, uuid.UUID) (requestStatusRow, bool, error) {
	return requestStatusRow{}, false, nil
}

func (railReadyStore) ListOpenRequestStatus(context.Context, domain.Principal, uuid.UUID, int32) ([]requestStatusRow, error) {
	return nil, nil
}

func pageCreateRunRouter(t *testing.T, agentVersion, precheckCode string) (*gin.Engine, *refusingPrecheckAgent, uuid.UUID) {
	t.Helper()
	f := newAbilityFixture(t)
	f.store.recheck.GrantCapabilities = []string{string(CapSitesRead), string(CapAbilityRead), string(CapAbilityRequest)}
	f.store.recheck.GrantOauthScopes = []string{string(ScopeRead), string(ScopeSite)}
	f.store.sites[0].AgentVersion = agentVersion
	f.store.sites[0].ContentEditingEnabledAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	minAgent, perm := agentcmd.MinAgentVersionForPageCreate, "site.content.edit"
	entry := catalogueRow(AbilityPageCreate, "wpmgr", "write")
	entry.ApprovalMode, entry.Snapshot, entry.EffectCopy = "per_call", "created_post_trash", "draft"
	entry.OperatorPermission, entry.MinAgentVersion = &perm, &minAgent
	_, sum, _ := testEntryEncoder(entry)
	entry.EntrySha256 = &sum
	f.ab.cat = append(f.ab.cat, entry)
	f.ab.rows = append(f.ab.rows, sqlc.SiteAbilityInventory{SiteID: f.siteID, Name: AbilityPageCreate, OwnerKind: "plugin"})

	agent := &refusingPrecheckAgent{code: precheckCode}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(railReadyStore{f.store}).WithContextResolver(emptyContextResolver()).withAuditRecorder(&capturingRecorder{})
	if err := svc.EnableAbilityTools(f.ab, agent, testEntryEncoder, "test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnableAbilityWrites(untouchedRequestStore{t: t}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetWriteToolsEnabled(true); err != nil {
		t.Fatal(err)
	}
	NewTransportHandler(svc, slog.New(slog.DiscardHandler), "test-version").Register(r)
	return r, agent, f.siteID
}

type runRefusal struct {
	code    int
	msg     string
	data    map[string]any
	rawData map[string]json.RawMessage
	raw     string
}

func runPageCreate(t *testing.T, r *gin.Engine, siteID uuid.UUID, input string) runRefusal {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatal(err)
	}
	w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": siteID, "name": AbilityPageCreate, "input": in}), nil)
	resp := decodeRPC(t, w)
	if resp.Error == nil {
		t.Fatalf("not refused: %s", w.Body.String())
	}
	out := runRefusal{code: resp.Error.Code, msg: resp.Error.Message, raw: w.Body.String()}
	_ = json.Unmarshal(resp.Error.Data, &out.data)
	_ = json.Unmarshal(resp.Error.Data, &out.rawData)
	return out
}

const (
	textOnlyPage = `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"}]}`
	layoutPage   = `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"columns","columns":[{"children":[{"type":"paragraph","text":"a"}]},{"children":[{"type":"separator"}]}]}]}`
)

// The layout floor is applied per input: a layout outline on an agent below
// it is refused before the site is asked, and a text-only outline still runs
// on the entry's floor.
func TestPageCreateRun_LayoutFloorIsPerInput(t *testing.T) {
	below := "0.61.158"
	r, agent, site := pageCreateRunRouter(t, below, "create_content_invalid")
	ref := runPageCreate(t, r, site, layoutPage)
	if ref.code != codeSiteAgentOutdated || ref.data["min_agent_version"] != agentcmd.MinAgentVersionForPageLayout ||
		!strings.Contains(ref.msg, agentcmd.MinAgentVersionForPageLayout) {
		t.Fatalf("layout outline on %s: %s", below, ref.raw)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("the site was asked %d times for an outline it cannot build", len(agent.calls))
	}
	for _, v := range []string{agentcmd.MinAgentVersionForPageCreate, below} {
		r, agent, site := pageCreateRunRouter(t, v, "create_content_invalid")
		_ = runPageCreate(t, r, site, textOnlyPage)
		if len(agent.calls) != 1 || agent.calls[0].Mode != agentcmd.AbilityRunModePrecheck {
			t.Fatalf("text-only outline on %s: the precheck was not sent (%d calls)", v, len(agent.calls))
		}
	}
	r, agent, site = pageCreateRunRouter(t, agentcmd.MinAgentVersionForPageLayout, "create_content_invalid")
	_ = runPageCreate(t, r, site, layoutPage)
	if len(agent.calls) != 1 {
		t.Fatalf("layout outline at the floor: %d precheck calls, want 1", len(agent.calls))
	}
}

// Grammar refusals happen before the site is asked and carry our hint.
func TestPageCreateRun_GrammarRefusalsCarryFixedHints(t *testing.T) {
	cases := map[string]struct{ input, code, hint string }{
		"classic layout": {`{"post_type":"page","editor":"wordpress_classic","title":"T","outline":[{"type":"group","children":[{"type":"paragraph","text":"x"}]}]}`,
			pageCreateNeedsBlockEditor, hintLayoutNeedsBlockEditor},
		"one column": {`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"columns","columns":[{"children":[{"type":"paragraph","text":"x"}]}]}]}`,
			pageCreateLayoutInvalid, hintLayoutInvalid},
		"javascript link": {`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"buttons","buttons":[{"text":"Go","url":"javascript:alert(1)"}]}]}`,
			pageCreateLinkInvalid, hintLinkInvalid},
	}
	for name, c := range cases {
		r, agent, site := pageCreateRunRouter(t, agentcmd.MinAgentVersionForPageLayout, "create_content_invalid")
		ref := runPageCreate(t, r, site, c.input)
		if ref.code != codeInvalidToolArguments || ref.data["code"] != c.code || ref.data["hint"] != c.hint {
			t.Errorf("%s: %s", name, ref.raw)
		}
		if len(agent.calls) != 0 {
			t.Errorf("%s: the site was asked", name)
		}
	}
	// A block the grammar does not know is a schema mismatch: the schema
	// comes back, and it is the agent's.
	r, agent, site := pageCreateRunRouter(t, agentcmd.MinAgentVersionForPageLayout, "create_content_invalid")
	ref := runPageCreate(t, r, site, `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"html","text":"x"}]}`)
	if ref.code != codeInvalidToolArguments || !bytes.Equal(ref.rawData["schema"], pageCreateInputSchema) || len(agent.calls) != 0 {
		t.Fatalf("unknown block: %s", ref.raw)
	}
}

// A precheck refused over an image or the layout carries our hint, never
// the site's words.
func TestPageCreateRun_PrecheckRefusalHints(t *testing.T) {
	image := `{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"image","attachment_id":42,"alt":""}]}`
	for code, hint := range map[string]string{
		"image_not_available": hintImageNotAvailable, "image_url_unusable": hintImageURLUnusable,
		pageCreateLayoutInvalid: hintLayoutInvalid, pageCreateNeedsBlockEditor: hintLayoutNeedsBlockEditor,
	} {
		r, agent, site := pageCreateRunRouter(t, agentcmd.MinAgentVersionForPageLayout, code)
		ref := runPageCreate(t, r, site, image)
		if len(agent.calls) != 1 || ref.data["code"] != code || ref.data["hint"] != hint {
			t.Errorf("%s: %s", code, ref.raw)
		}
		if strings.Contains(ref.raw, plantedInstruction) {
			t.Errorf("%s: the site's words reached the AI: %s", code, ref.raw)
		}
	}
}
