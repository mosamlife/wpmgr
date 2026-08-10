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

// TestValidateEnvWarnsAboutHalfConfiguredSocialProvider is the regression for
// validate-env confirming a broken configuration as fine.
//
// The command printed a fixed three-line checklist, so a half-entered social
// credential, and the unusable public base URL that goes with it, produced three
// OK lines and nothing else. An operator reading that output has every reason to
// stop looking.
//
// It reports them as WARN, and still exits zero, because that is precisely what
// the server does with them: the sign-in method is off, the control plane is
// fine. A warning that fails the operator's deploy pipeline would teach them to
// stop reading warnings.
func TestValidateEnvWarnsAboutHalfConfiguredSocialProvider(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_SOCIAL_GOOGLE_CLIENT_ID", "123.apps.googleusercontent.com")

	var out bytes.Buffer
	if err := validateEnv(&out); err != nil {
		t.Fatalf("a social misconfiguration does not stop the server, so it must not fail this command: %v\noutput was:\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"WPMGR_SOCIAL_GOOGLE_CLIENT_SECRET", "WPMGR_PUBLIC_BASE_URL"} {
		if !strings.Contains(got, "WARN  "+want) {
			t.Errorf("want a WARN line for %s; output was:\n%s", want, got)
		}
	}
	if strings.Contains(got, "FAIL") {
		t.Errorf("nothing here blocks boot, so nothing may be reported as FAIL; output was:\n%s", got)
	}
}

// TestValidateEnvWarnsAboutMissingPublicBaseURL covers the other half of the
// same blind spot: a fully configured provider whose derived redirect_uri would
// be a relative path.
func TestValidateEnvWarnsAboutMissingPublicBaseURL(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_SECRET", "gh-secret")

	var out bytes.Buffer
	if err := validateEnv(&out); err != nil {
		t.Fatalf("an unusable public base URL degrades social sign-in only: %v\noutput was:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "WARN  WPMGR_PUBLIC_BASE_URL") {
		t.Errorf("want a WARN line for WPMGR_PUBLIC_BASE_URL; output was:\n%s", out.String())
	}
}

// TestValidateEnvPassesOnSoundConfig keeps the command usable: a correct
// install still reports OK, prints no warnings, and exits zero.
func TestValidateEnvPassesOnSoundConfig(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_PUBLIC_BASE_URL", "https://manage.example.com")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_SECRET", "gh-secret")

	var out bytes.Buffer
	if err := validateEnv(&out); err != nil {
		t.Fatalf("validate-env must pass on a sound config: %v\noutput was:\n%s", err, out.String())
	}
	for _, unwanted := range []string{"FAIL", "WARN"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("no %s line expected; output was:\n%s", unwanted, out.String())
		}
	}
	if !strings.Contains(out.String(), "OK    WPMGR_SESSION_SECRET") {
		t.Errorf("the always-shown checks must still print; output was:\n%s", out.String())
	}
}

// TestValidateEnvFailsOnBootBlockingIssuesOutsideTheFixedChecklist states the
// general rule the command follows, so a future check added to config.Validate
// cannot again be dropped on the floor here.
func TestValidateEnvFailsOnBootBlockingIssuesOutsideTheFixedChecklist(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", "not a valid identifier")

	var out bytes.Buffer
	if err := validateEnv(&out); err == nil {
		t.Fatalf("this configuration parks the server in degraded boot, so the command must exit non-zero; output was:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL  WPMGR_RIVER_MEDIA_SCHEMA") {
		t.Errorf("want a FAIL line for WPMGR_RIVER_MEDIA_SCHEMA; output was:\n%s", out.String())
	}
}

// TestValidateEnvSeparatesFailFromWarn checks the two severities co-exist and
// keep their meanings: the boot-blocker fails the command, the degraded feature
// is still printed alongside it rather than being swallowed by it.
func TestValidateEnvSeparatesFailFromWarn(t *testing.T) {
	cleanEnv(t)
	t.Setenv("WPMGR_RIVER_MEDIA_SCHEMA", "not a valid identifier")
	t.Setenv("WPMGR_SOCIAL_GITHUB_CLIENT_ID", "gh-id")

	var out bytes.Buffer
	if err := validateEnv(&out); err == nil {
		t.Fatalf("a boot-blocking issue is present, so the command must exit non-zero; output was:\n%s", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "FAIL  WPMGR_RIVER_MEDIA_SCHEMA") {
		t.Errorf("want the boot-blocker as FAIL; output was:\n%s", got)
	}
	if !strings.Contains(got, "WARN  WPMGR_SOCIAL_GITHUB_CLIENT_SECRET") {
		t.Errorf("want the degraded feature as WARN; output was:\n%s", got)
	}
}
