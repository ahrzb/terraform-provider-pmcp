package provider

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func tunnelApp(slug string, roles ...string) pmcp.AppRow {
	families := map[string]pmcp.RoleFamilies{}
	for _, role := range roles {
		families[role] = pmcp.RoleFamilies{Tools: []string{".*"}}
	}
	return pmcp.AppRow{Slug: slug, Kind: "tunnel", Roles: families}
}

func proxyApp(slug string, roles ...string) pmcp.AppRow {
	families := map[string]pmcp.RoleFamilies{}
	for _, role := range roles {
		families[role] = pmcp.RoleFamilies{Tools: []string{".*"}}
	}
	return pmcp.AppRow{Slug: slug, Kind: "proxy", Roles: families}
}

// stringSetValues reads a types.Set of strings back into a sorted slice for comparison; nil for
// null/unknown, matching stringSetTo's own contract.
func stringSetValues(t *testing.T, s types.Set) []string {
	t.Helper()
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	values, diags := stringSetTo(context.Background(), s)
	if diags.HasError() {
		t.Fatalf("stringSetTo: %v", diags)
	}
	sort.Strings(values)
	return values
}

// --- Read: the three "gone" cases (§22.4) ---

func TestGrantReadRemovesResourceWhenAgentAbsent(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "agent_list" {
			t.Fatalf("expected agent_list, got %q", op)
		}
		return map[string]any{"agents": []pmcp.AgentRow{}}, nil
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	state := stateFor(t, res, &grantResourceModel{
		Agent: types.StringValue("gone"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: types.SetNull(types.StringType),
	})
	readResp := &resource.ReadResponse{State: state}
	res.Read(context.Background(), resource.ReadRequest{State: state}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Errorf("expected state removed when the agent is absent, got %v", readResp.State.Raw)
	}
}

func TestGrantReadRemovesResourceWhenAgentHasNoKeyForApp(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		return map[string]any{"agents": []pmcp.AgentRow{
			{Slug: "bot", Grants: map[string][]string{"other-app": {"reader"}}},
		}}, nil
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	state := stateFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: types.SetNull(types.StringType),
	})
	readResp := &resource.ReadResponse{State: state}
	res.Read(context.Background(), resource.ReadRequest{State: state}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Errorf("expected state removed when the agent holds no key for %q, got %v", "app1", readResp.State.Raw)
	}
}

func TestGrantReadRemovesResourceWhenKeyHasEmptyRoleList(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		return map[string]any{"agents": []pmcp.AgentRow{
			{Slug: "bot", Grants: map[string][]string{"app1": {}}},
		}}, nil
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	state := stateFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: types.SetNull(types.StringType),
	})
	readResp := &resource.ReadResponse{State: state}
	res.Read(context.Background(), resource.ReadRequest{State: state}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Errorf("expected state removed when the key's role list is empty, got %v", readResp.State.Raw)
	}
}

// --- allow/approval round-trip through the wire's ":approval" suffix ---

func TestGrantAllowApprovalRoundTripsThroughWireSuffix(t *testing.T) {
	var sentRoles []string
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		switch op {
		case "app_get":
			return map[string]any{"app": tunnelApp("app1", "reader", "writer")}, nil
		case "grant_set":
			roles := args["roles"].([]any)
			for _, r := range roles {
				sentRoles = append(sentRoles, r.(string))
			}
			return pmcp.GrantSetResult{Agent: args["agent"].(string), App: args["app"].(string), Roles: toStrings(roles)}, nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	plan := planFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: mustSet(t, "writer"),
	})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	sort.Strings(sentRoles)
	want := []string{"reader", "writer:approval"}
	if !reflect.DeepEqual(sentRoles, want) {
		t.Fatalf("roles sent to grant_set = %v, want %v", sentRoles, want)
	}

	// Now read it back, as agent_list would report the same wire encoding, and confirm the
	// split reverses cleanly.
	f.reply = func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "agent_list" {
			t.Fatalf("expected agent_list, got %q", op)
		}
		return map[string]any{"agents": []pmcp.AgentRow{
			{Slug: "bot", Grants: map[string][]string{"app1": {"reader", "writer:approval"}}},
		}}, nil
	}
	var stateModel grantResourceModel
	if diags := createResp.State.Get(context.Background(), &stateModel); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	readResp := &resource.ReadResponse{State: createResp.State}
	res.Read(context.Background(), resource.ReadRequest{State: createResp.State}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}
	if diags := readResp.State.Get(context.Background(), &stateModel); diags.HasError() {
		t.Fatalf("State.Get after Read: %v", diags)
	}
	if allow := stringSetValues(t, stateModel.Allow); !reflect.DeepEqual(allow, []string{"reader"}) {
		t.Errorf("allow after read = %v, want [reader]", allow)
	}
	if approval := stringSetValues(t, stateModel.Approval); !reflect.DeepEqual(approval, []string{"writer"}) {
		t.Errorf("approval after read = %v, want [writer]", approval)
	}
}

// --- ValidateConfig: disjointness and non-emptiness ---

func TestGrantValidateConfigRejectsOverlappingAllowApproval(t *testing.T) {
	res := &grantResource{}
	cfg := configFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader", "writer"), Approval: mustSet(t, "writer"),
	})
	var resp resource.ValidateConfigResponse
	res.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for a role granted in both allow and approval")
	}
}

func TestGrantValidateConfigRejectsEmptyGrant(t *testing.T) {
	res := &grantResource{}
	cfg := configFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: types.SetNull(types.StringType), Approval: types.SetNull(types.StringType),
	})
	var resp resource.ValidateConfigResponse
	res.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error when both allow and approval are empty")
	}
}

func TestGrantValidateConfigAcceptsDisjointNonEmptySets(t *testing.T) {
	res := &grantResource{}
	cfg := configFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: types.SetNull(types.StringType),
	})
	var resp resource.ValidateConfigResponse
	res.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("expected no error, got %v", resp.Diagnostics)
	}
}

// TestGrantValidateConfigRejectsEmptyAllowBesideNonEmptyApproval is a regression test for a
// reproduced perpetual diff: `allow` and `approval` are both Optional and not Computed, so
// OpenTofu requires state to echo config exactly, and `[]` is a different value from null. The
// hub's wire sends one flat role list, so Read cannot tell "no allow roles" (an explicit `[]`)
// apart from "allow omitted" (null) — both reconstruct as null — which would make an explicit
// `allow = []` beside a non-empty `approval` re-plan `null -> []` forever.
func TestGrantValidateConfigRejectsEmptyAllowBesideNonEmptyApproval(t *testing.T) {
	res := &grantResource{}
	cfg := configFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow:    types.SetValueMust(types.StringType, []attr.Value{}),
		Approval: mustSet(t, "reviewer"),
	})
	var resp resource.ValidateConfigResponse
	res.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for allow = [] beside a non-empty approval — [] and omitted round-trip identically and would perpetually re-plan")
	}
}

// TestGrantValidateConfigAcceptsOmittedAllowBesideNonEmptyApproval is the twin acceptance case:
// omitting `allow` entirely (null) beside a non-empty `approval` says the same thing as an
// empty allow set, without the perpetual-diff hazard, and must be allowed.
func TestGrantValidateConfigAcceptsOmittedAllowBesideNonEmptyApproval(t *testing.T) {
	res := &grantResource{}
	cfg := configFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow:    types.SetNull(types.StringType),
		Approval: mustSet(t, "reviewer"),
	})
	var resp resource.ValidateConfigResponse
	res.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: cfg}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("expected no error when allow is omitted beside a non-empty approval, got %v", resp.Diagnostics)
	}
}

// --- undeclared-role warning-vs-error split by app kind ---

func TestGrantCreateWarnsOnUndeclaredRoleForTunnelApp(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		switch op {
		case "app_get":
			return map[string]any{"app": tunnelApp("app1")}, nil // declares nothing
		case "grant_set":
			return pmcp.GrantSetResult{Agent: "bot", App: "app1", Roles: []string{"reader"}}, nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	plan := planFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: types.SetNull(types.StringType),
	})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)

	if createResp.Diagnostics.HasError() {
		t.Fatalf("undeclared role on a tunneled app must warn, not error: %v", createResp.Diagnostics)
	}
	if createResp.Diagnostics.WarningsCount() == 0 {
		t.Error("expected a warning for the undeclared role")
	}
	// The write must still have happened — a tunneled app's config may legitimately lead the
	// first connection.
	found := false
	for _, c := range f.calls {
		if c.Op == "grant_set" {
			found = true
		}
	}
	if !found {
		t.Error("expected grant_set to be called despite the undeclared-role warning")
	}
}

// TestGrantCreateTreatsOwnerRoleAsDeclared pins the pre-check to the hub's own rule (§20.3): a
// name only the app's `ownerRoles` defines is declared, so granting it draws no warning — the
// hub's setGrants draws none either, and a warning here would contradict it on every apply of
// a configuration that manages owner roles.
func TestGrantCreateTreatsOwnerRoleAsDeclared(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		switch op {
		case "app_get":
			app := tunnelApp("app1") // the app itself declares nothing
			app.OwnerRoles = map[string]pmcp.RoleFamilies{"mine": {Tools: []string{"get_.*"}}}
			return map[string]any{"app": app}, nil
		case "grant_set":
			return pmcp.GrantSetResult{Agent: "bot", App: "app1", Roles: []string{"mine"}}, nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	plan := planFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "mine"), Approval: types.SetNull(types.StringType),
	})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)

	if createResp.Diagnostics.HasError() || createResp.Diagnostics.WarningsCount() != 0 {
		t.Errorf("an owner role is declared; want no diagnostics, got %v", createResp.Diagnostics)
	}
}

func TestGrantCreateErrorsOnUndeclaredRoleForProxyApp(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_get" {
			t.Fatalf("grant_set must not be called when an undeclared role errors on a proxy app; got op %q", op)
		}
		return map[string]any{"app": proxyApp("app1")}, nil // declares nothing
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	plan := planFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: types.SetNull(types.StringType),
	})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)

	if !createResp.Diagnostics.HasError() {
		t.Fatal("expected an error for an undeclared role on a proxy app")
	}
}

// TestIsInlineGrantEntryMirrorsTheHubParser pins isInlineGrantEntry to the hub's
// parseGrantEntry: the family word is the text before the FIRST `/`, it must be exactly one of
// the three singular words, and the pattern after it, colons and further slashes included, is
// the item's. An empty pattern is still an item: the hub refuses it as an invalid pattern, never
// as an undeclared role.
func TestIsInlineGrantEntryMirrorsTheHubParser(t *testing.T) {
	for entry, want := range map[string]bool{
		"tool/get_.*":            true,
		"prompt/draft_.*":        true,
		"resource/news://feed/*": true,
		"tool/":                  true,
		"reader":                 false,
		"all":                    false,
		"tools/get_.*":           false, // the plural is a keyspace, not an entry prefix
		"Tool/get_.*":            false,
		"foo/x":                  false,
	} {
		if got := isInlineGrantEntry(entry); got != want {
			t.Errorf("isInlineGrantEntry(%q) = %v, want %v", entry, got, want)
		}
	}
}

// TestGrantCreateInlineEntriesAreNeverUndeclared covers the pre-check against §8's inline
// grant entries. An item carries its own pattern, so the hub's setGrants never checks it
// against the declaration, and neither may this: before, a proxy app refused
// `tool/<pattern>` as an "Undeclared role" without calling grant_set. A bare role name beside
// an item is still judged exactly as before, and a prefix that is not a family word is a role
// name, as the hub parses it.
func TestGrantCreateInlineEntriesAreNeverUndeclared(t *testing.T) {
	items := []string{"tool/get_.*", "prompt/draft_.*"}
	for _, tc := range []struct {
		name         string
		app          pmcp.AppRow
		allow        []string
		wantErr      bool
		wantWarnings int
	}{
		{name: "items on a proxy app", app: proxyApp("app1"), allow: items},
		{name: "items on a tunneled app", app: tunnelApp("app1"), allow: items},
		{
			name: "an undeclared role beside an item still errors on a proxy app",
			app:  proxyApp("app1"), allow: append([]string{"reader"}, items...), wantErr: true,
		},
		{
			name: "an undeclared role beside an item still warns on a tunneled app",
			app:  tunnelApp("app1"), allow: append([]string{"reader"}, items...), wantWarnings: 1,
		},
		{
			name: "an unknown prefix is a role name, as the hub parses it",
			app:  proxyApp("app1"), allow: []string{"foo/x"}, wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
				switch op {
				case "app_get":
					return map[string]any{"app": tc.app}, nil // declares nothing
				case "grant_set":
					if tc.wantErr {
						t.Errorf("grant_set must not run after the pre-check refused the set")
					}
					return pmcp.GrantSetResult{Agent: "bot", App: "app1", Roles: toStrings(args["roles"].([]any))}, nil
				default:
					t.Fatalf("unexpected op %q", op)
					return nil, nil
				}
			}}
			client := testClient(t, f)
			res := NewGrantResource()
			configure(t, res, client)

			plan := planFor(t, res, &grantResourceModel{
				Agent: types.StringValue("bot"), App: types.StringValue("app1"),
				Allow:    mustSet(t, tc.allow...),
				Approval: mustSet(t, "resource/news://feed/*"),
			})
			createResp := &resource.CreateResponse{State: emptyState(t, res)}
			res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)

			if createResp.Diagnostics.HasError() != tc.wantErr {
				t.Errorf("HasError = %v, want %v: %v", createResp.Diagnostics.HasError(), tc.wantErr, createResp.Diagnostics)
			}
			if got := createResp.Diagnostics.WarningsCount(); got != tc.wantWarnings {
				t.Errorf("warnings = %d, want %d: %v", got, tc.wantWarnings, createResp.Diagnostics)
			}
		})
	}
}

func TestGrantCreateAllowsBuiltinAllRoleWithoutDeclaration(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		switch op {
		case "app_get":
			return map[string]any{"app": proxyApp("app1")}, nil // declares nothing; proxy, so undeclared would normally error
		case "grant_set":
			return pmcp.GrantSetResult{Agent: "bot", App: "app1", Roles: []string{"all"}}, nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	plan := planFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "all"), Approval: types.SetNull(types.StringType),
	})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)

	if createResp.Diagnostics.HasError() {
		t.Fatalf("`all` must be exempt from the undeclared-role check even on a proxy app: %v", createResp.Diagnostics)
	}
}

// --- Import ---

func TestGrantImportSplitsAgentSlashApp(t *testing.T) {
	res := &grantResource{}
	state := emptyState(t, res)
	resp := &resource.ImportStateResponse{State: state}
	res.ImportState(context.Background(), resource.ImportStateRequest{ID: "bot/app1"}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", resp.Diagnostics)
	}

	var model grantResourceModel
	if diags := resp.State.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if model.Agent.ValueString() != "bot" || model.App.ValueString() != "app1" {
		t.Errorf("imported agent/app = %q/%q, want bot/app1", model.Agent.ValueString(), model.App.ValueString())
	}
}

func TestGrantImportRejectsMalformedID(t *testing.T) {
	res := &grantResource{}
	state := emptyState(t, res)
	resp := &resource.ImportStateResponse{State: state}
	res.ImportState(context.Background(), resource.ImportStateRequest{ID: "bot"}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for an import ID without a slash")
	}
}

// --- Delete ---

func TestGrantDeleteIsIdempotentWhenAgentAlreadyGone(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "grant_set" {
			t.Fatalf("expected grant_set, got %q", op)
		}
		if roles, ok := args["roles"].([]any); !ok || len(roles) != 0 {
			t.Errorf("delete must send an empty roles list, got %v", args["roles"])
		}
		return nil, &fakeRPCError{Code: -32602, Message: `no such agent "bot" in this namespace`}
	}}
	client := testClient(t, f)
	res := NewGrantResource()
	configure(t, res, client)

	state := stateFor(t, res, &grantResourceModel{
		Agent: types.StringValue("bot"), App: types.StringValue("app1"),
		Allow: mustSet(t, "reader"), Approval: types.SetNull(types.StringType),
	})
	deleteResp := &resource.DeleteResponse{State: state}
	res.Delete(context.Background(), resource.DeleteRequest{State: state}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("Delete against an already-gone agent should succeed, got %v", deleteResp.Diagnostics)
	}
}

// --- test helpers local to this file ---

func mustSet(t *testing.T, values ...string) types.Set {
	t.Helper()
	set, diags := stringSetFrom(context.Background(), values)
	if diags.HasError() {
		t.Fatalf("stringSetFrom: %v", diags)
	}
	return set
}

func toStrings(values []any) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v.(string)
	}
	return out
}

// configFor builds a tfsdk.Config from a model value using res's schema, for ValidateConfig
// tests that don't go through a full Create/Update request. tfsdk.Config has no Set method (only
// tfsdk.Plan and tfsdk.State do), so the raw value is built via a throwaway Plan and copied over
// — Plan/Config/State share the same underlying schema-keyed encoding.
func configFor(t *testing.T, res resource.Resource, model any) tfsdk.Config {
	t.Helper()
	s := resourceSchema(t, res)
	plan := tfsdk.Plan{Schema: s}
	if diags := plan.Set(context.Background(), model); diags.HasError() {
		t.Fatalf("building config: %v", diags)
	}
	return tfsdk.Config{Schema: s, Raw: plan.Raw}
}
