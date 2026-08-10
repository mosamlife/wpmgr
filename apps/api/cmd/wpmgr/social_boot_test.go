package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/auth"
	"github.com/mosamlife/wpmgr/apps/api/internal/config"
)

// TestBootSocialConfigDegradesTheFeatureNotTheProcess is the regression for
// having made a social misconfiguration a boot-blocking one.
//
// A public base URL that cannot yield an absolute redirect_uri is a real fault
// and it does have to be handled, but the handling is: switch off the sign-in
// method that depends on it, say so, and serve everything else. The version
// this replaces added the same condition to config.Validate, which parks the
// process in serveDegraded, so an operator upgrading an install that had never
// needed WPMGR_PUBLIC_BASE_URL would have come back to a control plane that
// answers nothing but 503, for every tenant, because of a sign-in button.
func TestBootSocialConfigDegradesTheFeatureNotTheProcess(t *testing.T) {
	fullCreds := func(cfg *config.Config) {
		cfg.Social.Google.ClientID = "google-id"
		cfg.Social.Google.ClientSecret = "google-secret"
		cfg.Social.GitHub.ClientID = "gh-id"
		cfg.Social.GitHub.ClientSecret = "gh-secret"
	}

	t.Run("unusable base URL switches every provider off", func(t *testing.T) {
		for _, base := range []string{"", "manage.example.com", "/wpmgr", "   "} {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			cfg := config.Config{PublicBaseURL: base}
			fullCreds(&cfg)

			got := bootSocialConfig(cfg, logger)
			if providers := auth.NewSocialProviders(got).Enabled(); len(providers) != 0 {
				t.Errorf("base URL %q cannot produce an absolute redirect_uri, so no button may render; got providers %v", base, providers)
			}
			if !strings.Contains(buf.String(), "WPMGR_PUBLIC_BASE_URL") {
				t.Errorf("base URL %q: the operator configured a provider and it vanished, so the log must name the variable responsible; log was:\n%s", base, buf.String())
			}
		}
	})

	t.Run("usable base URL leaves the providers alone", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		cfg := config.Config{PublicBaseURL: "https://manage.example.com"}
		fullCreds(&cfg)

		got := bootSocialConfig(cfg, logger)
		if providers := auth.NewSocialProviders(got).Enabled(); len(providers) != 2 {
			t.Errorf("a sound configuration must keep both providers; got %v", providers)
		}
	})

	t.Run("no social configuration at all is silent", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		// The default install: no providers, and no public base URL either. It
		// must not be told about a requirement that does not apply to it.
		got := bootSocialConfig(config.Config{}, logger)
		if providers := auth.NewSocialProviders(got).Enabled(); len(providers) != 0 {
			t.Errorf("no credentials means no providers; got %v", providers)
		}
		if buf.Len() != 0 {
			t.Errorf("an install that never configured social sign-in must not be warned about it; log was:\n%s", buf.String())
		}
	})
}
