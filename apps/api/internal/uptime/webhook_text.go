package uptime

import (
	"fmt"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/webhooktext"
)

func renderWebhookText(p WebhookPayload) string {
	switch p.Event {
	case "uptime.app_down_aggregate":
		return fmt.Sprintf("Application health issue: %d of %d sites are down", p.AppDownCount, p.AppEligibleCount)
	case "uptime.app_down_aggregate_update":
		return fmt.Sprintf("Application health issue worsened: %d of %d sites are down", p.AppDownCount, p.AppEligibleCount)
	case "uptime.app_recovery_aggregate":
		return fmt.Sprintf("✅ Fleet application health recovered: %d of %d sites are down", p.AppDownCount, p.AppEligibleCount)
	}
	name, url := webhooktext.Value(p.SiteName), webhooktext.Value(p.SiteURL)
	if name == "" {
		name = url
	} else if url != "" {
		name += " (" + url + ")"
	}
	if name == "" {
		name = webhooktext.Value(p.SiteID)
	}
	detail := webhooktext.Value(p.Error)
	if strings.HasPrefix(p.Event, "security.") {
		if detail == "" {
			detail = webhooktext.Value(strings.TrimPrefix(p.Event, "security."))
		}
		return "Security event on " + name + ": " + detail
	}
	switch p.Event {
	case "uptime.recovery":
		return "✅ " + name + " is back up"
	case "uptime.app_recovery":
		return "✅ " + name + " application is back up"
	case "uptime.app_down":
		return name + " application is down: " + appProbeReasonCopy(detail)
	default:
		switch p.Error {
		case "wp_fatal_error":
			detail = "WordPress critical error"
		case "wp_db_error":
			detail = "database connection error"
		}
		if detail == "" && p.HTTPStatus > 0 {
			detail = fmt.Sprintf("HTTP %d", p.HTTPStatus)
		}
		text := name + " is down"
		if detail != "" {
			text += ": " + detail
		}
		return text
	}
}
