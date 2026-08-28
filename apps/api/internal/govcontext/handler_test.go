package govcontext

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func bindCtx(body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("PATCH", "/api/v1/sites/x/context", strings.NewReader(body))
	return c
}

// TestBindJSON_RejectsTrailingContent is the security-review finding: the
// 1 MiB io.LimitReader in bindJSON bounds what is ever READ, but
// encoding/json's Decoder silently stops after the FIRST JSON value by
// design — a body like `{"base_version":1}<anything>` bound successfully
// with <anything> simply never inspected. Confirmed RED against the pre-fix
// bindJSON (the dec.More() check removed):
//
//	$ go test ./internal/govcontext/... -run TestBindJSON_RejectsTrailingContent -v
//	    handler_test.go:42: bindJSON("{\"base_version\":1}garbage") accepted a body with trailing content, want a rejection
//	--- FAIL: TestBindJSON_RejectsTrailingContent
//
// Restored, it is GREEN.
func TestBindJSON_RejectsTrailingContent(t *testing.T) {
	cases := []string{
		`{"base_version":1}garbage`,
		`{"base_version":1}{"base_version":2}`,
		"{\"base_version\":1}\nunexpected trailing line",
	}
	for _, body := range cases {
		var dst patchBody
		err := bindJSON(bindCtx(body), &dst)
		if err == nil {
			t.Errorf("bindJSON(%q) accepted a body with trailing content, want a rejection", body)
			continue
		}
		de, ok := domain.AsDomain(err)
		if !ok || de.Code != "invalid_body" {
			t.Errorf("bindJSON(%q) error = %v, want invalid_body", body, err)
		}
	}
}

// TestBindJSON_HonestCases_CleanBodyAndTrailingWhitespaceAreAccepted is the
// over-fire control: a well-formed body, including one with ordinary
// trailing whitespace (a trailing newline from a client that terminates its
// request bodies that way), must still bind.
func TestBindJSON_HonestCases_CleanBodyAndTrailingWhitespaceAreAccepted(t *testing.T) {
	cases := []string{
		`{"base_version":1}`,
		"{\"base_version\":1}\n",
		`{"base_version":1}   `,
	}
	for _, body := range cases {
		var dst patchBody
		if err := bindJSON(bindCtx(body), &dst); err != nil {
			t.Errorf("bindJSON(%q) rejected a well-formed body: %v", body, err)
			continue
		}
		if dst.BaseVersion == nil || *dst.BaseVersion != 1 {
			t.Errorf("bindJSON(%q) did not decode base_version correctly: %+v", body, dst)
		}
	}
}
