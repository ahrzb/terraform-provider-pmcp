package provider

import (
	"context"
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
		Slug:          types.StringValue("bot1"),
		Name:          types.StringValue("bot1"),
		Description:   types.StringValue(""),
		Archived:      types.BoolValue(false),
		Redact:        emptyRedact,
		RedactResults: emptyRedact,
		LogBodies:     types.BoolValue(true),
	}}
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
