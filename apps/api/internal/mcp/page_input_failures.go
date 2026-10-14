package mcp

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var pageInputKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// pageInputRefusal names a structural input failure without changing its code.
func pageInputRefusal(path, key, reason string) pageCreateRefusal {
	return pageCreateRefusal{Code: pageCreateBadInput, Input: true,
		Failures: []refusalFailure{{Path: path, Key: key, Reason: reason}}}
}

// pageInputFieldsRefusal describes an input the grammar has already refused.
// It names an unknown member in JSON order or a missing required member, with
// unsafe names addressed by position. When neither is found it states the
// member rules; every call returns a failure, never an acceptance decision.
func pageInputFieldsRefusal(raw []byte, path string, allowed []string, required ...string) refusalFailure {
	d := json.NewDecoder(bytes.NewReader(raw))
	if _, err := d.Token(); err != nil {
		return refusalFailure{Path: path, Key: "input", Reason: "must be a JSON object"}
	}
	seen := map[string]bool{}
	for i := 0; d.More(); i++ {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return refusalFailure{Path: path, Key: "input", Reason: "must be a JSON object"}
		}
		if !slices.Contains(allowed, key) {
			if !pageInputKeyPattern.MatchString(key) {
				key = "fields[" + strconv.Itoa(i) + "]"
			}
			return refusalFailure{Path: path, Key: key, Reason: "unknown field: send only " + pageInputFieldList(allowed)}
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return refusalFailure{Path: path, Key: key, Reason: "must be a JSON value"}
		}
	}
	for _, key := range required {
		if !seen[key] {
			return refusalFailure{Path: path, Key: key, Reason: "is required"}
		}
	}
	reason := "send only " + pageInputFieldList(allowed)
	if len(required) > 0 {
		reason += "; " + pageInputFieldList(required) + " are required"
	}
	return refusalFailure{Path: path, Key: "input", Reason: reason}
}

func pageInputFieldList(fields []string) string {
	if len(fields) < 2 {
		return strings.Join(fields, "")
	}
	return strings.Join(fields[:len(fields)-1], ", ") + " and " + fields[len(fields)-1]
}
