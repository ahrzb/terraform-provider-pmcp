package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// ownerAliases is the attribute value these tests start from: both halves of §23.6's shape set,
// spelled the way the hub's row and the op's arguments spell them.
func ownerAliases(service string, tools map[string]string) types.Object {
	toolValues := make(map[string]attr.Value, len(tools))
	for canonical, alias := range tools {
		toolValues[canonical] = types.StringValue(alias)
	}
	return types.ObjectValueMust(typescriptAliasesAttrTypes, map[string]attr.Value{
		"service": types.StringValue(service),
		"tools":   types.MapValueMust(types.StringType, toolValues),
	})
}

// TestTypescriptAliasesToWireShape pins what actually reaches the hub. The wire spellings matter
// because the hub's schema sets additionalProperties:false: `service` and `tools` are the only
// keys it accepts inside `typescript_aliases`, and the JSON round trip below is what holds the
// Go tags to that. The omission rules are the other half — §23.6 makes "not sent" mean "leave
// established assignments alone", so a null, an unknown, and an unknown CHILD must all vanish
// from the op arguments rather than being serialized as an empty string or an empty map.
func TestTypescriptAliasesToWireShape(t *testing.T) {
	ctx := context.Background()

	wire, diags := typescriptAliasesTo(ctx, ownerAliases("news", map[string]string{"get-news": "getNews"}))
	if diags.HasError() {
		t.Fatalf("typescriptAliasesTo: %v", diags)
	}
	if wire == nil {
		t.Fatal("a configured attribute must produce a wire object, not nil")
	}
	if wire.Service != "news" {
		t.Errorf("service = %q, want %q", wire.Service, "news")
	}
	if wire.Tools["get-news"] != "getNews" {
		t.Errorf(`tools["get-news"] = %q, want %q`, wire.Tools["get-news"], "getNews")
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"service":"news","tools":{"get-news":"getNews"}}`; string(encoded) != want {
		t.Errorf("wire JSON = %s, want %s (the contract's own key spellings)", encoded, want)
	}

	if got, diags := typescriptAliasesTo(ctx, types.ObjectNull(typescriptAliasesAttrTypes)); got != nil || diags.HasError() {
		t.Errorf("null must be omitted from the op, got %+v (%v)", got, diags)
	}
	if got, diags := typescriptAliasesTo(ctx, types.ObjectUnknown(typescriptAliasesAttrTypes)); got != nil || diags.HasError() {
		t.Errorf("unknown must be omitted from the op, got %+v (%v)", got, diags)
	}

	// What a configured `{ service = "news" }` presents for `tools`: unknown, not null. Sending
	// an empty map there would ask the hub to replace the tool map, when the operator said
	// nothing about tools at all.
	partial, diags := typescriptAliasesTo(ctx, types.ObjectValueMust(typescriptAliasesAttrTypes, map[string]attr.Value{
		"service": types.StringValue("news"),
		"tools":   types.MapUnknown(types.StringType),
	}))
	if diags.HasError() {
		t.Fatalf("typescriptAliasesTo(partial): %v", diags)
	}
	if partial == nil || partial.Tools != nil {
		t.Errorf("an unknown child must be omitted, got %+v", partial)
	}
	if partial != nil && partial.Service != "news" {
		t.Errorf("the known sibling must still be sent, got %+v", partial)
	}
}

// TestTypescriptAliasesFromWireShape pins the read direction against §23.6's row contract: the
// row always carries the owner configuration under `typescriptAliases` — `{}` when the owner
// never configured any — and only a hub that omits the key entirely (or predates §23.6) yields
// null. The empty object is deliberately NOT normalized to null: a configured-but-empty block
// must be able to land in state unchanged, and a normalization would refuse it as an
// inconsistent result.
func TestTypescriptAliasesFromWireShape(t *testing.T) {
	ctx := context.Background()

	full, diags := typescriptAliasesFrom(ctx, &pmcp.TypescriptAliases{
		Service: "news",
		Tools:   map[string]string{"get-news": "getNews"},
	})
	if diags.HasError() {
		t.Fatalf("typescriptAliasesFrom: %v", diags)
	}
	if want := ownerAliases("news", map[string]string{"get-news": "getNews"}); !full.Equal(want) {
		t.Errorf("full row decoded to %v, want %v", full, want)
	}

	empty, diags := typescriptAliasesFrom(ctx, &pmcp.TypescriptAliases{})
	if diags.HasError() {
		t.Fatalf("typescriptAliasesFrom({}): %v", diags)
	}
	if empty.IsNull() {
		t.Error("a `{}` row must decode to the empty object, not null")
	}
	wantEmpty := types.ObjectValueMust(typescriptAliasesAttrTypes, map[string]attr.Value{
		"service": types.StringNull(),
		"tools":   types.MapNull(types.StringType),
	})
	if !empty.Equal(wantEmpty) {
		t.Errorf("empty row decoded to %v, want %v", empty, wantEmpty)
	}

	absent, diags := typescriptAliasesFrom(ctx, nil)
	if diags.HasError() {
		t.Fatalf("typescriptAliasesFrom(nil): %v", diags)
	}
	if !absent.IsNull() {
		t.Errorf("an absent row key must decode to null, got %v", absent)
	}
}

// TestTypescriptAliasesValidatorRefusesNonIdentifiers pins the plan-time half of §22.4's alias
// validation: identifier syntax plus the 1-128-byte bound, and nothing more. Reserved words,
// Object-prototype/Promise-sensitive names, the fixed `hub`/`pmcp`/`resources` members and every
// collision stay the hub's, because it owns the reservation table this provider cannot read —
// the last case below states that boundary rather than leaving it implicit.
func TestTypescriptAliasesValidatorRefusesNonIdentifiers(t *testing.T) {
	str := func(s string) *string { return &s }
	atLimit := strings.Repeat("a", 128)
	overLimit := strings.Repeat("a", 129)

	build := func(service, alias *string) types.Object {
		var serviceValue attr.Value = types.StringNull()
		if service != nil {
			serviceValue = types.StringValue(*service)
		}
		var toolValue attr.Value = types.MapNull(types.StringType)
		if alias != nil {
			toolValue = types.MapValueMust(types.StringType, map[string]attr.Value{
				"get-news": types.StringValue(*alias),
			})
		}
		return types.ObjectValueMust(typescriptAliasesAttrTypes, map[string]attr.Value{
			"service": serviceValue,
			"tools":   toolValue,
		})
	}

	for _, tc := range []struct {
		name    string
		service *string
		alias   *string
		wantErr bool
	}{
		{name: "legal service and tool alias", service: str("news"), alias: str("getNews")},
		{name: "dollar and underscore are legal", service: str("$svc__"), alias: str("_x$1")},
		{name: "128 bytes is the inclusive limit", service: str(atLimit), alias: str(atLimit)},
		{name: "129 bytes is refused", service: str(overLimit), alias: str(overLimit), wantErr: true},
		{name: "leading digit is refused", service: str("1news"), alias: str("2nd"), wantErr: true},
		{name: "space is refused", service: str("get news"), wantErr: true},
		{name: "hyphen is refused", alias: str("get-news"), wantErr: true},
		{name: "empty service is refused", service: str(""), wantErr: true},
		{name: "keywords are the hub's call, not this validator's", service: str("class"), alias: str("then")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &validator.ObjectResponse{}
			typescriptAliasesValidator{}.ValidateObject(context.Background(), validator.ObjectRequest{
				Path:        path.Root("typescript_aliases"),
				ConfigValue: build(tc.service, tc.alias),
			}, resp)
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Errorf("HasError = %v, want %v (%v)", resp.Diagnostics.HasError(), tc.wantErr, resp.Diagnostics)
			}
		})
	}
}

// TestAppSlugValidatorReservesVirtualServices keeps provider plans aligned with the hub's two
// non-row namespaces. Without the `hub` case, Terraform accepts a resource identity the server
// must refuse, leaving the error until apply.
func TestAppSlugValidatorReservesVirtualServices(t *testing.T) {
	for _, slug := range []string{"pmcp", "hub"} {
		t.Run(slug, func(t *testing.T) {
			resp := &validator.StringResponse{}
			slugValidator{}.ValidateString(context.Background(), validator.StringRequest{
				Path:        path.Root("slug"),
				ConfigValue: types.StringValue(slug),
			}, resp)
			if !resp.Diagnostics.HasError() {
				t.Errorf("%q must be refused as a hub-owned virtual service", slug)
			}
		})
	}
}

// TestProxyAppAliasesOnlyChangeStillCallsAppUpdate is the regression test for
// commonAppChanged's alias arm. Without it, an alias-only edit produces a plan with a diff and
// an apply that calls no op at all: state silently keeps the old names while the operator
// believes the change landed — the "change that never reaches the hub" class §23.6's tombstone
// rules make expensive.
func TestProxyAppAliasesOnlyChangeStillCallsAppUpdate(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_update" {
			t.Fatalf("an apply touching only typescript_aliases must call app_update, got %q", op)
		}
		if _, ok := args["typescript_aliases"]; !ok {
			t.Error("app_update must carry typescript_aliases when that is what changed")
		}
		return map[string]any{"app": pmcp.AppRow{
			Slug: "app1", Kind: "proxy", Name: "app1", LogBodies: false,
			Roles: map[string]pmcp.RoleFamilies{}, Redact: map[string][]string{}, RedactResults: map[string][]string{},
			Endpoint: "https://upstream.example/mcp", Auth: "headers",
			TypescriptAliases: &pmcp.TypescriptAliases{
				Service: "wire",
				Tools:   map[string]string{"get-news": "headlines"},
			},
		}}, nil
	}}
	client := testClient(t, f)
	res := NewProxyAppResource()
	configure(t, res, client)

	state := baseProxyModel()
	state.TypescriptAliases = ownerAliases("news", map[string]string{"get-news": "getNews"})
	plan := baseProxyModel()
	plan.TypescriptAliases = ownerAliases("wire", map[string]string{"get-news": "headlines"})

	updateResp := &resource.UpdateResponse{State: stateFor(t, res, &state)}
	res.Update(context.Background(), resource.UpdateRequest{
		Plan:  planFor(t, res, &plan),
		State: stateFor(t, res, &state),
	}, updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", updateResp.Diagnostics)
	}
	if len(f.calls) != 1 {
		t.Fatalf("op sequence = %+v, want exactly one app_update", f.calls)
	}

	var got proxyAppModel
	if diags := updateResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.TypescriptAliases.Equal(plan.TypescriptAliases) {
		t.Errorf("state aliases = %v, want the row's committed value %v", got.TypescriptAliases, plan.TypescriptAliases)
	}
}

// TestTunnelAppCreateSendsAliasesAndKeepsRowValue covers the same field on the tunneled kind,
// where the hub rejects proxy-only fields: aliases are legal on both, and a regression that
// wired them into the proxy resource alone would otherwise pass every test in this file except
// this one.
func TestTunnelAppCreateSendsAliasesAndKeepsRowValue(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_create" {
			t.Fatalf("expected app_create, got %q", op)
		}
		aliases, ok := args["typescript_aliases"].(map[string]any)
		if !ok {
			t.Fatalf("app_create args carried typescript_aliases as %T, want an object", args["typescript_aliases"])
		}
		if aliases["service"] != "tools" {
			t.Errorf("service = %v, want %q", aliases["service"], "tools")
		}
		tools, _ := aliases["tools"].(map[string]any)
		if tools["paper_list"] != "paperList" {
			t.Errorf(`tools["paper_list"] = %v, want %q`, tools["paper_list"], "paperList")
		}
		return map[string]any{"app": pmcp.AppRow{
			Slug: "bot1", Kind: "tunnel", Name: "bot1", LogBodies: true,
			Roles: map[string]pmcp.RoleFamilies{}, Redact: map[string][]string{}, RedactResults: map[string][]string{},
			TypescriptAliases: &pmcp.TypescriptAliases{
				Service: "tools",
				Tools:   map[string]string{"paper_list": "paperList"},
			},
		}}, nil
	}}
	client := testClient(t, f)
	res := NewTunnelAppResource()
	configure(t, res, client)

	plan := baseTunnelModel()
	plan.TypescriptAliases = ownerAliases("tools", map[string]string{"paper_list": "paperList"})

	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: planFor(t, res, &plan)}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	var got tunnelAppModel
	if diags := createResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.TypescriptAliases.Equal(plan.TypescriptAliases) {
		t.Errorf("state aliases = %v, want %v", got.TypescriptAliases, plan.TypescriptAliases)
	}
}
