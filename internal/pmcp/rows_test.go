package pmcp

import (
	"context"
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

// TestAppRowDecodesOwnerAliases pins §23.6's read shape: the owner's naming configuration
// arrives under `typescriptAliases`, spelled exactly like the wire contract, separately from the
// hub's resolved reservations (which ride the row under their own keys and must NOT be folded
// into this one). It is a pointer so a hub that omits the key stays distinguishable from one
// that reports an empty object.
func TestAppRowDecodesOwnerAliases(t *testing.T) {
	raw := `{
		"slug": "app1", "kind": "proxy", "name": "app1", "description": "",
		"archived": false, "logBodies": false,
		"roles": {}, "redact": {}, "redactResults": {}, "builtin": false, "createdAt": 0,
		"endpoint": "", "auth": "headers", "forwardIdentity": false, "capabilities": null,
		"typescriptAliases": {"service": "news", "tools": {"get-news": "getNews"}},
		"typescriptReservations": {"service": "news"},
		"typescriptDiagnostics": []
	}`

	var row AppRow
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatalf("Unmarshal AppRow: %v", err)
	}
	if row.TypescriptAliases == nil {
		t.Fatal("typescriptAliases decoded to nil, want the owner configuration")
	}
	if row.TypescriptAliases.Service != "news" {
		t.Errorf("service = %q, want %q", row.TypescriptAliases.Service, "news")
	}
	if got := row.TypescriptAliases.Tools["get-news"]; got != "getNews" {
		t.Errorf(`tools["get-news"] = %q, want %q`, got, "getNews")
	}

	var absent AppRow
	if err := json.Unmarshal([]byte(`{"slug": "app1"}`), &absent); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if absent.TypescriptAliases != nil {
		t.Errorf("an omitted key must decode to nil, got %+v", absent.TypescriptAliases)
	}
}

// TestAppRowDecodesOwnerRoles pins §20.3's owner map as the tunnel row carries it: under
// `ownerRoles`, in the same canonical mixed shape as `roles`, and kept apart from `roles`, which
// on a tunnel row is the app's own declaration. The absent case is the proxied and builtin
// rows' (the hub omits the key there), and it must stay distinguishable from a tunnel row's `{}`.
func TestAppRowDecodesOwnerRoles(t *testing.T) {
	raw := `{
		"slug": "bot1", "kind": "tunnel", "name": "bot1",
		"roles": {"reader": ["app_tool"]},
		"ownerRoles": {"mine": ["get_.*"], "spanning": {"prompts": ["draft_.*"]}}
	}`

	var row AppRow
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatalf("Unmarshal AppRow: %v", err)
	}
	want := map[string]RoleFamilies{
		"mine":     {Tools: []string{"get_.*"}},
		"spanning": {Prompts: []string{"draft_.*"}},
	}
	if !reflect.DeepEqual(row.OwnerRoles, want) {
		t.Errorf("ownerRoles = %+v, want %+v", row.OwnerRoles, want)
	}
	if _, leaked := row.Roles["mine"]; leaked {
		t.Error("an owner role must not appear under roles, the app's own declaration")
	}

	var empty, absent AppRow
	if err := json.Unmarshal([]byte(`{"slug": "bot1", "ownerRoles": {}}`), &empty); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if empty.OwnerRoles == nil {
		t.Error("a tunnel row's `{}` must decode to an empty map, not nil")
	}
	if err := json.Unmarshal([]byte(`{"slug": "papp1"}`), &absent); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if absent.OwnerRoles != nil {
		t.Errorf("an omitted key must decode to nil, got %+v", absent.OwnerRoles)
	}
}

// TestHubSettingsUpdateSendsContractArgumentNames pins the write op's argument surface: exactly
// the contract's two snake_case integers, as JSON numbers. A string spelling would be refused by
// the hub's own schema before the value was ever compared, and an extra key would be refused by
// `additionalProperties: false` — both failures the provider would otherwise discover only at
// apply time.
func TestHubSettingsUpdateSendsContractArgumentNames(t *testing.T) {
	f := &fake{t: t, namespace: "owner", reply: func(string) (any, *rpcErrorBody) {
		return map[string]any{"settings": map[string]any{"defaultTimeoutMs": 45000, "maxTimeoutMs": 120000}}, nil
	}}
	srv := f.server()
	defer srv.Close()

	client, err := New(context.Background(), srv.URL, "pmcp_adm_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	settings, err := client.HubSettingsUpdate(context.Background(), 45000, 120000)
	if err != nil {
		t.Fatalf("HubSettingsUpdate: %v", err)
	}
	if settings.DefaultTimeoutMs != 45000 || settings.MaxTimeoutMs != 120000 {
		t.Errorf("settings = %+v, want the committed pair", settings)
	}

	if len(f.calls) != 1 {
		t.Fatalf("calls = %+v, want exactly one", f.calls)
	}
	call := f.calls[0]
	if call.Op != "hub_settings_update" {
		t.Errorf("op = %q, want hub_settings_update", call.Op)
	}
	if len(call.Args) != 2 {
		t.Errorf("args = %v, want exactly the contract's two fields", call.Args)
	}
	if got, ok := call.Args["default_timeout_ms"].(float64); !ok || got != 45000 {
		t.Errorf("default_timeout_ms = %#v, want the number 45000", call.Args["default_timeout_ms"])
	}
	if got, ok := call.Args["max_timeout_ms"].(float64); !ok || got != 120000 {
		t.Errorf("max_timeout_ms = %#v, want the number 120000", call.Args["max_timeout_ms"])
	}
}

// TestHubSettingsGetReadsTheSettingsEnvelope covers the read op: no arguments at all (the
// contract declares an empty object), and the payload arrives wrapped in `settings` rather than
// at the top level.
func TestHubSettingsGetReadsTheSettingsEnvelope(t *testing.T) {
	f := &fake{t: t, namespace: "owner", reply: func(op string) (any, *rpcErrorBody) {
		return map[string]any{"settings": map[string]any{"defaultTimeoutMs": 30000, "maxTimeoutMs": 30000}}, nil
	}}
	srv := f.server()
	defer srv.Close()

	client, err := New(context.Background(), srv.URL, "pmcp_adm_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	settings, err := client.HubSettingsGet(context.Background())
	if err != nil {
		t.Fatalf("HubSettingsGet: %v", err)
	}
	if settings.DefaultTimeoutMs != 30000 || settings.MaxTimeoutMs != 30000 {
		t.Errorf("settings = %+v, want the absent-row default pair", settings)
	}
	if len(f.calls) != 1 || f.calls[0].Op != "hub_settings_get" {
		t.Fatalf("calls = %+v, want one hub_settings_get", f.calls)
	}
	if len(f.calls[0].Args) != 0 {
		t.Errorf("args = %v, want the contract's empty object", f.calls[0].Args)
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
