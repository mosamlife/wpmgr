package agentcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
)

// OutputShape is a catalogue entry's pinned output shape (output_fields), in
// the grammar the agent projects with and the m158 CHECK enforces:
//
//	{"fields":{"<key>":<shape>,...}} | {"items":<shape>} | "string" | "int" | "bool"
//
// Keys match ^[A-Za-z0-9_-]{1,64}$ and nesting is at most 8 deep. Exactly one
// of Scalar, Fields and Items is set.
type OutputShape struct {
	Scalar string
	Fields map[string]*OutputShape
	Items  *OutputShape
}

// OutputShapeMaxDepth matches ability_output_shape_valid's depth bound.
const OutputShapeMaxDepth = 8

var outputShapeKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrOutputShape is a value outside the grammar: an unknown node kind, an
// extra member, a bad key, or too deep.
var ErrOutputShape = errors.New("output_fields is not a valid output shape")

// ParseOutputShape parses and validates output_fields strictly.
func ParseOutputShape(raw []byte) (*OutputShape, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, ErrOutputShape
	}
	return outputShapeNode(v, 0)
}

func outputShapeNode(v any, depth int) (*OutputShape, error) {
	if depth > OutputShapeMaxDepth {
		return nil, ErrOutputShape
	}
	switch t := v.(type) {
	case string:
		switch t {
		case "string", "int", "bool":
			return &OutputShape{Scalar: t}, nil
		}
	case map[string]any:
		if len(t) != 1 {
			return nil, ErrOutputShape
		}
		if f, ok := t["fields"]; ok {
			fm, ok := f.(map[string]any)
			if !ok {
				return nil, ErrOutputShape
			}
			out := &OutputShape{Fields: make(map[string]*OutputShape, len(fm))}
			for k, sub := range fm {
				if !outputShapeKeyRe.MatchString(k) {
					return nil, ErrOutputShape
				}
				s, err := outputShapeNode(sub, depth+1)
				if err != nil {
					return nil, err
				}
				out.Fields[k] = s
			}
			return out, nil
		}
		if it, ok := t["items"]; ok {
			s, err := outputShapeNode(it, depth+1)
			if err != nil {
				return nil, err
			}
			return &OutputShape{Items: s}, nil
		}
	}
	return nil, ErrOutputShape
}
