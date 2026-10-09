package mcp

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ConsentScreenPath is the dashboard page that renders a consent request:
// apps/web/src/routes/_authed/connect.ai.tsx. The API does not serve it; the
// web app does, on the same origin as AuthorizePath.
const ConsentScreenPath = "/connect/ai"

// AuthorizeNavigationRedirect sends a browser that opens the advertised
// authorization_endpoint on to the consent screen.
//
// AuthorizePath has two callers. An MCP client opens it in the user's browser,
// as a top-level navigation, because it is the authorization_endpoint the
// discovery document advertises. The consent screen then fetches the same
// path as JSON to learn what it is being asked to approve. Handler.authorize
// serves the second caller only, so this middleware answers the first: a GET
// on exactly AuthorizePath that is a navigation (see isBrowserNavigation) gets
// 303 See Other to ConsentScreenPath with the request's query appended. Every
// other request, including the screen's fetch, passes through untouched to
// the route and its full gate chain.
//
// THE TARGET CANNOT BE STEERED. Location is ConsentScreenPath, a constant, plus
// the raw query exactly as received: never decoded, never re-encoded, never
// reordered. Nothing is read from Host, X-Forwarded-*, configuration or any
// query parameter. Location is a path-absolute reference, so the browser
// resolves it against the origin it actually opened, which is the advertised
// one; the query cannot change the path because the first "?" ends it.
//
// 303 See Other, never a permanent 301 or 308, and Cache-Control: no-store:
// the redirect is an answer to one request's headers, not a property of the
// URL, so nothing may keep it. Vary names the three headers the decision
// reads, on both branches, so no cache can hand the JSON answer to a
// navigation or the redirect to the fetch.
//
// It does no work a navigation could exploit: no session, no database, no
// client lookup and no ticket. The screen it leads to fetches the JSON behind
// the same checks as before.
//
// MOUNT IT ON THE ROOT ENGINE, BEFORE ANY GROUP THAT CARRIES SESSION OR AUTH
// MIDDLEWARE IS CREATED. Gin copies a parent's handlers into a group when the
// group is created, so a root middleware added after that point never runs on
// the group's routes, and on a route behind authz.RequireAuth a signed-out
// browser would be refused before reaching it. server.New mounts it that way.
func AuthorizeNavigationRedirect() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet || c.Request.URL.Path != AuthorizePath {
			c.Next()
			return
		}
		h := c.Writer.Header()
		h.Add("Vary", "Accept, Sec-Fetch-Mode, Sec-Fetch-Dest")
		if !isBrowserNavigation(c.Request.Header) {
			c.Next()
			return
		}
		target := ConsentScreenPath
		if raw := c.Request.URL.RawQuery; raw != "" {
			target += "?" + raw
		}
		h.Set("Location", target)
		h.Set("Cache-Control", "no-store")
		c.AbortWithStatus(http.StatusSeeOther)
	}
}

// isBrowserNavigation decides on positive evidence only; anything it cannot
// place is the JSON fetch, which is what the route answered before this
// middleware existed.
//
//   - Sec-Fetch-Mode, when present, decides alone: a navigation iff it is
//     "navigate". So a cors, no-cors or same-origin request is never
//     redirected, whatever its Accept says.
//   - Without it, Sec-Fetch-Dest "document" is a navigation.
//   - With no Fetch Metadata at all (an older browser, or a plain-http origin
//     the browser does not send it to), Accept decides: a navigation iff it
//     lists text/html with a quality above zero. An absent Accept, "*/*" and
//     "application/json" are all the fetch.
func isBrowserNavigation(h http.Header) bool {
	if modes := h.Values("Sec-Fetch-Mode"); len(modes) > 0 {
		return modes[0] == "navigate"
	}
	if h.Get("Sec-Fetch-Dest") == "document" {
		return true
	}
	for _, v := range h.Values("Accept") {
		for _, mediaRange := range strings.Split(v, ",") {
			mediaType, params, _ := strings.Cut(mediaRange, ";")
			if strings.EqualFold(strings.TrimSpace(mediaType), "text/html") && qualityAboveZero(params) {
				return true
			}
		}
	}
	return false
}

// qualityAboveZero reads the q parameter from a media range's parameters. An
// absent q is 1. A q that does not parse is not evidence of anything, so it
// counts as zero.
func qualityAboveZero(params string) bool {
	for _, p := range strings.Split(params, ";") {
		name, value, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && q > 0
	}
	return true
}
