// Package elementornative owns the version-pinned ceiling for native controls.
package elementornative

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

//go:embed core_widgets.json
var embeddedAllowlist []byte

// Control is an owned definition; a site's live definition may only narrow it.
// Units use the policy's closed grammar because the dump does not export units.
type Control struct {
	ID         string    `json:"id,omitempty"`
	Type       string    `json:"type"`
	Options    []string  `json:"options"`
	Responsive *Window   `json:"responsive"`
	Group      *Group    `json:"group"`
	Fields     []Control `json:"fields"`
	Versions   []string  `json:"versions,omitempty"`
}

type Window struct {
	Min     *string  `json:"min"`
	Max     *string  `json:"max"`
	Devices []string `json:"devices"`
}

type Group struct {
	Family    string  `json:"family"`
	Activator *string `json:"activator"`
}

type definitions struct {
	Definitions []Control `json:"definitions"`
}
type ownControl struct {
	ID          string    `json:"id"`
	Versions    []string  `json:"versions"`
	Definitions []Control `json:"definitions"`
}
type sharedBinding struct {
	Versions []string       `json:"versions"`
	Controls map[string]int `json:"controls"`
}
type skippedControl struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type widget struct {
	Name     string                      `json:"name"`
	Versions []string                    `json:"versions"`
	Shared   []sharedBinding             `json:"shared_controls"`
	Skipped  map[string][]skippedControl `json:"controls_skipped"`
	Controls []ownControl                `json:"controls"`
}
type allowlist struct {
	SchemaVersion int                    `json:"schema_version"`
	Versions      []string               `json:"elementor_versions"`
	Shared        map[string]definitions `json:"shared_controls"`
	Widgets       []widget               `json:"widgets"`
	byVersion     map[string]map[string]map[string]Control
}

var ownedAllowlist = func() *allowlist {
	a, err := loadAllowlist(embeddedAllowlist)
	if err != nil {
		panic(fmt.Sprintf("native control allowlist: %v", err))
	}
	return a
}()

var versionPattern = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)
var controlToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func versionParts(v string) ([3]int, bool) {
	var p [3]int
	if !versionPattern.MatchString(v) {
		return p, false
	}
	for i, part := range strings.Split(v, ".") {
		p[i], _ = strconv.Atoi(part)
	}
	return p, true
}

func compareVersion(a, b [3]int) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// Lookup returns a detached control map at the highest pin at or below the
// site version. Invalid/older versions and absent widgets authorise nothing.
func Lookup(name, siteVersion string) map[string]Control {
	site, ok := versionParts(siteVersion)
	if !ok {
		return nil
	}
	pin := ""
	for _, v := range ownedAllowlist.Versions {
		p, _ := versionParts(v)
		if compareVersion(p, site) <= 0 {
			pin = v
		}
	}
	if pin == "" {
		return nil
	}
	controls := ownedAllowlist.byVersion[pin][name]
	if controls == nil {
		return nil
	}
	out := make(map[string]Control, len(controls))
	for key, c := range controls {
		out[key] = cloneControl(c)
	}
	return out
}

func cloneControl(c Control) Control {
	c.Options = slices.Clone(c.Options)
	c.Versions = slices.Clone(c.Versions)
	if c.Responsive != nil {
		w := *c.Responsive
		w.Devices = slices.Clone(w.Devices)
		if w.Min != nil {
			v := *w.Min
			w.Min = &v
		}
		if w.Max != nil {
			v := *w.Max
			w.Max = &v
		}
		c.Responsive = &w
	}
	if c.Group != nil {
		g := *c.Group
		if g.Activator != nil {
			v := *g.Activator
			g.Activator = &v
		}
		c.Group = &g
	}
	c.Fields = slices.Clone(c.Fields)
	for i := range c.Fields {
		c.Fields[i] = cloneControl(c.Fields[i])
	}
	return c
}

func loadAllowlist(raw []byte) (*allowlist, error) {
	var a allowlist
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	if a.SchemaVersion != 2 || len(a.Versions) == 0 || len(a.Widgets) == 0 || a.Shared == nil {
		return nil, fmt.Errorf("missing schema, pins or controls")
	}
	for i, pin := range a.Versions {
		p, ok := versionParts(pin)
		if !ok {
			return nil, fmt.Errorf("invalid pin %q", pin)
		}
		if i > 0 {
			prev, _ := versionParts(a.Versions[i-1])
			if compareVersion(prev, p) >= 0 {
				return nil, fmt.Errorf("pins must be strictly ordered")
			}
		}
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
			if !validVersions(c.Versions) || !validControl(c, 0) {
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
		if !controlToken.MatchString(w.Name) || seen[w.Name] || !validVersions(w.Versions) {
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
				if !validVersions(c.Versions) || !validControl(c, 0) {
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

func validControl(c Control, depth int) bool {
	if depth > 8 || !controlToken.MatchString(c.Type) || c.Options == nil || c.Fields == nil {
		return false
	}
	seen := map[string]bool{}
	for _, o := range c.Options {
		if seen[o] {
			return false
		}
		seen[o] = true
	}
	if c.Responsive != nil {
		devices := []string{"mobile", "mobile_extra", "tablet", "tablet_extra", "laptop", "desktop", "widescreen"}
		if c.Responsive.Devices == nil {
			return false
		}
		for _, d := range c.Responsive.Devices {
			if !slices.Contains(devices, d) {
				return false
			}
		}
		for _, bound := range []*string{c.Responsive.Min, c.Responsive.Max} {
			if bound != nil && !slices.Contains(devices, *bound) {
				return false
			}
		}
	}
	if c.Group != nil && (!controlToken.MatchString(c.Group.Family) || (c.Group.Activator != nil && !controlToken.MatchString(*c.Group.Activator))) {
		return false
	}
	if c.Type != "repeater" && len(c.Fields) > 0 {
		return false
	}
	seen = map[string]bool{}
	for _, f := range c.Fields {
		if !controlToken.MatchString(f.ID) || seen[f.ID] || !validControl(f, depth+1) {
			return false
		}
		seen[f.ID] = true
	}
	return true
}
