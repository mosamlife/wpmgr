package main

import (
	"bytes"
	"strings"
	"testing"
)

// cleanEnv puts the process in the state validate-env is meant to be run in:
// a development install with a sound session secret and nothing else set, so
// each test below sees only the issues it configures.
func cleanEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WPMGR_CONFIG_FILE", "")
	t.Setenv("WPMGR_ENV", "development")
	t.Setenv("WPMGR_SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("WPMGR_HOSTED", "false")
	t.Setenv("WPMGR_PUBLIC_BASE_URL", "")
	t.Setenv("WPMGR_SOCIAL_GOOGLE_CLIENT_ID", "")
	t.Setenv("WPMGR_SOCIAL_GOOGLE_CLIENT_SECRET", "")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_ID", "")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_SECRET", "")
}

// TestValidateEnvReportsHalfConfiguredSocialProvider is the regression for
// validate-env confirming a broken configuration as fine.
//
// The command printed a fixed three-line checklist and failed only on those
// three names, so a half-entered social credential, and the missing public base
// URL that goes with it, produced three OK lines and exit zero while the server
// on the same configuration would refuse to leave degraded boot. An operator
// reading that output has every reason to stop looking.
func TestValidateEnvReportsHalfConfiguredSocialProvider(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_SOCIAL_GOOGLE_CLIENT_ID", "123.apps.googleusercontent.com")

	var out bytes.Buffer
	err := validateEnv(&out)
	if err == nil {
		t.Fatalf("validate-env must fail on a half-configured social provider; output was:\n%s", out.String())
	}
	got := out.String()
	for _, want := range []string{"WPMGR_SOCIAL_GOOGLE_CLIENT_SECRET", "WPMGR_PUBLIC_BASE_URL"} {
		if !strings.Contains(got, "FAIL  "+want) {
			t.Errorf("want a FAIL line for %s; output was:\n%s", want, got)
		}
	}
}

// TestValidateEnvReportsMissingPublicBaseURL covers the other half of the same
// blind spot: a fully configured provider whose derived redirect_uri would be a
// relative path.
func TestValidateEnvReportsMissingPublicBaseURL(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_SECRET", "gh-secret")

	var out bytes.Buffer
	err := validateEnv(&out)
	if err == nil {
		t.Fatalf("validate-env must fail when social sign-in has no public base URL; output was:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL  WPMGR_PUBLIC_BASE_URL") {
		t.Errorf("want a FAIL line for WPMGR_PUBLIC_BASE_URL; output was:\n%s", out.String())
	}
}

// TestValidateEnvPassesOnSoundConfig keeps the command usable: a correct
// install still reports OK and exits zero.
func TestValidateEnvPassesOnSoundConfig(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_PUBLIC_BASE_URL", "https://manage.example.com")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_SECRET", "gh-secret")

	var out bytes.Buffer
	if err := validateEnv(&out); err != nil {
		t.Fatalf("validate-env must pass on a sound config: %v\noutput was:\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "FAIL") {
		t.Errorf("no FAIL line expected; output was:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "OK    WPMGR_SESSION_SECRET") {
		t.Errorf("the always-shown checks must still print; output was:\n%s", out.String())
	}
}

// TestValidateEnvSurfacesIssuesOutsideTheFixedChecklist states the general rule
// the command now follows, so a future check added to config.Validate cannot
// again be dropped on the floor here.
func TestValidateEnvSurfacesIssuesOutsideTheFixedChecklist(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", "not a valid identifier")

	var out bytes.Buffer
	if err := validateEnv(&out); err == nil {
		t.Fatalf("validate-env must fail on any config.Validate issue, not only the three named ones; output was:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL  WPMGR_RIVER_MEDIA_SCHEMA") {
		t.Errorf("want a FAIL line for WPMGR_RIVER_MEDIA_SCHEMA; output was:\n%s", out.String())
	}
}
