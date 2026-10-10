package wpfatal

import (
	"strings"
	"testing"
)

// errorScreen is the document WordPress's default wp_die() handler renders
// for its critical-error screen, in a locale with no phrase-table entry.
const errorScreen = `<!DOCTYPE html>
<html lang="fr-FR">
<head><meta charset="utf-8"><title>WordPress &rsaquo; Erreur</title></head>
<body id="error-page">
	<div class="wp-die-message"><p>Une erreur critique est survenue sur ce site.</p></div></body>
</html>`

// dbErrorScreen is the database connection failure screen.
const dbErrorScreen = `<!DOCTYPE html>
<html>
<head><title>Database Error</title></head>
<body>
<h1>Error establishing a database connection</h1>
</body>
</html>`

const html = "text/html; charset=UTF-8"

func TestScan_RecognisesWordPressErrorDocuments(t *testing.T) {
	pageOutput := `<html><head><title>Example</title></head><body class="home">` +
		strings.Repeat(`<div class="wp-block-group"><p>Recent posts and notes.</p></div>`+"\n", 2000)
	cases := []struct {
		name       string
		body       string
		wantReason string
	}{
		{"critical-error screen", errorScreen, ReasonFatalError},
		{"critical-error screen after page output", pageOutput + errorScreen, ReasonFatalError},
		{"database connection screen", dbErrorScreen, ReasonDBError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, reason := Scan([]byte(tc.body), html)
			if !found || reason != tc.wantReason {
				t.Fatalf("Scan = (%v, %q), want (true, %q)", found, reason, tc.wantReason)
			}
		})
	}
}

func TestScan_HealthyPagesAreNotRecognised(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"themed page", `<html><head><title>Example</title></head><body class="home page"><main><p>Welcome.</p></main></body></html>`},
		{"page about the error screen", `<html><head><title>Support - Example</title></head><body class="single">` +
			`<h1>There has been a critical error on this website: what it means</h1>` +
			`<pre><code>&lt;body id="error-page"&gt;&lt;div class="wp-die-message"&gt;</code></pre></body></html>`},
		{"page about database errors", `<html><head><title>Error establishing a database connection - Example</title></head>` +
			`<body><h1>Error establishing a database connection</h1></body></html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if found, reason := Scan([]byte(tc.body), html); found {
				t.Fatalf("Scan = (true, %q), want a healthy page left unrecognised", reason)
			}
		})
	}
}

func TestScan_OnlyHTMLIsScanned(t *testing.T) {
	for _, ct := range []string{"application/json", "text/plain", ""} {
		if found, reason := Scan([]byte(errorScreen), ct); found {
			t.Fatalf("Content-Type %q: Scan = (true, %q), want false", ct, reason)
		}
	}
	if found, _ := Scan([]byte(errorScreen), "TEXT/HTML; charset=utf-8"); !found {
		t.Fatal("Content-Type matching must ignore case and parameters")
	}
}
