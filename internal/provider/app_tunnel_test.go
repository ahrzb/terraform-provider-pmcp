package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// baseTunnelModel mirrors baseProxyModel in app_proxy_test.go: a fully-populated, valid
// tunnelAppModel so tests only vary the field under test.
func baseTunnelModel() tunnelAppModel {
	emptyRedact := types.MapValueMust(types.ListType{ElemType: types.StringType}, map[string]attr.Value{})
	return tunnelAppModel{commonAppModel: commonAppModel{
		Slug:              types.StringValue("bot1"),
		Name:              types.StringValue("bot1"),
		Description:       types.StringValue(""),
		Archived:          types.BoolValue(false),
		Redact:            emptyRedact,
		RedactResults:     emptyRedact,
		LogBodies:         types.BoolValue(true),
		TypescriptAliases: types.ObjectNull(typescriptAliasesAttrTypes),
	}, OwnerRoles: types.MapNull(types.ObjectType{AttrTypes: roleFamiliesAttrTypes})}
}

// toolRoles builds an `owner_roles` value of tools-only roles, as a configuration setting only
// `tools` plans once the other two families resolve to null.
func toolRoles(roles map[string][]string) types.Map {
	elements := make(map[string]attr.Value, len(roles))
	for name, patterns := range roles {
		tools := make([]attr.Value, len(patterns))
		for i, pattern := range patterns {
			tools[i] = types.StringValue(pattern)
		}
		elements[name] = types.ObjectValueMust(roleFamiliesAttrTypes, map[string]attr.Value{
			"tools":     types.ListValueMust(types.StringType, tools),
			"prompts":   types.ListNull(types.StringType),
			"resources": types.ListNull(types.StringType),
		})
	}
	return types.MapValueMust(types.ObjectType{AttrTypes: roleFamiliesAttrTypes}, elements)
}

// tunnelRow is an app_get/app_create/app_update tunnel row as the hub renders it, with
// ownerRoles in the canonical read shape given (a bare list for a tools-only role). A nil
// ownerRoles omits the key, as a hub predating the field does.
func tunnelRow(ownerRoles map[string]any) map[string]any {
	row := map[string]any{
		"slug": "bot1", "kind": "tunnel", "name": "bot1", "description": "", "logBodies": true,
		"roles": map[string]any{}, "redact": map[string]any{}, "redactResults": map[string]any{},
	}
	if ownerRoles != nil {
		row["ownerRoles"] = ownerRoles
	}
	return row
}

// jsonOf renders a recorded argument as JSON, whose sorted keys make the comparison
// order-independent and its failure message readable.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(encoded)
}

// TestTunnelAppCreateSendsOwnerRolesAndReadsCanonicalBack pins both directions of a create: the
// configured map reaches app_create as `owner_roles` in the object form, and the hub's canonical
// bare list for a tools-only role lands in state equal to the plan, so the next plan is empty.
func TestTunnelAppCreateSendsOwnerRolesAndReadsCanonicalBack(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_create" {
			t.Fatalf("expected app_create, got %q", op)
		}
		if got, want := jsonOf(t, args["owner_roles"]), `{"mine":{"tools":["get_.*"]}}`; got != want {
			t.Errorf("owner_roles = %s, want %s", got, want)
		}
		return map[string]any{"app": tunnelRow(map[string]any{"mine": []string{"get_.*"}})}, nil
	}}
	client := testClient(t, f)
	res := NewTunnelAppResource()
	configure(t, res, client)

	plan := baseTunnelModel()
	plan.OwnerRoles = toolRoles(map[string][]string{"mine": {"get_.*"}})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: planFor(t, res, &plan)}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	var got tunnelAppModel
	if diags := createResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.OwnerRoles.Equal(plan.OwnerRoles) {
		t.Errorf("state owner_roles = %v, want %v", got.OwnerRoles, plan.OwnerRoles)
	}
}

// TestTunnelAppCreateOmitsUnconfiguredOwnerRoles covers null's meaning at create: an attribute
// the configuration leaves out is unknown in the plan and must not reach app_create at all, and
// the hub's `{}` then lands in state as an empty map, not null.
func TestTunnelAppCreateOmitsUnconfiguredOwnerRoles(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if _, sent := args["owner_roles"]; sent {
			t.Errorf("an unconfigured owner_roles must not be sent, got %v", args["owner_roles"])
		}
		return map[string]any{"app": tunnelRow(map[string]any{})}, nil
	}}
	client := testClient(t, f)
	res := NewTunnelAppResource()
	configure(t, res, client)

	plan := baseTunnelModel()
	plan.OwnerRoles = types.MapUnknown(types.ObjectType{AttrTypes: roleFamiliesAttrTypes})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: planFor(t, res, &plan)}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	var got tunnelAppModel
	if diags := createResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.OwnerRoles.IsNull() || len(got.OwnerRoles.Elements()) != 0 {
		t.Errorf("state owner_roles = %v, want the hub's empty map", got.OwnerRoles)
	}
}

// TestTunnelAppUpdateOwnerRoles pins update's replace semantics. The hub stores whatever
// `owner_roles` carries as the whole map, so a change sends the complete new set, a clear sends
// `{}`, and an unchanged value (which is also what an unconfigured attribute plans, since
// UseStateForUnknown copies state) is never resent — resending it would overwrite, whole, an
// edit the web UI made since refresh.
func TestTunnelAppUpdateOwnerRoles(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state, plan types.Map
		description string
		// wantSent is the JSON owner_roles must carry, or "" when it must be absent.
		wantSent string
	}{
		{
			name:     "a change sends the complete new set",
			state:    toolRoles(map[string][]string{"mine": {"get_.*"}, "gone": {"x"}}),
			plan:     toolRoles(map[string][]string{"mine": {"get_.*", "list_.*"}}),
			wantSent: `{"mine":{"tools":["get_.*","list_.*"]}}`,
		},
		{
			name:     "a clear sends the empty map",
			state:    toolRoles(map[string][]string{"mine": {"get_.*"}}),
			plan:     toolRoles(map[string][]string{}),
			wantSent: `{}`,
		},
		{
			name:        "an unchanged set is not resent beside another change",
			state:       toolRoles(map[string][]string{"mine": {"get_.*"}}),
			plan:        toolRoles(map[string][]string{"mine": {"get_.*"}}),
			description: "renamed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
				if op != "app_update" {
					t.Fatalf("expected app_update, got %q", op)
				}
				sent, ok := args["owner_roles"]
				switch {
				case tc.wantSent == "" && ok:
					t.Errorf("owner_roles must not be sent, got %s", jsonOf(t, sent))
				case tc.wantSent != "" && jsonOf(t, sent) != tc.wantSent:
					t.Errorf("owner_roles = %s, want %s", jsonOf(t, sent), tc.wantSent)
				}
				row := tunnelRow(map[string]any{})
				if ok {
					row["ownerRoles"] = sent
				} else {
					row["ownerRoles"] = map[string]any{"mine": []string{"get_.*"}}
				}
				row["description"] = tc.description
				return map[string]any{"app": row}, nil
			}}
			client := testClient(t, f)
			res := NewTunnelAppResource()
			configure(t, res, client)

			state := baseTunnelModel()
			state.OwnerRoles = tc.state
			plan := baseTunnelModel()
			plan.OwnerRoles = tc.plan
			plan.Description = types.StringValue(tc.description)

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

			var got tunnelAppModel
			if diags := updateResp.State.Get(context.Background(), &got); diags.HasError() {
				t.Fatalf("State.Get: %v", diags)
			}
			if !got.OwnerRoles.Equal(tc.plan) {
				t.Errorf("state owner_roles = %v, want %v", got.OwnerRoles, tc.plan)
			}
		})
	}
}

// TestTunnelAppReadReportsOwnerRoles covers refresh: roles changed on the hub outside this
// configuration (the web UI's Roles pane writes the same column) replace state, so a configured
// attribute plans them away on the next apply; and a row with no `ownerRoles` key reads as null
// rather than an empty map claiming the hub reported one.
func TestTunnelAppReadReportsOwnerRoles(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  map[string]any
		want types.Map
	}{
		{
			name: "drift on the hub replaces state",
			row:  map[string]any{"mine": []string{"get_.*"}, "added": []string{"set_.*"}},
			want: toolRoles(map[string][]string{"mine": {"get_.*"}, "added": {"set_.*"}}),
		},
		{
			name: "an absent key reads as null",
			want: types.MapNull(types.ObjectType{AttrTypes: roleFamiliesAttrTypes}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHub{t: t, reply: func(op string, _ map[string]any) (any, *fakeRPCError) {
				if op != "app_get" {
					t.Fatalf("expected app_get, got %q", op)
				}
				return map[string]any{"app": tunnelRow(tc.row)}, nil
			}}
			client := testClient(t, f)
			res := NewTunnelAppResource()
			configure(t, res, client)

			state := baseTunnelModel()
			state.OwnerRoles = toolRoles(map[string][]string{"mine": {"get_.*"}})
			readResp := &resource.ReadResponse{State: stateFor(t, res, &state)}
			res.Read(context.Background(), resource.ReadRequest{State: stateFor(t, res, &state)}, readResp)
			if readResp.Diagnostics.HasError() {
				t.Fatalf("Read: %v", readResp.Diagnostics)
			}

			var got tunnelAppModel
			if diags := readResp.State.Get(context.Background(), &got); diags.HasError() {
				t.Fatalf("State.Get: %v", diags)
			}
			if !got.OwnerRoles.Equal(tc.want) {
				t.Errorf("state owner_roles = %v, want %v", got.OwnerRoles, tc.want)
			}
		})
	}
}

// TestTunnelAppImportReadsOwnerRoles covers import by slug: ImportState records only the slug,
// and the Read the framework runs next must fill `owner_roles` from the hub, so importing an app
// with owner roles and configuring the same roles plans nothing.
func TestTunnelAppImportReadsOwnerRoles(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_get" || args["slug"] != "bot1" {
			t.Fatalf("expected app_get for bot1, got %q %v", op, args)
		}
		return map[string]any{"app": tunnelRow(map[string]any{
			"mine": []string{"get_.*"}, "docs": map[string]any{"prompts": []string{"draft_.*"}},
		})}, nil
	}}
	client := testClient(t, f)
	res := NewTunnelAppResource()
	configure(t, res, client)

	importResp := &resource.ImportStateResponse{State: emptyState(t, res)}
	res.(resource.ResourceWithImportState).ImportState(context.Background(),
		resource.ImportStateRequest{ID: "bot1"}, importResp)
	if importResp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", importResp.Diagnostics)
	}
	readResp := &resource.ReadResponse{State: importResp.State}
	res.Read(context.Background(), resource.ReadRequest{State: importResp.State}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	var got tunnelAppModel
	if diags := readResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	want := types.MapValueMust(types.ObjectType{AttrTypes: roleFamiliesAttrTypes}, map[string]attr.Value{
		"mine": toolRoles(map[string][]string{"mine": {"get_.*"}}).Elements()["mine"],
		"docs": types.ObjectValueMust(roleFamiliesAttrTypes, map[string]attr.Value{
			"tools":     types.ListNull(types.StringType),
			"prompts":   types.ListValueMust(types.StringType, []attr.Value{types.StringValue("draft_.*")}),
			"resources": types.ListNull(types.StringType),
		}),
	})
	if !got.OwnerRoles.Equal(want) {
		t.Errorf("imported owner_roles = %v, want %v", got.OwnerRoles, want)
	}
}

// TestOwnerRolesOnlyOnTheTunnelSchema pins the kind rule at the schema: the hub refuses
// `owner_roles` on a proxied app, so pmcp_proxy_app must not offer it, which makes the refused
// request unrepresentable rather than an apply-time failure.
func TestOwnerRolesOnlyOnTheTunnelSchema(t *testing.T) {
	if _, ok := resourceSchema(t, NewTunnelAppResource()).Attributes["owner_roles"]; !ok {
		t.Error("pmcp_tunnel_app must offer owner_roles")
	}
	if _, ok := resourceSchema(t, NewProxyAppResource()).Attributes["owner_roles"]; ok {
		t.Error("pmcp_proxy_app must not offer owner_roles; the hub refuses it on a proxied app")
	}
}

// TestTunnelAppCreateSendsKindTunnel asserts app_create is invoked with kind = "tunnel".
// tunnelAppModel has no endpoint/auth/forward_identity/roles/capabilities fields and
// appendCommonArgs cannot emit them, so a loop asserting their absence from args can never
// fail — that assertion belongs to appendCommonArgs's own field set, not here.
func TestTunnelAppCreateSendsKindTunnel(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_create" {
			t.Fatalf("expected app_create, got %q", op)
		}
		if args["kind"] != "tunnel" {
			t.Errorf(`kind = %v, want "tunnel"`, args["kind"])
		}
		return map[string]any{"app": pmcp.AppRow{
			Slug: "bot1", Kind: "tunnel", Name: "bot1", LogBodies: true,
			Roles: map[string]pmcp.RoleFamilies{}, Redact: map[string][]string{}, RedactResults: map[string][]string{},
		}}, nil
	}}
	client := testClient(t, f)
	res := NewTunnelAppResource()
	configure(t, res, client)

	plan := baseTunnelModel()
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: planFor(t, res, &plan)}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}
}

// TestTunnelAppUpdateArchivedOnlyDoesNotCallAppUpdate mirrors the proxy resource's equivalent
// test: the same commonAppChanged gating exists in both files, and each is its own regression
// point since neither Update method calls into the other.
func TestTunnelAppUpdateArchivedOnlyDoesNotCallAppUpdate(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_unarchive" {
			t.Fatalf("an apply touching only archived must not call %q", op)
		}
		return map[string]any{"slug": "bot1"}, nil
	}}
	client := testClient(t, f)
	res := NewTunnelAppResource()
	configure(t, res, client)

	state := baseTunnelModel()
	state.Archived = types.BoolValue(true)

	plan := baseTunnelModel()
	plan.Archived = types.BoolValue(false)

	updateResp := &resource.UpdateResponse{State: stateFor(t, res, &state)}
	res.Update(context.Background(), resource.UpdateRequest{
		Plan:  planFor(t, res, &plan),
		State: stateFor(t, res, &state),
	}, updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", updateResp.Diagnostics)
	}
	if len(f.calls) != 1 {
		t.Fatalf("op sequence = %+v, want exactly one app_unarchive call", f.calls)
	}

	var got tunnelAppModel
	if diags := updateResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.Archived.ValueBool() {
		t.Error("archived should be false in state after app_unarchive succeeds")
	}
}

func TestTunnelAppDeleteIsIdempotentOnNotFound(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_delete" {
			t.Fatalf("expected app_delete, got %q", op)
		}
		return nil, &fakeRPCError{Code: -32602, Message: `no such app "bot1" in this namespace`}
	}}
	client := testClient(t, f)
	res := NewTunnelAppResource()
	configure(t, res, client)

	state := baseTunnelModel()
	deleteResp := &resource.DeleteResponse{}
	res.Delete(context.Background(), resource.DeleteRequest{State: stateFor(t, res, &state)}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("Delete on an already-gone app must succeed, got: %v", deleteResp.Diagnostics)
	}
}
