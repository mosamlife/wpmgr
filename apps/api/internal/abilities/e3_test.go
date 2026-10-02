package abilities

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func TestValidateEntryShape(t *testing.T) {
	ok := []sqlc.AbilityCatalogue{
		{OutputFields: []byte(`{"fields":{"title":"string"}}`), Limits: []byte(`{"allowed_option_patterns":["_transient_builder_*"]}`)},
		{Limits: []byte(`{}`)},
	}
	for i, r := range ok {
		if err := validateEntryShape(r); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
	bad := []sqlc.AbilityCatalogue{
		{OutputFields: []byte(`{"object":{"title":"string"}}`)},
		{OutputFields: []byte(`{"fields":{"title":"number"}}`)},
		{Limits: []byte(`{"allowed_option_patterns":["*"]}`)},
		{Limits: []byte(`{"allowed_option_patterns":["ok_*","**"]}`)},
		{Limits: []byte(`{"allowed_option_patterns":[""]}`)},
		{Limits: []byte(`{"allowed_option_patterns":"*"}`)},
	}
	for i, r := range bad {
		err := validateEntryShape(r)
		if de, isDomain := domain.AsDomain(err); !isDomain || de.Kind != domain.KindValidation {
			t.Errorf("bad %d: %v", i, err)
		}
	}
}

func TestMapCatalogueErr_RangeOverlapIs409(t *testing.T) {
	err := mapCatalogueErr(&pgconn.PgError{Code: "23P01", Message: "ability_catalogue_range_overlap"})
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict || de.Code != "version_range_overlap" {
		t.Fatalf("got %v", err)
	}
}

func TestValidateInventory_C2Fields(t *testing.T) {
	out := []byte(`{"api_present":true,"count":4,"truncated":false,"abilities":[
	 {"name":"builder/a","owner_kind":"plugin","owner_mismatch":false,"version":"2.4.1","owner_dir":"builder","owner_version":"2.4.1","owner_split":false,"ability_class_ok":true},
	 {"name":"builder/b","owner_kind":"plugin","owner_mismatch":true,"version":"2.4.1","owner_dir":"builder","owner_version":"2.4.1","owner_split":true,"ability_class_ok":true},
	 {"name":"core/c","owner_kind":"core","owner_mismatch":false,"version":"7.1","owner_dir":"","owner_version":"7.1","owner_split":false,"ability_class_ok":true},
	 {"name":"old/d","owner_kind":"site","owner_mismatch":false,"version":null}]}`)
	res, err := ValidateInventory(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []InventoryRow{
		{Name: "builder/a", OwnerKind: "plugin", OwnerDir: "builder", OwnerOK: "true", OwnerVersion: "2.4.1"},
		{Name: "builder/b", OwnerKind: "plugin", OwnerDir: "builder", OwnerOK: "false", OwnerVersion: "2.4.1"},
		{Name: "core/c", OwnerKind: "core", OwnerDir: "", OwnerOK: "true", OwnerVersion: "7.1"},
		{Name: "old/d", OwnerKind: "unknown", OwnerOK: ""},
	}
	for i, w := range want {
		if res.Rows[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, res.Rows[i], w)
		}
	}
}
