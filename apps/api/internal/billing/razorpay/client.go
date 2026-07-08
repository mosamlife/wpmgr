package razorpay

// client.go — a minimal, hand-rolled Razorpay REST client. Deliberately does
// NOT use github.com/razorpay/razorpay-go: every one of that SDK's request
// methods (Get/Post/Patch/...) returns (map[string]interface{}, error) —
// there is no typed-response escape hatch — so using it here would only add
// an untyped round trip in front of the typed structs below, for no benefit.
// (Its own signature-verification helpers, in the sibling "utils" package,
// were also passed over: that file is NOT test-gated — it is a plain
// production .go file that unconditionally imports "testing" and
// github.com/stretchr/testify, which would drag a test-assertion library into
// this control plane's production binary for a ~10-line HMAC compare. The
// stdlib crypto/hmac equivalent, in signature.go, is byte-for-byte the same
// algorithm.)
//
// Auth is HTTP Basic (key_id / key_secret) — https://razorpay.com/docs/api/.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// defaultBaseURL is Razorpay's production API base. Overridable (Config.BaseURL)
// for tests only.
const defaultBaseURL = "https://api.razorpay.com/v1"

// defaultHTTPTimeout bounds every Razorpay API call this adapter makes.
const defaultHTTPTimeout = 15 * time.Second

// restClient is the tiny typed-JSON HTTP client every Razorpay REST call in
// this package goes through.
type restClient struct {
	baseURL    string
	keyID      string
	keySecret  string
	httpClient *http.Client
}

func newRestClient(cfg Config) *restClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &restClient{baseURL: baseURL, keyID: cfg.KeyID, keySecret: cfg.KeySecret, httpClient: httpClient}
}

// apiError is a Razorpay API error response (https://razorpay.com/docs/errors/).
type apiError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("razorpay API error (HTTP %d, code=%s): %s", e.StatusCode, e.Code, e.Description)
}

// razorpayErrorEnvelope is Razorpay's standard error response body shape:
// {"error": {"code": "...", "description": "...", ...}}.
type razorpayErrorEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	} `json:"error"`
}

// maxResponseBodyBytes bounds every Razorpay API response read. Subscription
// and Plan entities are small (well under a few KB even fully populated);
// 1 MiB is generous headroom against a misbehaving/compromised endpoint.
const maxResponseBodyBytes = 1 << 20

// do performs one Razorpay REST call. body is JSON-marshaled as the request
// body when non-nil (nil for a GET); out is JSON-unmarshaled from a
// successful (2xx) response body when non-nil. A non-2xx response is parsed
// into *apiError (best-effort — a malformed error body still yields a usable
// status-only error).
func (c *restClient) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode razorpay request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build razorpay request: %w", err)
	}
	req.SetBasicAuth(c.keyID, c.keySecret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("razorpay request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return fmt.Errorf("read razorpay response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env razorpayErrorEnvelope
		_ = json.Unmarshal(respBody, &env) // best-effort; a non-JSON error body still yields a status-only apiError
		return &apiError{StatusCode: resp.StatusCode, Code: env.Error.Code, Description: env.Error.Description}
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode razorpay response: %w", err)
		}
	}
	return nil
}

// --- Typed request/response shapes -----------------------------------------
//
// Only the fields this adapter actually reads/writes are declared — Razorpay
// entities carry many more fields than this control plane needs.

// subscriptionCreateRequest is the POST /subscriptions body.
// https://razorpay.com/docs/api/payments/subscriptions/create-subscription/
type subscriptionCreateRequest struct {
	PlanID string `json:"plan_id"`
	// TotalCount is REQUIRED by Razorpay's Subscriptions API — there is no
	// open-ended/"until cancelled" subscription the way Stripe's Checkout
	// Session subscription mode has. subscriptionTotalCycles picks a large
	// number so the subscription is, in practice, perpetual until explicitly
	// cancelled (see that constant's doc comment).
	TotalCount int `json:"total_count"`
	// CustomerNotify is set to 0 (false): the CP owns all customer comms
	// (mirrors the Stripe adapter never delegating email to the provider).
	CustomerNotify int `json:"customer_notify"`
	// Notes carries tenant attribution (wpmgr_tenant_id) — Razorpay has no
	// checkout-session client_reference_id equivalent, so this is the ONLY
	// place a subscription is stamped with which tenant it belongs to.
	Notes map[string]string `json:"notes,omitempty"`
}

// subscriptionEntity is a Razorpay Subscription
// (https://razorpay.com/docs/api/payments/subscriptions/subscription-entity/),
// trimmed to the fields this adapter reads. current_end/current_start/
// charge_at/created_at are Unix epoch seconds.
type subscriptionEntity struct {
	ID         string            `json:"id"`
	PlanID     string            `json:"plan_id"`
	CustomerID string            `json:"customer_id"`
	Status     string            `json:"status"`
	CurrentEnd int64             `json:"current_end"`
	Notes      map[string]string `json:"notes"`
}

// planItem is the pricing sub-object of a Razorpay Plan entity.
type planItem struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// planEntity is a Razorpay Plan
// (https://razorpay.com/docs/api/payments/subscriptions/plans/), trimmed to
// the fields this adapter reads (the authoritative amount+currency for the
// Checkout.js "amount"/"currency" options — never computed/guessed CP-side).
type planEntity struct {
	ID   string   `json:"id"`
	Item planItem `json:"item"`
}

// subscriptionCancelRequest is the POST /subscriptions/:id/cancel body.
// https://razorpay.com/docs/api/payments/subscriptions/cancel-subscription/
type subscriptionCancelRequest struct {
	// CancelAtCycleEnd, when 1, schedules cancellation for the END of the
	// current billing cycle instead of immediately — the customer keeps
	// access through what they already paid for. ALWAYS 1 in this adapter
	// (see cancelSubscription): matches the state machine's existing
	// non-destructive-downgrade intent, and mirrors the Stripe adapter's own
	// cancel_at_period_end=true choice exactly.
	CancelAtCycleEnd int `json:"cancel_at_cycle_end"`
}

// subscriptionTotalCycles is the total_count Razorpay requires on every
// subscription create call. Razorpay has no concept of an open-ended
// recurring subscription (unlike Stripe's Checkout Session subscription
// mode): 1200 MONTHLY cycles is 100 years — functionally perpetual until the
// customer or CP explicitly cancels it.
const subscriptionTotalCycles = 1200

// createSubscription calls POST /subscriptions.
func (p *Provider) createSubscription(ctx context.Context, in subscriptionCreateRequest) (subscriptionEntity, error) {
	var sub subscriptionEntity
	if err := p.rest.do(ctx, http.MethodPost, "/subscriptions", in, &sub); err != nil {
		return subscriptionEntity{}, wrapErr("razorpay_subscription_create_failed", "failed to create the Razorpay subscription", err)
	}
	return sub, nil
}

// fetchSubscription calls GET /subscriptions/{id}.
func (p *Provider) fetchSubscription(ctx context.Context, id string) (subscriptionEntity, error) {
	var sub subscriptionEntity
	if err := p.rest.do(ctx, http.MethodGet, "/subscriptions/"+id, nil, &sub); err != nil {
		return subscriptionEntity{}, wrapErr("razorpay_subscription_fetch_failed", "failed to fetch the Razorpay subscription", err)
	}
	return sub, nil
}

// cancelSubscription calls POST /subscriptions/:id/cancel with
// cancel_at_cycle_end=1 (see subscriptionCancelRequest's doc comment).
func (p *Provider) cancelSubscription(ctx context.Context, id string) error {
	var sub subscriptionEntity
	if err := p.rest.do(ctx, http.MethodPost, "/subscriptions/"+id+"/cancel",
		subscriptionCancelRequest{CancelAtCycleEnd: 1}, &sub); err != nil {
		return wrapErr("razorpay_subscription_cancel_failed", "failed to cancel the Razorpay subscription", err)
	}
	return nil
}

// fetchPlan calls GET /plans/{id} — used by CreateCheckout to read the
// authoritative per-cycle amount/currency for the Checkout.js modal.
func (p *Provider) fetchPlan(ctx context.Context, id string) (planEntity, error) {
	var plan planEntity
	if err := p.rest.do(ctx, http.MethodGet, "/plans/"+id, nil, &plan); err != nil {
		return planEntity{}, wrapErr("razorpay_plan_fetch_failed", "failed to fetch the Razorpay plan", err)
	}
	return plan, nil
}

// wrapErr maps a Razorpay API/transport error to a domain error. A
// *apiError's Description is user-safe (Razorpay's own documented error
// shape); anything else (network/timeout/decode) is wrapped as an opaque
// Internal error so no transport detail leaks.
func wrapErr(code, msg string, err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return domain.Internal(code, fmt.Sprintf("%s: %s", msg, ae.Description)).WithCause(err)
	}
	return domain.Internal(code, msg).WithCause(err)
}
