package site

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	agentpkg "github.com/mosamlife/wpmgr/apps/api/internal/agent"
)

func boolPtr(b bool) *bool { return &b }

// Every metadata push REWRITES the whole inventory document, so builder_facts
// must be added to the payload the write path builds or the site loses it on
// the next push.
func TestInventoryPayloadCarriesBuilderFacts(t *testing.T) {
	payload := buildInventoryPayload(Metadata{
		Extras: &MetadataExtras{
			BuilderFacts: &BuilderFacts{
				V:             1,
				ThemeTemplate: "bricks",
				Elementor:     &BuilderFactsElementor{AtomicEditor: boolPtr(true)},
			},
		},
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		BuilderFacts json.RawMessage `json:"builder_facts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	const want = `{"v":1,"theme_template":"bricks","elementor":{"atomic_editor":true}}`
	if string(doc.BuilderFacts) != want {
		t.Fatalf("stored builder_facts = %s, want %s", doc.BuilderFacts, want)
	}
}

// An agent that did not report builder_facts leaves the key absent, which the
// readiness checklist reads as "not reported" and never as "off".
func TestInventoryPayloadOmitsBuilderFactsWhenUnreported(t *testing.T) {
	if _, ok := buildInventoryPayload(Metadata{})["builder_facts"]; ok {
		t.Fatal("no extras: the builder_facts key must be absent")
	}
	if _, ok := buildInventoryPayload(Metadata{Extras: &MetadataExtras{UserCount: 3}})["builder_facts"]; ok {
		t.Fatal("extras without builder facts: the builder_facts key must be absent")
	}
}

// null means "Elementor is loaded and gave no answer". The stored document
// must keep that null rather than dropping the member, so a reader can tell
// it from an absent elementor member.
func TestStoredBuilderFactsKeepsNullAtomicEditor(t *testing.T) {
	raw, err := json.Marshal(&BuilderFacts{V: 1, Elementor: &BuilderFactsElementor{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(raw), `{"v":1,"elementor":{"atomic_editor":null}}`; got != want {
		t.Fatalf("stored = %s, want %s", got, want)
	}
	raw, _ = json.Marshal(&BuilderFacts{V: 1})
	if got, want := string(raw), `{"v":1}`; got != want {
		t.Fatalf("an absent elementor member must stay absent: %s, want %s", got, want)
	}
}

// A push whose ONLY new information is builder_facts must still produce
// extras. Without this the nil check in fromAgentMetadataExtras would drop it.
func TestFromAgentMetadataExtrasKeepsBuilderFactsAlone(t *testing.T) {
	x := fromAgentMetadataExtras(agentpkg.Metadata{
		BuilderFacts: &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "bricks"},
	})
	if x == nil || x.BuilderFacts == nil {
		t.Fatalf("a builder-facts-only push was dropped: %+v", x)
	}
	if x.BuilderFacts.ThemeTemplate != "bricks" {
		t.Fatalf("theme_template lost: %+v", x.BuilderFacts)
	}
	if got := fromAgentMetadataExtras(agentpkg.Metadata{}); got != nil {
		t.Fatalf("a push with nothing optional must still give nil extras, got %+v", got)
	}
}

func TestFromAgentBuilderFactsValidatesEveryField(t *testing.T) {
	long := strings.Repeat("a", 101)
	cases := []struct {
		name      string
		in        *agentpkg.BuilderFacts
		wantNil   bool
		wantV     int
		wantTheme string
	}{
		{"nil is nil", nil, true, 0, ""},
		{"plain slug", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "bricks"}, false, 1, "bricks"},
		{"dots, underscores and hyphens", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "my_theme-2.0"}, false, 1, "my_theme-2.0"},
		{"exactly 100 characters", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: strings.Repeat("a", 100)}, false, 1, strings.Repeat("a", 100)},
		{"101 characters is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: long}, false, 1, ""},
		{"empty is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: ""}, false, 1, ""},
		{"a path is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "../bricks"}, false, 1, ""},
		{"a slash is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "a/b"}, false, 1, ""},
		{"markup is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "<b>x</b>"}, false, 1, ""},
		{"a space is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "my theme"}, false, 1, ""},
		{"a trailing newline is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "bricks\n"}, false, 1, ""},
		{"a non-ASCII letter is dropped", &agentpkg.BuilderFacts{SchemaVersion: 1, ThemeTemplate: "brícks"}, false, 1, ""},
		{"v zero is dropped", &agentpkg.BuilderFacts{SchemaVersion: 0, ThemeTemplate: "bricks"}, false, 0, "bricks"},
		{"v negative is dropped", &agentpkg.BuilderFacts{SchemaVersion: -1, ThemeTemplate: "bricks"}, false, 0, "bricks"},
		{"v above the cap is dropped", &agentpkg.BuilderFacts{SchemaVersion: 101, ThemeTemplate: "bricks"}, false, 0, "bricks"},
		{"v at the cap is kept", &agentpkg.BuilderFacts{SchemaVersion: 100}, false, 100, ""},
		{"an object with nothing usable is kept empty", &agentpkg.BuilderFacts{}, false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fromAgentBuilderFacts(c.in)
			if c.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("an object the agent sent must be stored, even when empty")
			}
			if got.V != c.wantV || got.ThemeTemplate != c.wantTheme {
				t.Fatalf("got v=%d theme=%q, want v=%d theme=%q", got.V, got.ThemeTemplate, c.wantV, c.wantTheme)
			}
		})
	}
}

func TestFromAgentBuilderFactsKeepsTheThreeAtomicAnswers(t *testing.T) {
	for _, a := range []*bool{boolPtr(true), boolPtr(false), nil} {
		got := fromAgentBuilderFacts(&agentpkg.BuilderFacts{
			SchemaVersion: 1,
			Elementor:     &agentpkg.BuilderFactsElementor{AtomicEditor: a},
		})
		if got.Elementor == nil {
			t.Fatal("the elementor member must be kept")
		}
		switch {
		case a == nil && got.Elementor.AtomicEditor != nil:
			t.Fatalf("no answer became %v", *got.Elementor.AtomicEditor)
		case a != nil && (got.Elementor.AtomicEditor == nil || *got.Elementor.AtomicEditor != *a):
			t.Fatalf("answer %v changed to %v", *a, got.Elementor.AtomicEditor)
		}
	}
	if got := fromAgentBuilderFacts(&agentpkg.BuilderFacts{SchemaVersion: 1}); got.Elementor != nil {
		t.Fatalf("an absent elementor member must stay absent, got %+v", got.Elementor)
	}
}

// The full path: an agent push carrying builder_facts reaches the stored
// components document, and a later push without them removes the key (every
// push rewrites the document, so an old agent's silence is "not reported").
func TestApplyAgentMetadataStoresBuilderFacts(t *testing.T) {
	repo := &keystoreCaptureRepo{}
	svc := newSvc(repo)
	tenantID, siteID := uuid.New(), uuid.New()

	_, err := svc.ApplyAgentMetadata(context.Background(), tenantID, siteID, agentpkg.Metadata{
		WPVersion: "7.1",
		BuilderFacts: &agentpkg.BuilderFacts{
			SchemaVersion: 1,
			ThemeTemplate: "bricks",
			Elementor:     &agentpkg.BuilderFactsElementor{AtomicEditor: boolPtr(false)},
		},
	})
	if err != nil {
		t.Fatalf("ApplyAgentMetadata: %v", err)
	}
	var stored struct {
		BuilderFacts *struct {
			V             int    `json:"v"`
			ThemeTemplate string `json:"theme_template"`
			Elementor     *struct {
				AtomicEditor *bool `json:"atomic_editor"`
			} `json:"elementor"`
		} `json:"builder_facts"`
	}
	if err := json.Unmarshal(repo.gotComponents, &stored); err != nil {
		t.Fatalf("stored components: %v", err)
	}
	bf := stored.BuilderFacts
	if bf == nil || bf.V != 1 || bf.ThemeTemplate != "bricks" ||
		bf.Elementor == nil || bf.Elementor.AtomicEditor == nil || *bf.Elementor.AtomicEditor {
		t.Fatalf("builder_facts did not reach the stored document intact: %s", repo.gotComponents)
	}

	_, err = svc.ApplyAgentMetadata(context.Background(), tenantID, siteID, agentpkg.Metadata{WPVersion: "7.1"})
	if err != nil {
		t.Fatalf("ApplyAgentMetadata (old agent): %v", err)
	}
	if strings.Contains(string(repo.gotComponents), "builder_facts") {
		t.Fatalf("a push without builder_facts must not carry a stale one: %s", repo.gotComponents)
	}
}
