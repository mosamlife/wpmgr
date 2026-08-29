package config

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// publishedSecret is the value under test, taken from the denylist itself so
// the test cannot drift from the thing it is guarding.
var publishedSecret = publishedSessionSecrets[0]

// unsetEnv removes a variable for the duration of the test and restores whatever
// was there afterwards. t.Setenv registers the restore; the Unsetenv that
// follows is what actually produces the absent case, which is the one that
// matters most here — an unset WPMGR_ENV is the state every self-hosted install
// boots in, and it must never be read as a declaration of development.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

// loadWithEnv drives the real loader with the given environment. This is the
// layer that matters: a test that hands ValidateSessionSecret a Go string never
// touches mapEnvKey, koanf, or the defaults merge, and so cannot show that an
// operator's WPMGR_SESSION_SECRET actually reaches the check.
func loadWithEnv(t *testing.T, secret string, env *string) Config {
	t.Helper()
	t.Setenv("WPMGR_SESSION_SECRET", secret)
	if env == nil {
		unsetEnv(t, "WPMGR_ENV")
	} else {
		t.Setenv("WPMGR_ENV", *env)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.SessionSecret != secret {
		t.Fatalf("Auth.SessionSecret = %q, want the value from the environment %q", cfg.Auth.SessionSecret, secret)
	}
	return cfg
}

func strptr(s string) *string { return &s }

// TestLoadRefusesPublishedSessionSecret is the core guard, driven end to end
// from the environment.
//
// WPMGR_ENV is deliberately ABSENT, because that is the exact state of a
// self-hosted install: neither infra/docker-compose.yml nor
// infra/docker-compose.prod.yml sets it, so defaults() supplies "development"
// and IsProduction() is false. A production-only check would pass this case
// silently, which is why this one is not production-only.
func TestLoadRefusesPublishedSessionSecret(t *testing.T) {
	cfg := loadWithEnv(t, publishedSecret, nil)

	err := cfg.ValidateSessionSecret()
	if err == nil {
		t.Fatal("ValidateSessionSecret() = nil for a session secret published in the public repo; the api would boot on it")
	}
	msg := err.Error()
	if !strings.Contains(msg, "WPMGR_SESSION_SECRET") {
		t.Errorf("error does not name the variable an operator has to change: %q", msg)
	}
	if !strings.Contains(msg, "published") {
		t.Errorf("error does not say the secret is published, so it reads like a malformed-value complaint: %q", msg)
	}
	if !strings.Contains(msg, "openssl rand -base64 48") {
		t.Errorf("error does not tell the operator how to generate a replacement: %q", msg)
	}
	// The message is the deliverable for an operator staring at a stopped
	// container, so keep it visible in `go test -v`.
	t.Logf("operator sees: %s", msg)

	// The aggregate surface must agree; it is the one main uses to decide
	// whether to park in serveDegraded.
	issues := Validate(cfg)
	var found *Issue
	for i := range issues {
		if issues[i].Name == "WPMGR_SESSION_SECRET" {
			found = &issues[i]
		}
	}
	if found == nil {
		t.Fatal("Validate() reported no WPMGR_SESSION_SECRET issue; the boot gate would let the published secret through even though ValidateSessionSecret refuses it")
	}
	if !strings.Contains(found.Reason, "published") {
		t.Errorf("Validate() reason does not identify the secret as published: %q", found.Reason)
	}
}

// TestLoadRefusesPublishedSessionSecretInProduction covers the explicit
// production declaration.
func TestLoadRefusesPublishedSessionSecretInProduction(t *testing.T) {
	cfg := loadWithEnv(t, publishedSecret, strptr("production"))
	if err := cfg.ValidateSessionSecret(); err == nil {
		t.Fatal("ValidateSessionSecret() = nil with WPMGR_ENV=production and a published secret")
	}
}

// TestLoadRefusesPublishedSessionSecretWhenEnvIsPresentButEmpty is the
// absence-coerced-into-a-plausible-value case for the exemption itself.
// WPMGR_ENV= (present, empty) is the ordinary output of WPMGR_ENV=${SOMETHING}
// with SOMETHING unset. It is not a declaration of development and must not
// unlock the published secret.
func TestLoadRefusesPublishedSessionSecretWhenEnvIsPresentButEmpty(t *testing.T) {
	cfg := loadWithEnv(t, publishedSecret, strptr(""))
	if err := cfg.ValidateSessionSecret(); err == nil {
		t.Fatal("ValidateSessionSecret() = nil with an empty WPMGR_ENV; an unset-and-interpolated variable must not read as a development declaration")
	}
}

// TestLoadAllowsPublishedSessionSecretInDeclaredDevelopment is the OVER-FIRE
// proof, at the env layer.
//
// infra/docker-compose.dev.yml supplies BOTH the published fallback secret and
// WPMGR_ENV=development, so a zero-config `make dev` runs on a published value
// by design. If this test fails, `make dev` cannot boot, and a guard that breaks
// local development gets deleted.
//
// It must still be loud: the advisory is what keeps the exempted state visible.
func TestLoadAllowsPublishedSessionSecretInDeclaredDevelopment(t *testing.T) {
	for _, env := range []string{"development", "Development", "  development  "} {
		t.Run(env, func(t *testing.T) {
			cfg := loadWithEnv(t, publishedSecret, strptr(env))
			if err := cfg.ValidateSessionSecret(); err != nil {
				t.Fatalf("ValidateSessionSecret() = %v with WPMGR_ENV=%s; zero-config local development must still boot", err, env)
			}
			for _, is := range Validate(cfg) {
				if is.Name == "WPMGR_SESSION_SECRET" {
					t.Fatalf("Validate() returned a boot-blocking issue in declared development: %+v", is)
				}
			}
			var advised bool
			for _, is := range Advisories(cfg) {
				if is.Name == "WPMGR_SESSION_SECRET" && strings.Contains(is.Reason, "published") {
					advised = true
				}
			}
			if !advised {
				t.Error("Advisories() said nothing about running on a published session secret; the exemption would be completely silent")
			}
		})
	}
}

// TestLoadRefusesPublishedSessionSecretForNearMissEnvLabels pins the width of
// the exemption. Exactly one WPMGR_ENV spelling unlocks a published secret;
// everything that merely looks like a development label does not.
//
// These are the refusal cases for aliases the exemption once accepted. They are
// what stops the aliases creeping back: widening explicitDevelopmentEnv
// again turns each of these red. "test" and "local" are the same shape as the
// case this check exists for — a plausible label on a real box, not a statement
// that a publicly known session secret is acceptable there.
func TestLoadRefusesPublishedSessionSecretForNearMissEnvLabels(t *testing.T) {
	for _, env := range []string{"dev", "local", "test", "", "staging", "developmentt", "prod"} {
		name := env
		if name == "" {
			name = "present but empty"
		}
		t.Run(name, func(t *testing.T) {
			cfg := loadWithEnv(t, publishedSecret, strptr(env))
			if err := cfg.ValidateSessionSecret(); err == nil {
				t.Fatalf("ValidateSessionSecret() = nil with WPMGR_ENV=%q; only an explicit \"development\" may exempt a published secret", env)
			}
			var flagged bool
			for _, is := range Validate(cfg) {
				if is.Name == "WPMGR_SESSION_SECRET" {
					flagged = true
				}
			}
			if !flagged {
				t.Fatalf("Validate() reported no issue with WPMGR_ENV=%q; the boot gate disagrees with ValidateSessionSecret", env)
			}
		})
	}
}

// TestPublishedSessionSecretSpellings proves the match is on key material, not
// on one exact string. An operator who re-encoded the same bytes, whose tooling
// dropped the padding, or who pasted the decoded plaintext holds a value that is
// just as public as the original.
func TestPublishedSessionSecretSpellings(t *testing.T) {
	decoded, err := base64.StdEncoding.DecodeString(publishedSecret)
	if err != nil {
		t.Fatalf("the denylist entry is not valid std base64: %v", err)
	}
	spellings := map[string]string{
		"as published":           publishedSecret,
		"unpadded std":           base64.RawStdEncoding.EncodeToString(decoded),
		"url alphabet":           base64.URLEncoding.EncodeToString(decoded),
		"unpadded url alphabet":  base64.RawURLEncoding.EncodeToString(decoded),
		"decoded plaintext":      string(decoded),
		"trailing newline":       publishedSecret + "\n",
		"surrounding whitespace": "  " + publishedSecret + "  ",
		"plaintext with newline": string(decoded) + "\n",
	}
	for name, secret := range spellings {
		t.Run(name, func(t *testing.T) {
			cfg := loadWithEnv(t, secret, nil)
			if err := cfg.ValidateSessionSecret(); err == nil {
				t.Fatalf("ValidateSessionSecret() = nil for the published key material spelled as %s", name)
			}
		})
	}
}

// TestPublishedSessionSecretDoesNotOverFire is the other half of the guard: a
// check that reddens correct configurations gets switched off. Every value here
// is a legitimate secret and must boot with WPMGR_ENV absent.
func TestPublishedSessionSecretDoesNotOverFire(t *testing.T) {
	legitimate := map[string]string{
		// The shape `openssl rand -base64 48` produces.
		"random base64":    "9f8Ck2mVQr7tYxLpN3sHwZbA6dJeR1uOiG5kTvXyM0qPcFnB4aSlEhWjD2gZrUt8",
		"random raw bytes": strings.Repeat("Xq7", 16),
		"exactly 32 bytes": strings.Repeat("a", 32),
		"passphrase style": "correct-horse-battery-staple-and-then-some-more-entropy",
		// Decodes cleanly as base64 but to entirely different bytes: proves the
		// decoded comparison matches key material rather than "looks like base64".
		"other valid base64": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("not-the-published-value-", 3))),
	}
	for name, secret := range legitimate {
		t.Run(name, func(t *testing.T) {
			cfg := loadWithEnv(t, secret, nil)
			if err := cfg.ValidateSessionSecret(); err != nil {
				t.Fatalf("ValidateSessionSecret() = %v for a legitimate secret (%s); this guard would block correct configurations", err, name)
			}
			for _, is := range Validate(cfg) {
				if is.Name == "WPMGR_SESSION_SECRET" {
					t.Fatalf("Validate() flagged a legitimate secret (%s): %+v", name, is)
				}
			}
			for _, is := range Advisories(cfg) {
				if is.Name == "WPMGR_SESSION_SECRET" {
					t.Fatalf("Advisories() warned about a legitimate secret (%s): %+v", name, is)
				}
			}
		})
	}
}

// TestExplicitDevelopmentEnvRequiresPresence pins the exemption's core property
// directly: absence is never a declaration.
func TestExplicitDevelopmentEnvRequiresPresence(t *testing.T) {
	unsetEnv(t, "WPMGR_ENV")
	if explicitDevelopmentEnv() {
		t.Fatal("explicitDevelopmentEnv() = true with WPMGR_ENV unset; the default-to-development merge would exempt every unlabelled install")
	}
	t.Setenv("WPMGR_ENV", "development")
	if !explicitDevelopmentEnv() {
		t.Fatal("explicitDevelopmentEnv() = false with WPMGR_ENV=development; make dev cannot boot")
	}
	for _, env := range []string{"staging", "dev", "local", "test", "production", ""} {
		t.Setenv("WPMGR_ENV", env)
		if explicitDevelopmentEnv() {
			t.Fatalf("explicitDevelopmentEnv() = true for WPMGR_ENV=%q; the exemption must be exactly one spelling wide", env)
		}
	}
}
