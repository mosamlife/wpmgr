package admin

// billing_contract_test.go — anti-drift guard for the admin-billing wire
// contract (M16 Phase C1 + the follow-up that added these routes to
// packages/openapi/openapi.yaml). ADR-047 keeps this whole area hand-rolled
// Gin — ogen's own server is never mounted for /admin/* (see billing_handler.go
// and handler.go) — so nothing at compile time enforces that billing_dto.go's
// hand-written structs still match the spec's AdminAccount*/AdminRevenue*/
// Admin*Request schemas. That is exactly the gap that crashed the prod
// /admin/accounts panel once already (a hand-maintained frontend interface
// drifted from billing_dto.go — see use-admin-accounts.ts's former header
// comment).
//
// This test closes the gap the mechanical way: for every response and
// request DTO in this package, it builds one FULLY POPULATED instance
// (every field set, including every optional one), marshals it to JSON, and
// decodes that JSON into the matching ogen-GENERATED type (which is derived
// straight from the spec via `go generate ./internal/api/gen/...`). It then
// re-marshals the generated type and diffs the two JSON documents.
//
//   - A renamed/retyped REQUIRED field fails at decode time (ogen tracks a
//     required-field bitset and errors "field \"x\" is required").
//   - A renamed/retyped OPTIONAL field decodes silently (unknown JSON keys
//     are skipped) but then the re-marshaled generated-type JSON is missing
//     that key, so the byte-level diff below still fails.
//
// Either way, a field-for-field mismatch between this package's DTOs and the
// spec fails `go test ./internal/admin/...` — the same signal a compiler
// would give if the handler used the generated type directly. See
// TestAdminBillingContract_NonVacuous for a demonstration that this isn't a
// no-op guard.
import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
)

// Fixture plan-tier string constants below are built from billing.Tier /
// billing.Status (never quoted literals) so this file doesn't trip
// internal/billing/grep_guard_test.go's single-ownership guard on the paid
// tier names.

// assertConforms marshals dto, decodes the payload into gen (a pointer to an
// ogen-generated type), then re-marshals gen and asserts the two JSON
// documents are semantically identical — same keys, same values.
func assertConforms(t *testing.T, name string, dto any, gen any) {
	t.Helper()
	want, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("%s: marshal hand-rolled DTO: %v", name, err)
	}
	if err := json.Unmarshal(want, gen); err != nil {
		t.Fatalf("%s: DTO payload does not decode into the ogen-generated type "+
			"— the wire contract has drifted from packages/openapi/openapi.yaml:\n%v\npayload: %s",
			name, err, want)
	}
	got, err := json.Marshal(gen)
	if err != nil {
		t.Fatalf("%s: marshal generated type: %v", name, err)
	}
	wantAny := decodeForCompare(t, name, want)
	gotAny := decodeForCompare(t, name, got)
	if !reflect.DeepEqual(wantAny, gotAny) {
		wantPretty, _ := json.MarshalIndent(wantAny, "", "  ")
		gotPretty, _ := json.MarshalIndent(gotAny, "", "  ")
		t.Fatalf("%s: DTO JSON and generated-type JSON diverge — a field was "+
			"renamed, dropped, or retyped on one side of the contract:\n"+
			"--- DTO (billing_dto.go / billing_handler.go) ---\n%s\n"+
			"--- ogen-generated (packages/openapi/openapi.yaml) ---\n%s",
			name, wantPretty, gotPretty)
	}
}

// decodeForCompare re-decodes a JSON document with UseNumber so int64 values
// compare exactly (avoids float64 precision loss on large cent amounts).
func decodeForCompare(t *testing.T, name string, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: decode JSON for comparison: %v", name, err)
	}
	return v
}

// ---------------------------------------------------------------------------
// Fully-populated fixtures — every field set, including every optional one,
// so a drift in an omitempty/pointer field is caught too.
// ---------------------------------------------------------------------------

func fixtureTime(offsetDays int) time.Time {
	return time.Date(2026, 3, 15, 9, 30, 0, 0, time.UTC).AddDate(0, 0, offsetDays)
}

func fixtureAccountTiles() AccountTiles {
	return AccountTiles{MRRCents: 458800, ActiveSubs: 71, PastDueCount: 4, AccountsTotal: 212}
}

func fixtureAccountListItem() AccountListItem {
	suspendedAt := fixtureTime(-10)
	lastActivity := fixtureTime(-1)
	return AccountListItem{
		TenantID:               uuid.New(),
		OrgName:                "Acme Web Studio",
		OrgSlug:                "acme-web-studio",
		OwnerEmail:             "owner@acme.test",
		Plan:                   string(billing.TierAgency),
		PlanStatus:             string(billing.StatusActive),
		SuspendedAt:            &suspendedAt,
		HasOverrides:           true,
		MRRCents:               5900,
		SitesUsed:              12,
		SitesCap:               25,
		StorageUsedBytesApprox: 1 << 34,
		StorageCapBytes:        1 << 40,
		NearLimit:              true,
		CreatedAt:              fixtureTime(-400),
		LastActivity:           &lastActivity,
	}
}

func fixtureAccountsResponse() AccountsResponse {
	return AccountsResponse{
		Tiles:  fixtureAccountTiles(),
		Items:  []AccountListItem{fixtureAccountListItem()},
		Total:  212,
		Limit:  50,
		Offset: 0,
	}
}

func fixtureAccountUsageMeter() AccountUsageMeter {
	return AccountUsageMeter{Used: 12, Cap: 25}
}

func fixtureAccountEntitlementValues() AccountEntitlementValues {
	return AccountEntitlementValues{
		ProbeIntervalFloorSec:     60,
		BackupCadenceFloorSeconds: 3600,
		IncrementalBackups:        true,
		ClientPortal:              true,
	}
}

func fixtureAccountUsage() AccountUsage {
	return AccountUsage{
		Sites:                    fixtureAccountUsageMeter(),
		Storage:                  AccountUsageMeter{Used: 1 << 34, Cap: 1 << 40},
		Seats:                    3,
		RestoreVolumeBytesApprox: 1 << 20,
		Entitlements:             fixtureAccountEntitlementValues(),
	}
}

func fixtureAccountSubscription() AccountSubscription {
	periodEnd := fixtureTime(15)
	graceUntil := fixtureTime(5)
	lastEvent := fixtureTime(-2)
	return AccountSubscription{
		Provider:               "stripe",
		ProviderCustomerID:     "cus_123",
		ProviderSubscriptionID: "sub_456",
		DashboardURL:           "https://dashboard.stripe.com/test/subscriptions/sub_456",
		CurrentPeriodEnd:       &periodEnd,
		CancelAtPeriodEnd:      true,
		GraceUntil:             &graceUntil,
		CompReason:             "goodwill credit",
		LastBillingEventAt:     &lastEvent,
		Stale:                  false,
	}
}

func fixtureTimelineEntry() TimelineEntry {
	return TimelineEntry{
		Source:     "audit",
		OccurredAt: fixtureTime(-3),
		Kind:       "admin.billing.suspend",
		ActorType:  "user",
		ActorID:    uuid.NewString(),
		Metadata:   map[string]any{"reason": "manual review"},
	}
}

func fixtureAccountMember() AccountMember {
	lastLogin := fixtureTime(-1)
	return AccountMember{
		ID:            uuid.New(),
		Email:         "member@acme.test",
		Name:          "Jamie Rivera",
		Role:          "owner",
		Status:        "active",
		EmailVerified: true,
		LastLoginAt:   &lastLogin,
		MemberSince:   fixtureTime(-400),
	}
}

func fixtureAccountSite() AccountSite {
	return AccountSite{
		ID:              uuid.New(),
		URL:             "https://acme.example.com",
		ConnectionState: "connected",
		CreatedAt:       fixtureTime(-200),
	}
}

func fixtureAccountDetail() AccountDetail {
	suspendedAt := fixtureTime(-10)
	return AccountDetail{
		TenantID:        uuid.New(),
		OrgName:         "Acme Web Studio",
		OrgSlug:         "acme-web-studio",
		OwnerEmail:      "owner@acme.test",
		Plan:            string(billing.TierAgency),
		PlanStatus:      string(billing.StatusActive),
		MRRCents:        5900,
		CreatedAt:       fixtureTime(-400),
		SuspendedAt:     &suspendedAt,
		SuspendedReason: "manual review",
		Usage:           fixtureAccountUsage(),
		Subscription:    fixtureAccountSubscription(),
		Timeline:        []TimelineEntry{fixtureTimelineEntry()},
		Members:         []AccountMember{fixtureAccountMember()},
		Sites:           []AccountSite{fixtureAccountSite()},
	}
}

func fixtureRevenueTiles() RevenueTiles {
	return RevenueTiles{
		MRRCents:           458800,
		MRRPastDueCents:    11800,
		ActiveSubs:         71,
		TrialingSubs:       9,
		PastDueCount:       4,
		PastDueAtRiskCents: 11800,
		NewThisMonth:       6,
		CanceledThisMonth:  2,
	}
}

func fixturePlanDistributionRow() PlanDistributionRow {
	return PlanDistributionRow{Plan: string(billing.TierAgency), Count: 40, MRRCents: 236000}
}

func fixtureCompedRow() CompedRow {
	return CompedRow{Count: 3, HypotheticalCents: 17700}
}

func fixturePastDueRevenueRow() PastDueRevenueRow {
	graceUntil := fixtureTime(5)
	lastFailed := fixtureTime(-1)
	return PastDueRevenueRow{
		TenantID:            uuid.New(),
		OrgName:             "Acme Web Studio",
		OrgSlug:             "acme-web-studio",
		OwnerEmail:          "owner@acme.test",
		AmountCents:         5900,
		DaysPastDue:         3,
		GraceUntil:          &graceUntil,
		LastPaymentFailedAt: &lastFailed,
	}
}

func fixtureRecentBillingEvent() RecentBillingEvent {
	tenantID := uuid.New()
	return RecentBillingEvent{
		ID:         uuid.New(),
		OccurredAt: fixtureTime(-1),
		OrgName:    "Acme Web Studio",
		OrgSlug:    "acme-web-studio",
		TenantID:   &tenantID,
		Kind:       "subscription_updated",
		Provider:   "stripe",
	}
}

func fixtureRevenueResponse() RevenueResponse {
	lastWebhook := fixtureTime(-1)
	return RevenueResponse{
		Tiles:                 fixtureRevenueTiles(),
		PlanDistribution:      []PlanDistributionRow{fixturePlanDistributionRow()},
		Comped:                fixtureCompedRow(),
		PastDue:               []PastDueRevenueRow{fixturePastDueRevenueRow()},
		RecentEvents:          []RecentBillingEvent{fixtureRecentBillingEvent()},
		LastWebhookReceivedAt: &lastWebhook,
	}
}

func fixtureCompRequestBody() compRequestBody {
	return compRequestBody{Tier: string(billing.TierAgency), Reason: "sales-approved goodwill comp"}
}

func fixtureReasonOnlyBody() reasonOnlyBody {
	return reasonOnlyBody{Reason: "customer requested cancellation reversal"}
}

func fixtureOverridesRequestBody() overridesRequestBody {
	sites, storageGB, seats := 5, 100, 2
	return overridesRequestBody{Sites: &sites, StorageGB: &storageGB, Seats: &seats, Reason: "temporary migration bump"}
}

func fixtureGraceRequestBody() graceRequestBody {
	return graceRequestBody{Until: fixtureTime(10).Format(time.RFC3339), Reason: "payment retry scheduled"}
}

func fixtureForceStateRequestBody() forceStateRequestBody {
	return forceStateRequestBody{Plan: string(billing.TierStarter), PlanStatus: string(billing.StatusActive), Reason: "reconciled with Stripe dashboard"}
}

// ---------------------------------------------------------------------------
// The conformance assertions — one per DTO type this package puts on the wire
// for the admin-billing surface.
// ---------------------------------------------------------------------------

func TestAdminBillingContract(t *testing.T) {
	assertConforms(t, "AccountTiles", fixtureAccountTiles(), &gen.AdminAccountTiles{})
	assertConforms(t, "AccountListItem", fixtureAccountListItem(), &gen.AdminAccountListItem{})
	assertConforms(t, "AccountsResponse", fixtureAccountsResponse(), &gen.AdminAccountsResponse{})
	assertConforms(t, "AccountUsageMeter", fixtureAccountUsageMeter(), &gen.AdminAccountUsageMeter{})
	assertConforms(t, "AccountEntitlementValues", fixtureAccountEntitlementValues(), &gen.AdminAccountEntitlementValues{})
	assertConforms(t, "AccountUsage", fixtureAccountUsage(), &gen.AdminAccountUsage{})
	assertConforms(t, "AccountSubscription", fixtureAccountSubscription(), &gen.AdminAccountSubscription{})
	assertConforms(t, "TimelineEntry", fixtureTimelineEntry(), &gen.AdminAccountTimelineEntry{})
	assertConforms(t, "AccountMember", fixtureAccountMember(), &gen.AdminAccountMember{})
	assertConforms(t, "AccountSite", fixtureAccountSite(), &gen.AdminAccountSite{})
	assertConforms(t, "AccountDetail", fixtureAccountDetail(), &gen.AdminAccountDetail{})
	assertConforms(t, "RevenueTiles", fixtureRevenueTiles(), &gen.AdminRevenueTiles{})
	assertConforms(t, "PlanDistributionRow", fixturePlanDistributionRow(), &gen.AdminPlanDistributionRow{})
	assertConforms(t, "CompedRow", fixtureCompedRow(), &gen.AdminCompedRow{})
	assertConforms(t, "PastDueRevenueRow", fixturePastDueRevenueRow(), &gen.AdminPastDueRow{})
	assertConforms(t, "RecentBillingEvent", fixtureRecentBillingEvent(), &gen.AdminRecentBillingEvent{})
	assertConforms(t, "RevenueResponse", fixtureRevenueResponse(), &gen.AdminRevenueResponse{})
	assertConforms(t, "compRequestBody", fixtureCompRequestBody(), &gen.AdminCompAccountRequest{})
	assertConforms(t, "reasonOnlyBody", fixtureReasonOnlyBody(), &gen.AdminReasonRequest{})
	assertConforms(t, "overridesRequestBody", fixtureOverridesRequestBody(), &gen.AdminSetOverridesRequest{})
	assertConforms(t, "graceRequestBody", fixtureGraceRequestBody(), &gen.AdminExtendGraceRequest{})
	assertConforms(t, "forceStateRequestBody", fixtureForceStateRequestBody(), &gen.AdminForceStateRequest{})
}

// TestAdminBillingContract_NonVacuous proves assertConforms actually detects
// drift rather than passing vacuously: it feeds a payload that is missing a
// field the spec marks required (a stand-in for "someone renamed
// AccountTiles.MRRCents on one side of the contract") and asserts the
// generated type's decode fails.
func TestAdminBillingContract_NonVacuous(t *testing.T) {
	drifted := map[string]any{
		// mrr_cents deliberately omitted — AdminAccountTiles requires it.
		"active_subs":    1,
		"past_due_count": 0,
		"accounts_total": 1,
	}
	raw, err := json.Marshal(drifted)
	if err != nil {
		t.Fatalf("marshal drifted fixture: %v", err)
	}
	var out gen.AdminAccountTiles
	err = json.Unmarshal(raw, &out)
	if err == nil {
		t.Fatal("expected a decode error for a payload missing a required field, got nil — " +
			"the contract guard would be vacuous")
	}
	t.Logf("got the expected drift-detection error: %v", err)
}
