package pmcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestRoleFamiliesUnmarshalAcceptsBareListAndObject covers the bug the custom UnmarshalJSON
// exists to fix: the hub's canonical rendering (§20.3) emits a bare pattern list for a
// tools-only role and the per-family object otherwise. Without this, decoding the common case
// — any role that grants tools alone — into AppRow.Roles would fail outright, since
// encoding/json cannot unmarshal a JSON array into a struct.
func TestRoleFamiliesUnmarshalAcceptsBareListAndObject(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want RoleFamilies
	}{
		{
			name: "bare list is tools-only, the canonical rendering for a tools-only role",
			wire: `["get_.*", "list_.*"]`,
			want: RoleFamilies{Tools: []string{"get_.*", "list_.*"}},
		},
		{
			name: "object form carries every declared family",
			wire: `{"prompts": ["summarize_.*"], "resources": ["linear://docs/*"]}`,
			want: RoleFamilies{Prompts: []string{"summarize_.*"}, Resources: []string{"linear://docs/*"}},
		},
		{
			name: "empty object means no families declared",
			wire: `{}`,
			want: RoleFamilies{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got RoleFamilies
			if err := json.Unmarshal([]byte(tc.wire), &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.wire, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Unmarshal(%s) = %+v, want %+v", tc.wire, got, tc.want)
			}
		})
	}
}

// TestAppRowDecodesRolesWithMixedShapes covers the realistic case AppRow.Roles decodes: a
// namespace with one tools-only role (bare list) and one multi-family role (object) in the same
// app_get response — the shape that would previously fail to unmarshal at all.
func TestAppRowDecodesRolesWithMixedShapes(t *testing.T) {
	raw := `{
		"slug": "app1", "kind": "proxy", "name": "app1", "description": "",
		"archived": false, "logBodies": false,
		"roles": {
			"reader": ["get_.*"],
			"docs": {"prompts": ["summarize_.*"], "resources": ["linear://docs/*"]}
		},
		"redact": {}, "redactResults": {}, "builtin": false, "createdAt": 0,
		"endpoint": "", "auth": "headers", "forwardIdentity": false, "capabilities": null
	}`

	var row AppRow
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatalf("Unmarshal AppRow: %v", err)
	}
	if got := row.Roles["reader"]; !reflect.DeepEqual(got, RoleFamilies{Tools: []string{"get_.*"}}) {
		t.Errorf(`roles["reader"] = %+v, want the bare list decoded as tools-only`, got)
	}
	if got := row.Roles["docs"]; !reflect.DeepEqual(got, RoleFamilies{
		Prompts: []string{"summarize_.*"}, Resources: []string{"linear://docs/*"},
	}) {
		t.Errorf(`roles["docs"] = %+v, want the object form decoded verbatim`, got)
	}
}
