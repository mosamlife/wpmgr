package domain

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// kindCases lists every Kind exactly once. HTTPStatus is the only switch over
// Kind, so this table is what proves a Kind added to the list is also mapped:
// extend it, and the upper bound in TestEveryKindIsCovered, with any new Kind.
var kindCases = []struct {
	name string
	err  *Error
	want int
}{
	{"internal", Internal("c", "m"), http.StatusInternalServerError},
	{"not found", NotFound("c", "m"), http.StatusNotFound},
	{"conflict", Conflict("c", "m"), http.StatusConflict},
	{"validation", Validation("c", "m"), http.StatusUnprocessableEntity},
	{"forbidden", Forbidden("c", "m"), http.StatusForbidden},
	{"unauthorized", Unauthorized("c", "m"), http.StatusUnauthorized},
	{"unavailable", Unavailable("c", "m"), http.StatusNotImplemented},
	{"service unavailable", ServiceUnavailable("c", "m"), http.StatusServiceUnavailable},
	{"gone", Gone("c", "m"), http.StatusGone},
	{"rate limited", RateLimited("c", "m"), http.StatusTooManyRequests},
	{"too large", TooLarge("c", "m"), http.StatusRequestEntityTooLarge},
	{"payment required", PaymentRequired("c", "m"), http.StatusPaymentRequired},
	{"bad gateway", BadGateway("c", "m"), http.StatusBadGateway},
}

func TestHTTPStatusMapsEveryKind(t *testing.T) {
	for _, tc := range kindCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HTTPStatus(tc.err); got != tc.want {
				t.Errorf("HTTPStatus = %d, want %d", got, tc.want)
			}
			// A domain error keeps its kind through the wrapping callers add.
			wrapped := fmt.Errorf("while serving: %w", tc.err)
			if got := HTTPStatus(wrapped); got != tc.want {
				t.Errorf("wrapped: HTTPStatus = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEveryKindIsCovered: the table lists each Kind from KindInternal to
// KindBadGateway once, and every Kind but KindInternal has a status of its own.
func TestEveryKindIsCovered(t *testing.T) {
	seen := map[Kind]bool{}
	for _, tc := range kindCases {
		if seen[tc.err.Kind] {
			t.Errorf("kind %d listed twice (%s)", tc.err.Kind, tc.name)
		}
		seen[tc.err.Kind] = true
		if tc.err.Kind != KindInternal && tc.want == http.StatusInternalServerError {
			t.Errorf("%s falls through to 500", tc.name)
		}
	}
	for k := KindInternal; k <= KindBadGateway; k++ {
		if !seen[k] {
			t.Errorf("kind %d has no case", k)
		}
	}
}

func TestHTTPStatusOfAnErrorThatIsNotADomainError(t *testing.T) {
	if got := HTTPStatus(errors.New("plain")); got != http.StatusInternalServerError {
		t.Fatalf("HTTPStatus = %d, want 500", got)
	}
}

func TestBadGatewayCarriesItsCodeAndMessage(t *testing.T) {
	err := BadGateway("site_refused", "The site refused the request.")
	de, ok := AsDomain(fmt.Errorf("outer: %w", err))
	if !ok || de.Kind != KindBadGateway || de.Code != "site_refused" || de.Message != "The site refused the request." {
		t.Fatalf("AsDomain = %+v, %v", de, ok)
	}
}
