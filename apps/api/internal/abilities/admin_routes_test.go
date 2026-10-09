package abilities

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func TestMapRouteErr(t *testing.T) {
	cases := []struct {
		pg   *pgconn.PgError
		kind domain.Kind
		code string
	}{
		{&pgconn.PgError{Code: "22023", Message: "rest_route_catalogue_hash_not_moved"}, domain.KindConflict, "route_hash_not_moved"},
		{&pgconn.PgError{Code: "22023", Message: "rest_route_catalogue: create flag and route_id are required"}, domain.KindValidation, "invalid_route"},
		{&pgconn.PgError{Code: "42501"}, domain.KindForbidden, "superadmin_required"},
		{&pgconn.PgError{Code: "23505"}, domain.KindConflict, "route_conflict"},
		{&pgconn.PgError{Code: "P0002"}, domain.KindNotFound, "route_not_found"},
		{&pgconn.PgError{Code: "23514", ConstraintName: "rest_route_catalogue_read_is_get_check"}, domain.KindValidation, "invalid_route"},
	}
	for _, tc := range cases {
		de, ok := domain.AsDomain(mapRouteErr(tc.pg))
		if !ok || de.Kind != tc.kind || de.Code != tc.code {
			t.Fatalf("%s %q: got %+v", tc.pg.Code, tc.pg.Message, de)
		}
		if tc.pg.Code == "23514" && de.Details["constraint"] != tc.pg.ConstraintName {
			t.Fatalf("23514 lost its constraint name: %+v", de.Details)
		}
	}
	plain := errors.New("boom")
	if mapRouteErr(plain) != plain {
		t.Fatal("a non-database error was rewritten")
	}
}

func TestValidateRouteShape(t *testing.T) {
	if err := validateRouteShape(seededPagesUpdateFieldsRoute()); err != nil {
		t.Fatalf("the seed row was refused: %v", err)
	}
	bad := seededPagesUpdateFieldsRoute()
	bad.PathParams = []byte(`{"id":{"type":"int","min":1,"max":1e3,"required":true}}`)
	if err := validateRouteShape(bad); err == nil {
		t.Fatal("a number jsonb would re-spell was accepted")
	}
	bad = seededPagesUpdateFieldsRoute()
	bad.OutputFields = []byte(`{"fields":{"id":"float"}}`)
	if err := validateRouteShape(bad); err == nil {
		t.Fatal("an unknown output node kind was accepted")
	}
	bad.OutputFields = nil
	if err := validateRouteShape(bad); err == nil {
		t.Fatal("a route without output_fields was accepted")
	}
}

// TestRouteInputMergeMovesHash: omitted fields keep their values, and both a
// content edit and a disable move the route hash (so W1 closes old approvals).
func TestRouteInputMergeMovesHash(t *testing.T) {
	base := seededPagesUpdateFieldsRoute()
	_, before, _ := RouteBytes(base)
	var in RouteInput
	if err := json.Unmarshal([]byte(`{"enabled":false}`), &in); err != nil {
		t.Fatal(err)
	}
	merged := in.merge(base)
	if merged.Enabled || merged.Title != base.Title || string(merged.BodyKeys) != string(base.BodyKeys) {
		t.Fatalf("merge: %+v", merged)
	}
	if _, after, _ := RouteBytes(merged); after == before {
		t.Fatal("a disable did not move the route hash")
	}
	if err := json.Unmarshal([]byte(`{"title":"New title"}`), &in); err != nil {
		t.Fatal(err)
	}
	if _, after, _ := RouteBytes(RouteInput{Title: in.Title}.merge(base)); after == before {
		t.Fatal("a title edit did not move the route hash")
	}
}
