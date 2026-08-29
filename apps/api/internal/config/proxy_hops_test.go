package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// baseValidCfg loads the defaults and satisfies the unrelated session-secret
// check, so a Validate result reflects only what the test varies.
func baseValidCfg(t *testing.T) Config {
	t.Helper()
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Auth.SessionSecret = strings.Repeat("a", 32)
	return cfg
}

// TestProxyHopsDefault pins the default. It is the hosted topology's value, and
// it is a default rather than a required variable because making it required
// turns the next deploy that forgets it into a boot failure.
func TestProxyHopsDefault(t *testing.T) {
	cfg := baseValidCfg(t)
	if cfg.Auth.ProxyHops != 2 {
		t.Fatalf("Auth.ProxyHops default = %d, want 2", cfg.Auth.ProxyHops)
	}
	if issues := Validate(cfg); len(issues) != 0 {
		t.Fatalf("Validate() on the default returned issues: %+v", issues)
	}
}

// TestProxyHopsFromEnv pins that the documented variable actually reaches the
// field. A koanf key that no env var maps to would leave every install on the
// default while appearing configurable — the failure shape this codebase has
// hit before with social sign-in.
func TestProxyHopsFromEnv(t *testing.T) {
	for _, want := range []int{0, 1, 3} {
		t.Setenv("WPMGR_AUTH_PROXY_HOPS", strconv.Itoa(want))
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.ProxyHops != want {
			t.Errorf("WPMGR_AUTH_PROXY_HOPS=%d gave Auth.ProxyHops=%d", want, cfg.Auth.ProxyHops)
		}
	}
}

// TestProxyHopsRawEnvStrings is the pin at the layer the coercion happens on.
//
// The earlier pins assigned already-typed ints to the struct field and covered
// "0", "1", "3" — both sit BELOW the string→int conversion, so neither could
// see that weak typing rewrote an empty variable to 0. On a load-balanced
// deployment that silently keys every caller in the fleet onto one limiter
// bucket, so the case that mattered most was the one nothing exercised.
//
// Drive Load() with raw strings, and assert refusal by spelling.
func TestProxyHopsRawEnvStrings(t *testing.T) {
	cases := []struct {
		raw     string
		want    int  // when accepted
		refused bool // Load must return an error
		why     string
	}{
		{raw: "2", want: 2},
		{raw: "0", want: 0, why: "0 is a real setting: nothing in front appends"},
		{raw: "1", want: 1},

		{raw: "", refused: true, why: "empty is not 0 — the ${UNSET} and empty-ConfigMap case"},
		{raw: " ", refused: true, why: "whitespace is not a number"},
		{raw: "2 ", refused: true, why: "trailing space"},
		{raw: " 2", refused: true, why: "leading space"},
		{raw: "2\n", refused: true, why: "trailing newline survives .env parsing"},
		{raw: "010", refused: true, why: "leading zero reads as octal in some tooling"},
		{raw: "0x3", refused: true, why: "hex form"},
		{raw: "+2", refused: true, why: "signed"},
		{raw: "-1", refused: true, why: "negative"},
		{raw: "abc", refused: true},
		{raw: "2.0", refused: true, why: "decimal point"},
		{raw: "2,3", refused: true},
	}

	for _, tc := range cases {
		t.Run(strconv.Quote(tc.raw), func(t *testing.T) {
			t.Setenv("WPMGR_AUTH_PROXY_HOPS", tc.raw)
			cfg, err := Load("")

			if tc.refused {
				if err == nil {
					t.Fatalf("Load accepted %q and produced ProxyHops=%d; it must be refused (%s)",
						tc.raw, cfg.Auth.ProxyHops, tc.why)
				}
				if !strings.Contains(err.Error(), "WPMGR_AUTH_PROXY_HOPS") {
					t.Errorf("error for %q does not name the variable: %v", tc.raw, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Load refused %q, which is a legitimate spelling (%s): %v", tc.raw, tc.why, err)
			}
			if cfg.Auth.ProxyHops != tc.want {
				t.Errorf("raw %q gave ProxyHops=%d, want %d", tc.raw, cfg.Auth.ProxyHops, tc.want)
			}
		})
	}
}

// TestProxyHopsAbsentTakesDefault pins the distinction the empty-string case
// turns on: an ABSENT variable takes the default, which is not the same as a
// present-but-empty one.
func TestProxyHopsAbsentTakesDefault(t *testing.T) {
	// t.Setenv registers cleanup; Unsetenv here gives a genuinely absent var.
	t.Setenv("WPMGR_AUTH_PROXY_HOPS", "")
	if err := os.Unsetenv("WPMGR_AUTH_PROXY_HOPS"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with the variable absent: %v", err)
	}
	if cfg.Auth.ProxyHops != 2 {
		t.Fatalf("absent variable gave ProxyHops=%d, want the default 2", cfg.Auth.ProxyHops)
	}
}

// TestProxyHopsFromConfigFile is the same pin as TestProxyHopsRawEnvStrings,
// one source over.
//
// The environment is not the only documented way in: .env.example ships
// WPMGR_CONFIG_FILE and the README documents a defaults<file<env precedence, so
// a self-hosted operator can set this key in YAML. The strict parse originally
// lived inside the environment provider's callback, which a file-supplied value
// walks straight past on its way to the same weak decode.
//
// The null shapes matter most: a bare `proxy_hops:` is what a templating tool
// renders for a value it could not resolve, which is the same accident as an
// unset ${VAR} in the environment.
func TestProxyHopsFromConfigFile(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		want    int
		refused bool
		why     string
	}{
		{name: "bare integer", yaml: "auth:\n  proxy_hops: 1\n", want: 1},
		{name: "explicit zero", yaml: "auth:\n  proxy_hops: 0\n", want: 0},
		{name: "quoted numeral", yaml: "auth:\n  proxy_hops: \"2\"\n", want: 2,
			why: "unambiguous, so parsed rather than rejected"},

		{name: "quoted empty", yaml: "auth:\n  proxy_hops: \"\"\n", refused: true,
			why: "empty is not 0"},
		{name: "bare null", yaml: "auth:\n  proxy_hops:\n", refused: true,
			why: "what a template renders for an unresolved value"},
		{name: "explicit null", yaml: "auth:\n  proxy_hops: null\n", refused: true},
		{name: "quoted octal-looking", yaml: "auth:\n  proxy_hops: \"010\"\n", refused: true},
		{name: "quoted hex", yaml: "auth:\n  proxy_hops: \"0x3\"\n", refused: true},
		{name: "boolean", yaml: "auth:\n  proxy_hops: true\n", refused: true,
			why: "weak decode turns this into 1"},
		{name: "float", yaml: "auth:\n  proxy_hops: 2.5\n", refused: true},
		{name: "list", yaml: "auth:\n  proxy_hops: [2]\n", refused: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The env var must be absent, or it would take precedence and this
			// would silently test the environment path instead.
			if err := os.Unsetenv("WPMGR_AUTH_PROXY_HOPS"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := Load(path)
			if tc.refused {
				if err == nil {
					t.Fatalf("Load accepted %q and produced ProxyHops=%d; it must be refused (%s)",
						tc.yaml, cfg.Auth.ProxyHops, tc.why)
				}
				if !strings.Contains(err.Error(), "proxy_hops") {
					t.Errorf("error does not name the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load refused %q, a legitimate spelling (%s): %v", tc.yaml, tc.why, err)
			}
			if cfg.Auth.ProxyHops != tc.want {
				t.Errorf("yaml %q gave ProxyHops=%d, want %d", tc.yaml, cfg.Auth.ProxyHops, tc.want)
			}
		})
	}
}

// TestProxyHopsEnvBeatsConfigFile pins the documented defaults<file<env
// precedence for this key, so the strict environment path cannot be bypassed by
// also having the key in a file.
func TestProxyHopsEnvBeatsConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  proxy_hops: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WPMGR_AUTH_PROXY_HOPS", "0")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.ProxyHops != 0 {
		t.Fatalf("env 0 with file 1 gave ProxyHops=%d, want 0 (env wins)", cfg.Auth.ProxyHops)
	}

	// And a bad environment value is still refused even when the file holds a
	// good one — the file must not rescue it.
	t.Setenv("WPMGR_AUTH_PROXY_HOPS", "")
	if _, err := Load(path); err == nil {
		t.Fatal("an empty env value was accepted because the file held a valid one")
	}
}

// TestProxyHopsRefusedNotClamped pins that an unusable value stops the boot
// rather than being quietly coerced into something plausible. A silently
// corrected value is the defect class this whole change exists to close.
func TestProxyHopsRefusedNotClamped(t *testing.T) {
	for _, hops := range []int{-1, maxProxyHops + 1, 100} {
		cfg := baseValidCfg(t)
		cfg.Auth.ProxyHops = hops

		issues := Validate(cfg)
		found := false
		for _, is := range issues {
			if is.Name == "WPMGR_AUTH_PROXY_HOPS" {
				found = true
			}
		}
		if !found {
			t.Errorf("ProxyHops=%d produced no issue; it must be refused, not accepted", hops)
		}
		// And it must not have been rewritten to something acceptable.
		if cfg.Auth.ProxyHops != hops {
			t.Errorf("ProxyHops was mutated from %d to %d; validation must report, not coerce",
				hops, cfg.Auth.ProxyHops)
		}
	}
}

// TestProxyHopsBoundaryValuesAccepted is the over-fire half: the check must not
// reject counts a real deployment can legitimately have.
func TestProxyHopsBoundaryValuesAccepted(t *testing.T) {
	for _, hops := range []int{0, 1, 2, maxProxyHops} {
		cfg := baseValidCfg(t)
		cfg.Auth.ProxyHops = hops
		for _, is := range Validate(cfg) {
			if is.Name == "WPMGR_AUTH_PROXY_HOPS" {
				t.Errorf("ProxyHops=%d was refused (%s); it is a legitimate deployment shape",
					hops, is.Reason)
			}
		}
	}
}
