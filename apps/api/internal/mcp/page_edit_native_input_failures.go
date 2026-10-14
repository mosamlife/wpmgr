package mcp

import (
	"fmt"
	"regexp"
)

const (
	nativeInputStringReason    = `write every value as a string; write a number as a numeric string, such as "20"`
	nativeInputNullReason      = "use null only for a top-level settings key deletion"
	nativeInputEmptyReason     = "write a nonempty JSON object for a nested map; use a JSON array for a list"
	nativeInputListReason      = "write a JSON array for a list, not an object with consecutive numeric keys"
	nativeInputDuplicateReason = "send each object key only once, including escaped spellings"
)

var nativeInputReportKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var nativeInputSettingsPath = regexp.MustCompile(`^operations\[[0-9]+\]\.settings$`)

// Caller keys outside the reporting grammar are addressed by insertion position.
func nativeInputAddress(path, key string, position int) (string, string) {
	if nativeInputReportKey.MatchString(key) {
		if path == "" {
			return key, key
		}
		return path + "." + key, key
	}
	step := fmt.Sprintf("[%d]", position)
	return path + step, step
}

func nativeInputFailure(path, key, reason string) refusalFailure {
	return refusalFailure{Path: refusalLine(path, refusalMaxPathChars), Key: key, Reason: reason, fullPath: true}
}

// Duplicate checks retain raw member order and decoded escaped spellings.
func pageEditNativeDuplicateFailures(v any) []refusalFailure {
	var failures []refusalFailure
	var walk func(any, string, string)
	walk = func(v any, path, root string) {
		if len(failures) == refusalMaxFailures {
			return
		}
		switch x := v.(type) {
		case phpObject:
			seen := map[string]bool{}
			for i, p := range x {
				if len(failures) == refusalMaxFailures {
					return
				}
				at, key := nativeInputAddress(path, p.key, i)
				nextRoot := root
				if nativeInputSettingsPath.MatchString(path) {
					nextRoot = key
				}
				if seen[p.key] {
					if nextRoot != "" {
						key = nextRoot
					}
					failures = append(failures, nativeInputFailure(at, key, nativeInputDuplicateReason))
				}
				seen[p.key] = true
				walk(p.value, at, nextRoot)
			}
		case phpList:
			for i, child := range x {
				walk(child, fmt.Sprintf("%s[%d]", path, i), root)
			}
		}
	}
	walk(v, "", "")
	return failures
}

// Native input preserves collection identity, string leaves and top-level deletions.
func pageEditNativeValueFailures(m phpObject, path string) []refusalFailure {
	var failures []refusalFailure
	var walk func(any, string, string, bool)
	walk = func(v any, path, key string, deletion bool) {
		if len(failures) == refusalMaxFailures {
			return
		}
		fail := func(reason string) { failures = append(failures, nativeInputFailure(path, key, reason)) }
		switch x := v.(type) {
		case string:
		case nil:
			if !deletion {
				fail(nativeInputNullReason)
			}
		case phpList:
			for i, child := range x {
				walk(child, fmt.Sprintf("%s[%d]", path, i), key, false)
			}
		case phpObject:
			if len(x) == 0 {
				fail(nativeInputEmptyReason)
				return
			}
			numeric := true
			for i, p := range x {
				numeric = numeric && p.key == fmt.Sprint(i)
			}
			if numeric {
				fail(nativeInputListReason)
			}
			for i, p := range x {
				at, _ := nativeInputAddress(path, p.key, i)
				walk(p.value, at, key, false)
			}
		default:
			fail(nativeInputStringReason)
		}
	}
	for i, p := range m {
		at, key := nativeInputAddress(path, p.key, i)
		walk(p.value, at, key, true)
	}
	return failures
}
