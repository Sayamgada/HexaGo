package apidocs

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Validator checks JSON values against the document's schemas (the JSON
// Schema subset the document uses: type incl. type arrays, properties,
// required, items, enum, additionalProperties, allOf / anyOf / oneOf, $ref).
// Contract tests use it to hold responses to the spec.
type Validator struct{ doc M }

// NewValidator returns a validator over the spec (round-tripped through JSON
// so it sees exactly what clients see).
func NewValidator() *Validator {
	var doc M
	if err := json.Unmarshal(JSON(), &doc); err != nil {
		panic(err)
	}
	return &Validator{doc: doc}
}

// Doc returns the decoded document.
func (v *Validator) Doc() M { return v.doc }

// ResponseSchema returns the JSON schema documented for method+path (an
// OpenAPI path template) and status, or nil when none is documented.
func (v *Validator) ResponseSchema(method, pathTemplate string, status int) (any, error) {
	paths, _ := v.doc["paths"].(M)
	item, ok := paths[pathTemplate].(M)
	if !ok {
		return nil, fmt.Errorf("path %s is not documented", pathTemplate)
	}
	op, ok := item[strings.ToLower(method)].(M)
	if !ok {
		return nil, fmt.Errorf("%s %s is not documented", method, pathTemplate)
	}
	resps, _ := op["responses"].(M)
	r, ok := resps[strconv.Itoa(status)].(M)
	if !ok {
		return nil, fmt.Errorf("%s %s: status %d is not documented", method, pathTemplate, status)
	}
	if rf, ok := r["$ref"].(string); ok {
		r, _ = v.resolve(rf).(M)
	}
	c, _ := r["content"].(M)
	j, _ := c["application/json"].(M)
	if j == nil {
		return nil, nil
	}
	return j["schema"], nil
}

// ValidateResponse checks body (raw JSON) against the documented response.
func (v *Validator) ValidateResponse(method, pathTemplate string, status int, body []byte) error {
	schema, err := v.ResponseSchema(method, pathTemplate, status)
	if err != nil || schema == nil {
		return err
	}
	var val any
	if err := json.Unmarshal(body, &val); err != nil {
		return fmt.Errorf("%s %s %d: body is not JSON: %v", method, pathTemplate, status, err)
	}
	if err := v.Validate(schema, val); err != nil {
		return fmt.Errorf("%s %s %d: %v", method, pathTemplate, status, err)
	}
	return nil
}

// Validate checks value against schema.
func (v *Validator) Validate(schema, value any) error { return v.check(schema, value, "$", 0) }

func (v *Validator) resolve(ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = v.doc
	for _, part := range strings.Split(ref[2:], "/") {
		m, ok := cur.(M)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func typeOf(val any) string {
	switch x := val.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case M:
		return "object"
	}
	return fmt.Sprintf("%T", val)
}

func typeMatches(want, got string) bool {
	return want == got || (want == "number" && got == "integer")
}

func (v *Validator) check(schema, val any, at string, depth int) error {
	if depth > 64 {
		return fmt.Errorf("%s: schema nesting too deep", at)
	}
	s, ok := schema.(M)
	if !ok {
		if schema == nil || schema == true {
			return nil
		}
		return fmt.Errorf("%s: unsupported schema %v", at, schema)
	}
	if rf, ok := s["$ref"].(string); ok {
		target := v.resolve(rf)
		if target == nil {
			return fmt.Errorf("%s: unresolved $ref %s", at, rf)
		}
		return v.check(target, val, at, depth+1)
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, sub := range all {
			if err := v.check(sub, val, at, depth+1); err != nil {
				return err
			}
		}
	}
	for _, kw := range []string{"anyOf", "oneOf"} { // oneOf is checked leniently (at least one)
		subs, ok := s[kw].([]any)
		if !ok {
			continue
		}
		matched := 0
		var first error
		for _, sub := range subs {
			if err := v.check(sub, val, at, depth+1); err == nil {
				matched++
			} else if first == nil {
				first = err
			}
		}
		if matched == 0 {
			return fmt.Errorf("%s: matches none of %s (first: %v)", at, kw, first)
		}
	}
	if t, ok := s["type"]; ok {
		got := typeOf(val)
		var wants []string
		switch tt := t.(type) {
		case string:
			wants = []string{tt}
		case []any:
			for _, x := range tt {
				if xs, ok := x.(string); ok {
					wants = append(wants, xs)
				}
			}
		}
		okType := false
		for _, w := range wants {
			if typeMatches(w, got) {
				okType = true
			}
		}
		if !okType {
			return fmt.Errorf("%s: want type %v, got %s", at, t, got)
		}
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if x == val {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of %v", at, val, e)
		}
	}
	switch x := val.(type) {
	case M:
		props, _ := s["properties"].(M)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, ok := x[r.(string)]; !ok {
					return fmt.Errorf("%s: missing required property %q", at, r)
				}
			}
		}
		for k, pv := range x {
			if ps, ok := props[k]; ok {
				if err := v.check(ps, pv, at+"."+k, depth+1); err != nil {
					return err
				}
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					return fmt.Errorf("%s: unexpected property %q", at, k)
				}
			case M:
				if err := v.check(ap, pv, at+"."+k, depth+1); err != nil {
					return err
				}
			}
		}
	case []any:
		if items, ok := s["items"]; ok {
			for i, it := range x {
				if err := v.check(items, it, fmt.Sprintf("%s[%d]", at, i), depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
