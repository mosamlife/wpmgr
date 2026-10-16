package elementornative

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

//go:embed pro_theme_widgets.json
var embeddedProThemeAllowlist []byte

// ProThemeWidgets returns the closed set of supported theme widget names.
func ProThemeWidgets() []string {
	return []string{"theme-site-logo", "theme-site-title", "nav-menu", "search-form"}
}

func isProThemeWidget(name string) bool {
	return slices.Contains(ProThemeWidgets(), name)
}

type proThemeAllowlist struct {
	SchemaVersion int                    `json:"schema_version"`
	Versions      []string               `json:"pairs"`
	Shared        map[string]definitions `json:"shared_controls"`
	Widgets       []widget               `json:"widgets"`
	byVersion     map[string]map[string]map[string]Control
}

var ownedProThemeAllowlist = func() *proThemeAllowlist {
	a, err := loadProThemeAllowlist(embeddedProThemeAllowlist)
	if err != nil {
		panic(fmt.Sprintf("native Pro theme control allowlist: %v", err))
	}
	return a
}()

func pairParts(token string) (elementor, pro [3]int, ok bool) {
	left, right, found := strings.Cut(token, "+")
	if !found {
		return elementor, pro, false
	}
	elementor, validElementor := versionParts(left)
	pro, validPro := versionParts(right)
	return elementor, pro, validElementor && validPro
}

// LookupProTheme returns detached controls at the qualifying pair with the
// highest Pro version, breaking ties by Elementor version. Both pins must be
// at or below the site's versions; invalid or unavailable policy admits nothing.
func LookupProTheme(name, siteElementor, siteElementorPro string) map[string]Control {
	site, ok := versionParts(siteElementor)
	sitePro, proOK := versionParts(siteElementorPro)
	if !ok || !proOK || !isProThemeWidget(name) {
		return nil
	}
	pin := ""
	var best, bestPro [3]int
	for _, token := range ownedProThemeAllowlist.Versions {
		e, p, _ := pairParts(token)
		if compareVersion(e, site) > 0 || compareVersion(p, sitePro) > 0 {
			continue
		}
		if pin == "" || compareVersion(p, bestPro) > 0 || (compareVersion(p, bestPro) == 0 && compareVersion(e, best) > 0) {
			pin, best, bestPro = token, e, p
		}
	}
	controls := ownedProThemeAllowlist.byVersion[pin][name]
	if len(controls) == 0 {
		return nil
	}
	out := make(map[string]Control, len(controls))
	for id, c := range controls {
		out[id] = cloneControl(c)
	}
	return out
}

func loadProThemeAllowlist(raw []byte) (*proThemeAllowlist, error) {
	var a proThemeAllowlist
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	if a.SchemaVersion != 1 || a.Versions == nil || a.Shared == nil || a.Widgets == nil {
		return nil, fmt.Errorf("missing schema, pairs or controls")
	}
	if (len(a.Versions) == 0 && (len(a.Widgets) != 0 || len(a.Shared) != 0)) || (len(a.Versions) > 0 && len(a.Widgets) == 0) {
		return nil, fmt.Errorf("pairs and controls must be empty together")
	}
	pairs := map[string]bool{}
	for _, pin := range a.Versions {
		if _, _, ok := pairParts(pin); !ok || pairs[pin] {
			return nil, fmt.Errorf("invalid or duplicate pair %q", pin)
		}
		pairs[pin] = true
	}
	validVersions := func(vs []string) bool {
		if len(vs) == 0 {
			return false
		}
		seen := map[string]bool{}
		for _, v := range vs {
			if !slices.Contains(a.Versions, v) || seen[v] {
				return false
			}
			seen[v] = true
		}
		return true
	}
	for id, ds := range a.Shared {
		if !controlToken.MatchString(id) || len(ds.Definitions) == 0 {
			return nil, fmt.Errorf("invalid shared control %q", id)
		}
		for _, c := range ds.Definitions {
			if !validVersions(c.Versions) || !validControl(c, 0) || !validNestedVersions(c.Fields, validVersions) {
				return nil, fmt.Errorf("invalid shared definition %q", id)
			}
		}
	}
	a.byVersion = map[string]map[string]map[string]Control{}
	for _, v := range a.Versions {
		a.byVersion[v] = map[string]map[string]Control{}
	}
	seen := map[string]bool{}
	for _, w := range a.Widgets {
		if !isProThemeWidget(w.Name) || seen[w.Name] || !validVersions(w.Versions) {
			return nil, fmt.Errorf("invalid widget %q", w.Name)
		}
		seen[w.Name] = true
		for _, v := range w.Versions {
			a.byVersion[v][w.Name] = map[string]Control{}
		}
		put := func(v, id string, c Control) error {
			m := a.byVersion[v][w.Name]
			if m == nil || m[id].Type != "" {
				return fmt.Errorf("duplicate or unavailable %s.%s at %s", w.Name, id, v)
			}
			c.ID = id
			m[id] = c
			return nil
		}
		for _, b := range w.Shared {
			if !validVersions(b.Versions) || b.Controls == nil {
				return nil, fmt.Errorf("invalid shared binding")
			}
			for id, index := range b.Controls {
				ds, ok := a.Shared[id]
				if !ok || index < 0 || index >= len(ds.Definitions) {
					return nil, fmt.Errorf("invalid shared reference %s", id)
				}
				c := ds.Definitions[index]
				for _, v := range b.Versions {
					if !slices.Contains(c.Versions, v) {
						return nil, fmt.Errorf("shared definition unavailable at %s", v)
					}
					if err := put(v, id, c); err != nil {
						return nil, err
					}
				}
			}
		}
		for _, own := range w.Controls {
			if !controlToken.MatchString(own.ID) || !validVersions(own.Versions) || len(own.Definitions) == 0 {
				return nil, fmt.Errorf("invalid widget control")
			}
			covered := map[string]bool{}
			for _, c := range own.Definitions {
				if !validVersions(c.Versions) || !validControl(c, 0) || !validNestedVersions(c.Fields, validVersions) {
					return nil, fmt.Errorf("invalid definition %s.%s", w.Name, own.ID)
				}
				for _, v := range c.Versions {
					if !slices.Contains(own.Versions, v) {
						return nil, fmt.Errorf("definition outside control versions")
					}
					covered[v] = true
					if err := put(v, own.ID, c); err != nil {
						return nil, err
					}
				}
			}
			if len(covered) != len(own.Versions) {
				return nil, fmt.Errorf("missing control definition")
			}
		}
		for v, skipped := range w.Skipped {
			if !slices.Contains(w.Versions, v) {
				return nil, fmt.Errorf("invalid skipped version")
			}
			ids := map[string]bool{}
			for _, s := range skipped {
				if !controlToken.MatchString(s.ID) || !controlToken.MatchString(s.Reason) || ids[s.ID] || a.byVersion[v][w.Name][s.ID].Type != "" {
					return nil, fmt.Errorf("invalid skipped control")
				}
				ids[s.ID] = true
			}
		}
	}
	return &a, nil
}

// Nested definitions may omit versions; any explicit list uses declared pairs.
func validNestedVersions(fields []Control, valid func([]string) bool) bool {
	for _, c := range fields {
		if (c.Versions != nil && !valid(c.Versions)) || !validNestedVersions(c.Fields, valid) {
			return false
		}
	}
	return true
}
