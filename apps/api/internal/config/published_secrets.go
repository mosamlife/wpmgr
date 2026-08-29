package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
)

// publishedSessionSecrets lists WPMGR_SESSION_SECRET values that exist in this
// repository's tracked files and are therefore readable by anyone. A value here
// is not a secret in any sense: it is a shared constant that every install
// copying it holds in common, which makes every session cookie it keys forgeable
// by anyone who can read the repo.
//
// The values are written out in full, deliberately. Obfuscating or hashing them
// would make this list unusable for the one thing it exists for — letting a
// person compare it against what their own deployment actually has — and would
// hide from the next reader what these strings are. They are already public;
// repeating them here costs nothing and is what makes the check auditable.
//
// Entries are matched by isPublishedSessionSecret, which also catches the same
// bytes written under a different base64 alphabet or pasted in decoded form.
//
// Add an entry whenever a session secret is committed to a tracked file, and
// never remove one: an operator who copied it years ago is exactly the person
// this check is for.
var publishedSessionSecrets = []string{
	// infra/docker-compose.yml carried this as the api service's built-in
	// WPMGR_SESSION_SECRET fallback. Decoded, it reads
	// "dev-only-session-secret-do-not-use-in-production-48bytes".
	"ZGV2LW9ubHktc2Vzc2lvbi1zZWNyZXQtZG8tbm90LXVzZS1pbi1wcm9kdWN0aW9uLTQ4Ynl0ZXM=",
}

// base64Alphabets are the four spellings a given byte string can arrive in.
// Padding and the URL-safe alphabet are cosmetic: an operator who re-encoded the
// same bytes, or whose tooling stripped the "=", holds the identical key.
var base64Alphabets = []*base64.Encoding{
	base64.StdEncoding,
	base64.RawStdEncoding,
	base64.URLEncoding,
	base64.RawURLEncoding,
}

// base64Decodings returns every distinct byte string s decodes to across the
// four alphabets. A value that is not base64 at all yields nothing, which is the
// ordinary case for a passphrase-style secret and is not an error.
func base64Decodings(s string) [][]byte {
	var out [][]byte
	for _, enc := range base64Alphabets {
		b, err := enc.DecodeString(s)
		if err != nil || len(b) == 0 {
			continue
		}
		dup := false
		for _, seen := range out {
			if bytes.Equal(seen, b) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, b)
		}
	}
	return out
}

// isPublishedSessionSecret reports whether raw is one of the known-published
// session secrets, in any spelling that yields the same key material.
//
// Three comparisons, because a secret can arrive in three shapes:
//
//  1. Byte-for-byte the published text. This is the load-bearing one. The
//     control plane never base64-decodes WPMGR_SESSION_SECRET — it feeds the
//     string's own bytes to the handshake codec and to
//     cryptbox.DeriveAgeIdentity ([]byte(sessionSecret) in cmd/wpmgr/main.go) —
//     so the text IS the key, and an exact text match is an exact key match.
//
//  2. The same bytes under a different base64 alphabet or without padding.
//     Because the app uses the text rather than the decoding, these are not
//     literally the same key; they are, however, trivially derived from a
//     published value by anyone holding it, so they are refused too.
//
//  3. The published constant's decoded plaintext, pasted directly. Anyone who
//     ran the value through `base64 -d` to see what it was may have pasted the
//     result back, and that plaintext is just as public as the encoding.
//
// Comparison is plain equality, not constant-time: every value involved is
// already published, so there is no secret here to leak by timing.
func isPublishedSessionSecret(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	candidateDecodings := base64Decodings(s)
	for _, published := range publishedSessionSecrets {
		if s == published {
			return true // shape 1
		}
		for _, pub := range base64Decodings(published) {
			if s == string(pub) {
				return true // shape 3
			}
			for _, cand := range candidateDecodings {
				if bytes.Equal(cand, pub) {
					return true // shape 2
				}
			}
		}
	}
	return false
}

// explicitDevelopmentEnv reports whether the operator has *declared* this
// process to be a development one, by setting WPMGR_ENV to a development value
// in the environment. An absent WPMGR_ENV is not a declaration and never
// satisfies this, which is the entire point of the function.
//
// It deliberately reads the process environment rather than Config.Env. By the
// time Config exists the two cases this must separate have already been merged:
// defaults() supplies env="development" when nothing is set, so an unconfigured
// production install and a developer's laptop are the same string. Validate
// already reaches for the environment this way for WPMGR_SITE_DEST_AGE_SECRET.
func explicitDevelopmentEnv() bool {
	v, ok := os.LookupEnv("WPMGR_ENV")
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "development", "dev", "local", "test":
		return true
	default:
		return false
	}
}

// publishedSessionSecretRefusal returns the operator-facing reason the session
// secret must be refused, or "" when it is acceptable.
//
// # Why this is not gated on IsProduction, unlike ValidateAgentSigningKey
//
// The obvious shape for this check is the one ValidateAgentSigningKey uses:
// refuse the committed dev value in production, allow it everywhere else. That
// shape is wrong here, and the reason is worth stating because it is invisible
// from inside this package.
//
// Nothing in infra/docker-compose.yml or infra/docker-compose.prod.yml sets
// WPMGR_ENV. A self-hosted operator who brings the stack up therefore gets
// WPMGR_ENV unset, which defaults() turns into "development" and IsProduction()
// reads as false. Gating on IsProduction() would skip the check for precisely
// the population that has the problem — a real install, serving real sessions,
// that never declared an environment — while catching nobody else.
//
// So the refusal is universal, and the exemption is narrow: it applies only when
// the operator has explicitly said this is a development environment. Absence
// never exempts. That is the direction a wrong answer has to fail in: an
// unlabelled install is treated as real, because it usually is.
//
// The exemption exists because it has to. The dev overlay
// (infra/docker-compose.dev.yml) does not set WPMGR_SESSION_SECRET, so a
// zero-config `make dev` inherits the published fallback from the base compose
// file and would be unable to boot at all. That overlay does set
// WPMGR_ENV=development explicitly, which is the signal this keys on. A guard
// that breaks the standard local-dev bring-up gets switched off, and then it
// guards nothing.
//
// Config.Advisories still reports the exempted case, so a developer sitting on
// the published secret is told so on every boot rather than never.
func publishedSessionSecretRefusal(secret string) string {
	if !isPublishedSessionSecret(secret) {
		return ""
	}
	if explicitDevelopmentEnv() {
		return ""
	}
	return "is set to a session secret published in this project's public repository, " +
		"where it shipped as a docker-compose fallback value. It is well-formed but not " +
		"confidential: every install using it shares one session key, so anyone who has read " +
		"the repository can mint sessions here. Generate a private one — " +
		"openssl rand -base64 48 — set WPMGR_SESSION_SECRET to it, and restart. " +
		"Existing sessions will be signed out, which is the intended effect. " +
		"If this really is a development machine, set WPMGR_ENV=development"
}

// publishedSessionSecretAdvisory returns a non-fatal warning for the case
// publishedSessionSecretRefusal deliberately lets through: a declared
// development environment running on a published secret. It boots, and it says
// so every time, so the state is never silent.
func publishedSessionSecretAdvisory(secret string) string {
	if !isPublishedSessionSecret(secret) || !explicitDevelopmentEnv() {
		return ""
	}
	return "is a session secret published in this project's public repository. " +
		"Permitted here only because WPMGR_ENV declares a development environment. " +
		"Never deploy this value: generate one with openssl rand -base64 48"
}
