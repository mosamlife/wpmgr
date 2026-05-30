// Package loginbrand implements the M14 Login Whitelabel feature: a per-site
// login brand config (logo URL, logo link, message) stored on the control plane
// and pushed to the agent via a signed `sync_login_brand` command.
package loginbrand

import (
	"time"

	"github.com/google/uuid"
)

// LoginBrand is the per-site login brand configuration. All string fields
// default to empty (""), which means "no override" on the agent side —
// WordPress's built-in login logo / default link / no message is used.
type LoginBrand struct {
	TenantID  uuid.UUID
	SiteID    uuid.UUID
	LogoURL   string
	LogoLink  string
	Message   string
	UpdatedAt time.Time
}
