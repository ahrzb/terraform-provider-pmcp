// Package coverage implements §22.5's parity oracle: it drives the provider's real
// Create/Read/Update/Delete (and data source Read) paths against a scripted fake of the hub's
// admin surface, records which op each path called and with which argument field names, and
// checks that recording against the hub's checked-in admin-ops contract. See oracle.go's Check
// for the three assertions and coverage/staged.json for the staging rule that lets the two
// repositories land a change in separate commits.
package coverage

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
)

// Contract is admin-ops.json's shape: every op name the hub advertises, and the JSON Schema each
// op's `tools/call` arguments must satisfy. Only the two fields the oracle needs are decoded;
// outputSchemas (write-only masking, result shapes) is §22.1's concern, not this one's.
type Contract struct {
	Names        []string              `json:"names"`
	InputSchemas map[string]schemaNode `json:"inputSchemas"`
}

// schemaNode is the subset of JSON Schema admin-ops.json actually uses: object schemas with
// properties/required/additionalProperties:false at the top, string/integer/number/boolean/
// array/object leaves, `enum`, `pattern`, `oneOf` (expires_in's "integer or the literal
// \"never\"") and `additionalProperties` as a nested schema for the map-shaped fields (`roles`,
// `redact`, `redact_results`). It decodes recursively via plain encoding/json — no custom
// UnmarshalJSON needed, since every nested shape here is itself valid JSON Schema.
type schemaNode struct {
	Type                 string                `json:"type"`
	Enum                 []json.RawMessage     `json:"enum"`
	Const                json.RawMessage       `json:"const"`
	Pattern              string                `json:"pattern"`
	Items                *schemaNode           `json:"items"`
	Properties           map[string]schemaNode `json:"properties"`
	Required             []string              `json:"required"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
	OneOf                []schemaNode          `json:"oneOf"`
}

// LoadContract parses one admin-ops.json fixture — either the hub's own working-tree copy
// (passed by `apps.coverage-check`) or this repo's pinned snapshot (`checks.coverage-check`).
func LoadContract(path string) (*Contract, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading contract %s: %w", path, err)
	}
	var c Contract
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing contract %s: %w", path, err)
	}
	return &c, nil
}

// validateArgs checks one recorded call's top-level arguments against its op's declared input
// schema: every `required` key present, every key a declared property, and each value
// type-checked against its property schema. This is what keeps the oracle from drifting into
// accepting a field the hub's own `parseInput` would reject (§22.5) — a typo'd or renamed
// argument fails here, at the fake, rather than silently padding field coverage.
func validateArgs(op string, node schemaNode, args map[string]any) error {
	for _, req := range node.Required {
		if _, ok := args[req]; !ok {
			return fmt.Errorf("%s: missing required field %q", op, req)
		}
	}
	for key, val := range args {
		prop, ok := node.Properties[key]
		if !ok {
			return fmt.Errorf("%s: sent field %q, which is not in the contract's declared properties", op, key)
		}
		if err := validateValue(prop, val); err != nil {
			return fmt.Errorf("%s.%s: %w", op, key, err)
		}
	}
	return nil
}

// validateValue type-checks one field's value against its schema node, recursing through
// oneOf/array-items/object-properties/map-additionalProperties. It is deliberately not a full
// JSON Schema implementation — admin-ops.json only ever uses the shapes handled below — but it
// is enough to catch a field sent as the wrong shape entirely (e.g. a string where the contract
// declares an object), which is the class of drift a coverage oracle exists to catch.
func validateValue(s schemaNode, v any) error {
	if len(s.OneOf) > 0 {
		var errs []string
		for _, sub := range s.OneOf {
			if err := validateValue(sub, v); err == nil {
				return nil
			} else {
				errs = append(errs, err.Error())
			}
		}
		return fmt.Errorf("matched none of oneOf (%v)", errs)
	}
	if s.Const != nil {
		var want any
		_ = json.Unmarshal(s.Const, &want)
		if !reflect.DeepEqual(v, want) {
			return fmt.Errorf("want const %v, got %v", want, v)
		}
		return nil
	}
	if len(s.Enum) > 0 {
		matched := false
		for _, e := range s.Enum {
			var want any
			_ = json.Unmarshal(e, &want)
			if reflect.DeepEqual(v, want) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("value %v not in enum", v)
		}
	}
	switch s.Type {
	case "string":
		sv, ok := v.(string)
		if !ok {
			return fmt.Errorf("want string, got %T", v)
		}
		if s.Pattern != "" {
			if !regexp.MustCompile(s.Pattern).MatchString(sv) {
				return fmt.Errorf("value %q does not match pattern %q", sv, s.Pattern)
			}
		}
	case "integer", "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("want number, got %T", v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("want boolean, got %T", v)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("want array, got %T", v)
		}
		if s.Items != nil {
			for i, elem := range arr {
				if err := validateValue(*s.Items, elem); err != nil {
					return fmt.Errorf("[%d]: %w", i, err)
				}
			}
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("want object, got %T", v)
		}
		if len(s.Properties) > 0 {
			for k, val := range obj {
				p, ok := s.Properties[k]
				if !ok {
					return fmt.Errorf("unexpected field %q", k)
				}
				if err := validateValue(p, val); err != nil {
					return fmt.Errorf(".%s: %w", k, err)
				}
			}
			break
		}
		if s.AdditionalProperties == nil {
			break
		}
		var allowed bool
		if err := json.Unmarshal(s.AdditionalProperties, &allowed); err == nil {
			if !allowed && len(obj) > 0 {
				return fmt.Errorf("additional properties not allowed")
			}
			break
		}
		var sub schemaNode
		if err := json.Unmarshal(s.AdditionalProperties, &sub); err == nil {
			for k, val := range obj {
				if err := validateValue(sub, val); err != nil {
					return fmt.Errorf("[%q]: %w", k, err)
				}
			}
		}
	}
	return nil
}
