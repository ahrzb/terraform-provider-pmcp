package provider

import (
	"context"
	"testing"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// int64Ptr is a table-test convenience; decideHeadersPlan itself takes *int64 so nil is
// expressible without a helper at every call site, but a table's literal rows read better as
// int64Ptr(1) than as a local variable per row.
func int64Ptr(v int64) *int64 { return &v }

// TestDecideHeadersPlan covers every row of §22.2's transition table via the pure decision
// function, independent of the framework's request/response plumbing. Create's row ("no prior
// state to compare against") is deliberately absent: ModifyPlan never calls this function on a
// create plan (it returns before reaching it), so it is not decideHeadersPlan's row to cover.
func TestDecideHeadersPlan(t *testing.T) {
	tests := []struct {
		name           string
		auth           string
		pairConfigured bool
		planVersion    *int64
		appliedVersion *int64
		wantMark       bool
		wantErr        bool
	}{
		{
			name: "headers to headers, versions differ", auth: "headers", pairConfigured: true,
			planVersion: int64Ptr(2), appliedVersion: int64Ptr(1), wantMark: true,
		},
		{
			name: "headers to headers, versions equal", auth: "headers", pairConfigured: true,
			planVersion: int64Ptr(1), appliedVersion: int64Ptr(1), wantMark: false,
		},
		{
			name: "headers to oauth", auth: "oauth", pairConfigured: false,
			planVersion: nil, appliedVersion: int64Ptr(1), wantMark: true,
		},
		{
			name: "oauth to headers, pair configured", auth: "headers", pairConfigured: true,
			planVersion: int64Ptr(5), appliedVersion: nil, wantMark: true,
		},
		{
			name: "oauth to headers, pair absent", auth: "headers", pairConfigured: false,
			planVersion: nil, appliedVersion: nil, wantMark: false,
		},
		{
			name: "pair removed while applied version non-null", auth: "headers", pairConfigured: false,
			planVersion: nil, appliedVersion: int64Ptr(3), wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideHeadersPlan(tc.auth, tc.pairConfigured, tc.planVersion, tc.appliedVersion)
			if (got.Error != "") != tc.wantErr {
				t.Errorf("Error = %q, wantErr = %v", got.Error, tc.wantErr)
			}
			if !tc.wantErr && got.MarkApplyUnknown != tc.wantMark {
				t.Errorf("MarkApplyUnknown = %v, want %v", got.MarkApplyUnknown, tc.wantMark)
			}
		})
	}
}

// baseProxyModel is a fully-populated, valid proxyAppModel with every field a real Plan/State/
// Config needs a concrete value for — ModifyPlan tests vary only auth/headers/applied version
// per case, and Get() requires the target struct to account for every schema attribute.
func baseProxyModel() proxyAppModel {
	emptyRedact := types.MapValueMust(types.ListType{ElemType: types.StringType}, map[string]attr.Value{})
	return proxyAppModel{
		commonAppModel: commonAppModel{
			Slug:              types.StringValue("app1"),
			Name:              types.StringValue("app1"),
			Description:       types.StringValue(""),
			Archived:          types.BoolValue(false),
			Redact:            emptyRedact,
			RedactResults:     emptyRedact,
			LogBodies:         types.BoolValue(false),
			TypescriptAliases: types.ObjectNull(typescriptAliasesAttrTypes),
		},
		Endpoint:        types.StringValue("https://upstream.example/mcp"),
		Auth:            types.StringValue("headers"),
		ForwardIdentity: types.BoolValue(false),
		Roles:           types.MapValueMust(types.ObjectType{AttrTypes: roleFamiliesAttrTypes}, map[string]attr.Value{}),
		Capabilities:    types.SetNull(types.StringType),
		HeadersWo:       types.MapNull(types.StringType),
		HeadersVersion:  types.Int64Null(),
	}
}

// headersAppliedVersionUnknown reports whether resp.Plan marks headers_applied_version unknown
// — the mechanism ModifyPlan's doc comment describes as what schedules Update.
func headersAppliedVersionUnknown(t *testing.T, plan tfsdk.Plan) bool {
	t.Helper()
	var v types.Int64
	if diags := plan.GetAttribute(context.Background(), path.Root("headers_applied_version"), &v); diags.HasError() {
		t.Fatalf("reading headers_applied_version from plan: %v", diags)
	}
	return v.IsUnknown()
}

func TestProxyAppModifyPlanMarksAppliedVersionUnknownOnMismatch(t *testing.T) {
	res := NewProxyAppResource()
	ctx := context.Background()

	state := baseProxyModel()
	state.HeadersVersion = types.Int64Value(1)
	state.HeadersAppliedVersion = types.Int64Value(1)

	plan := baseProxyModel()
	plan.HeadersVersion = types.Int64Value(2)
	plan.HeadersAppliedVersion = types.Int64Value(1) // carried forward by UseStateForUnknown
	cfgModel := plan
	cfgModel.HeadersWo = types.MapValueMust(types.StringType, map[string]attr.Value{"Authorization": types.StringValue("Bearer new")})

	req := resource.ModifyPlanRequest{
		Config: configFor(t, res, &cfgModel),
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	res.(resource.ResourceWithModifyPlan).ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
	}
	if !headersAppliedVersionUnknown(t, resp.Plan) {
		t.Error("headers_applied_version should be marked unknown when headers_version differs from the applied witness")
	}
}

func TestProxyAppModifyPlanNoOpWhenVersionsEqual(t *testing.T) {
	res := NewProxyAppResource()
	ctx := context.Background()

	state := baseProxyModel()
	state.HeadersVersion = types.Int64Value(1)
	state.HeadersAppliedVersion = types.Int64Value(1)

	plan := baseProxyModel()
	plan.HeadersVersion = types.Int64Value(1)
	plan.HeadersAppliedVersion = types.Int64Value(1)
	cfgModel := plan
	cfgModel.HeadersWo = types.MapValueMust(types.StringType, map[string]attr.Value{"Authorization": types.StringValue("Bearer same")})

	req := resource.ModifyPlanRequest{
		Config: configFor(t, res, &cfgModel),
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	res.(resource.ResourceWithModifyPlan).ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
	}
	if headersAppliedVersionUnknown(t, resp.Plan) {
		t.Error("headers_applied_version must stay put when versions already match — out-of-band header changes are invisible by construction")
	}
}

func TestProxyAppModifyPlanErrorsWhenPairRemovedWithAppliedVersion(t *testing.T) {
	res := NewProxyAppResource()
	ctx := context.Background()

	state := baseProxyModel()
	state.HeadersVersion = types.Int64Value(1)
	state.HeadersAppliedVersion = types.Int64Value(1)

	plan := baseProxyModel()
	plan.HeadersVersion = types.Int64Null()
	plan.HeadersAppliedVersion = types.Int64Value(1)

	req := resource.ModifyPlanRequest{
		Config: configFor(t, res, &plan), // headers_wo/headers_version both absent
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	res.(resource.ResourceWithModifyPlan).ModifyPlan(ctx, req, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a plan-time error: the hub has no operation that clears a headers envelope")
	}
}

// emptyPlan mirrors agent_test.go's emptyState, for the one case that needs a destroy plan:
// Terraform 1.3+ plans a resource destroy with a fully-null plan value.
func emptyPlan(t *testing.T, res resource.Resource) tfsdk.Plan {
	t.Helper()
	s := resourceSchema(t, res)
	ctx := context.Background()
	return tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
}

func TestProxyAppModifyPlanAllowsDestroyDespitePairRemoved(t *testing.T) {
	res := NewProxyAppResource()
	ctx := context.Background()

	state := baseProxyModel()
	state.HeadersVersion = types.Int64Value(1)
	state.HeadersAppliedVersion = types.Int64Value(1)

	req := resource.ModifyPlanRequest{
		Config: configFor(t, res, &state),
		Plan:   emptyPlan(t, res),
		State:  stateFor(t, res, &state),
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	res.(resource.ResourceWithModifyPlan).ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("a destroy plan must never be refused for the headers pair being absent: %v", resp.Diagnostics)
	}
}

func TestProxyAppModifyPlanOauthToHeadersWithoutPairWarns(t *testing.T) {
	res := NewProxyAppResource()
	ctx := context.Background()

	state := baseProxyModel()
	state.Auth = types.StringValue("oauth")
	state.HeadersVersion = types.Int64Null()
	state.HeadersAppliedVersion = types.Int64Null()

	plan := baseProxyModel()
	plan.Auth = types.StringValue("headers")
	plan.HeadersVersion = types.Int64Null()
	plan.HeadersAppliedVersion = types.Int64Null()

	req := resource.ModifyPlanRequest{
		Config: configFor(t, res, &plan),
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	res.(resource.ResourceWithModifyPlan).ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
	}
	if headersAppliedVersionUnknown(t, resp.Plan) {
		t.Error("applied version was already null and stays null on this row; nothing to reconcile")
	}
	hasWarning := false
	for _, d := range resp.Diagnostics.Warnings() {
		if d.Summary() == "App will have no upstream credentials" {
			hasWarning = true
		}
	}
	if !hasWarning {
		t.Error("expected the §22.2 'legal, with a warning' notice for oauth -> headers with the pair absent")
	}
}

// paCall/paFakeHub mirror fakeHub/fakeCall from agent_test.go, prefixed "pa" (ProviderApps)
// only where this file needs its own reply shape; the shared fakeHub/testClient/planFor/
// stateFor/emptyState/configure helpers from agent_test.go are reused as-is.

func TestProxyAppCreateWithHeadersCallsCreateThenSetUpstreamAuth(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		switch op {
		case "app_create":
			return map[string]any{"app": pmcp.AppRow{
				Slug: "app1", Kind: "proxy", Name: "app1", Auth: "headers",
				Endpoint: "https://upstream.example/mcp", LogBodies: false,
				Roles: map[string]pmcp.RoleFamilies{}, Redact: map[string][]string{}, RedactResults: map[string][]string{},
			}}, nil
		case "app_set_upstream_auth":
			return map[string]any{"slug": "app1"}, nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}}
	client := testClient(t, f)
	res := NewProxyAppResource()
	configure(t, res, client)

	plan := baseProxyModel()
	plan.Name, plan.Description, plan.Archived = types.StringUnknown(), types.StringUnknown(), types.BoolUnknown()
	plan.LogBodies = types.BoolUnknown()
	plan.Redact = types.MapUnknown(types.ListType{ElemType: types.StringType})
	plan.RedactResults = types.MapUnknown(types.ListType{ElemType: types.StringType})
	plan.ForwardIdentity = types.BoolUnknown()
	plan.Roles = types.MapUnknown(types.ObjectType{AttrTypes: roleFamiliesAttrTypes})
	plan.Capabilities = types.SetUnknown(types.StringType)
	plan.HeadersVersion = types.Int64Value(1)
	plan.HeadersAppliedVersion = types.Int64Unknown()

	cfgModel := plan
	cfgModel.HeadersWo = types.MapValueMust(types.StringType, map[string]attr.Value{"Authorization": types.StringValue("Bearer secret")})

	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{
		Plan:   planFor(t, res, &plan),
		Config: configFor(t, res, &cfgModel),
	}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	if len(f.calls) != 2 || f.calls[0].Op != "app_create" || f.calls[1].Op != "app_set_upstream_auth" {
		var ops []string
		for _, c := range f.calls {
			ops = append(ops, c.Op)
		}
		t.Fatalf("op sequence = %v, want [app_create app_set_upstream_auth]", ops)
	}
	sentHeaders, _ := f.calls[1].Args["headers"].(map[string]any)
	if sentHeaders["Authorization"] != "Bearer secret" {
		t.Errorf("app_set_upstream_auth headers = %v, want the configured Authorization header", f.calls[1].Args["headers"])
	}

	var state proxyAppModel
	if diags := createResp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if state.HeadersAppliedVersion.ValueInt64() != 1 {
		t.Errorf("headers_applied_version = %v, want 1 after a successful app_set_upstream_auth", state.HeadersAppliedVersion)
	}
}

// TestProxyAppCreateSetUpstreamAuthFailureKeepsCreatedAppWithNullAppliedVersion guards §22.4's
// partial-apply rule against the exact silent gate the review flagged: if
// model.HeadersAppliedVersion = plan.HeadersVersion were ever assigned before knowing whether
// app_set_upstream_auth succeeds (instead of only on its success), this test — not the 51
// pre-existing ones — is what would catch it. On failure the app_create'd app must stay in
// state (a rejected credential push does not undo the app) while headers_applied_version stays
// null, so the next apply retries the credential instead of reporting false convergence.
func TestProxyAppCreateSetUpstreamAuthFailureKeepsCreatedAppWithNullAppliedVersion(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		switch op {
		case "app_create":
			return map[string]any{"app": pmcp.AppRow{
				Slug: "app1", Kind: "proxy", Name: "app1", Auth: "headers",
				Endpoint: "https://upstream.example/mcp", LogBodies: false,
				Roles: map[string]pmcp.RoleFamilies{}, Redact: map[string][]string{}, RedactResults: map[string][]string{},
			}}, nil
		case "app_set_upstream_auth":
			return nil, &fakeRPCError{Code: -32000, Message: "upstream unreachable"}
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}}
	client := testClient(t, f)
	res := NewProxyAppResource()
	configure(t, res, client)

	plan := baseProxyModel()
	plan.Name, plan.Description, plan.Archived = types.StringUnknown(), types.StringUnknown(), types.BoolUnknown()
	plan.LogBodies = types.BoolUnknown()
	plan.Redact = types.MapUnknown(types.ListType{ElemType: types.StringType})
	plan.RedactResults = types.MapUnknown(types.ListType{ElemType: types.StringType})
	plan.ForwardIdentity = types.BoolUnknown()
	plan.Roles = types.MapUnknown(types.ObjectType{AttrTypes: roleFamiliesAttrTypes})
	plan.Capabilities = types.SetUnknown(types.StringType)
	plan.HeadersVersion = types.Int64Value(1)
	plan.HeadersAppliedVersion = types.Int64Unknown()

	cfgModel := plan
	cfgModel.HeadersWo = types.MapValueMust(types.StringType, map[string]attr.Value{"Authorization": types.StringValue("Bearer secret")})

	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{
		Plan:   planFor(t, res, &plan),
		Config: configFor(t, res, &cfgModel),
	}, createResp)

	if !createResp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic when app_set_upstream_auth fails")
	}

	var state proxyAppModel
	if diags := createResp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if state.Slug.ValueString() != "app1" {
		t.Errorf("slug = %q, want the app_create'd app kept in state despite the later failure (§22.4 partial apply)", state.Slug.ValueString())
	}
	if !state.HeadersAppliedVersion.IsNull() {
		t.Errorf("headers_applied_version = %v, want null after a failed app_set_upstream_auth — the credential was never applied, so it must be retried", state.HeadersAppliedVersion)
	}
}

func TestProxyAppUpdateHeadersToOauthCallsAppUpdateOnlyAndClearsAppliedVersion(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_update" {
			t.Fatalf("headers -> oauth must call app_update only, got %q", op)
		}
		return map[string]any{"app": pmcp.AppRow{
			Slug: "app1", Kind: "proxy", Name: "app1", Auth: "oauth",
			Endpoint: "https://upstream.example/mcp",
			Roles:    map[string]pmcp.RoleFamilies{}, Redact: map[string][]string{}, RedactResults: map[string][]string{},
		}}, nil
	}}
	client := testClient(t, f)
	res := NewProxyAppResource()
	configure(t, res, client)

	state := baseProxyModel()
	state.HeadersVersion = types.Int64Value(1)
	state.HeadersAppliedVersion = types.Int64Value(1)

	plan := baseProxyModel()
	plan.Auth = types.StringValue("oauth")
	plan.HeadersVersion = types.Int64Null()
	plan.HeadersAppliedVersion = types.Int64Unknown() // ModifyPlan's mark, simulated directly

	updateResp := &resource.UpdateResponse{State: stateFor(t, res, &state)}
	res.Update(context.Background(), resource.UpdateRequest{
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
		Config: configFor(t, res, &plan),
	}, updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", updateResp.Diagnostics)
	}

	if len(f.calls) != 1 || f.calls[0].Op != "app_update" {
		t.Fatalf("expected exactly one app_update call, got %+v", f.calls)
	}

	var got proxyAppModel
	if diags := updateResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.HeadersAppliedVersion.IsNull() {
		t.Errorf("headers_applied_version = %v, want null after auth flips away from headers", got.HeadersAppliedVersion)
	}
}

func TestProxyAppUpdateOauthToHeadersOrdersAppUpdateBeforeSetUpstreamAuth(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		switch op {
		case "app_update":
			return map[string]any{"app": pmcp.AppRow{
				Slug: "app1", Kind: "proxy", Name: "app1", Auth: "headers",
				Endpoint: "https://upstream.example/mcp",
				Roles:    map[string]pmcp.RoleFamilies{}, Redact: map[string][]string{}, RedactResults: map[string][]string{},
			}}, nil
		case "app_set_upstream_auth":
			return map[string]any{"slug": "app1"}, nil
		default:
			t.Fatalf("unexpected op %q", op)
			return nil, nil
		}
	}}
	client := testClient(t, f)
	res := NewProxyAppResource()
	configure(t, res, client)

	state := baseProxyModel()
	state.Auth = types.StringValue("oauth")
	state.HeadersVersion = types.Int64Null()
	state.HeadersAppliedVersion = types.Int64Null()

	plan := baseProxyModel()
	plan.Auth = types.StringValue("headers")
	plan.HeadersVersion = types.Int64Value(1)
	plan.HeadersAppliedVersion = types.Int64Unknown()

	cfgModel := plan
	cfgModel.HeadersWo = types.MapValueMust(types.StringType, map[string]attr.Value{"Authorization": types.StringValue("Bearer x")})

	updateResp := &resource.UpdateResponse{State: stateFor(t, res, &state)}
	res.Update(context.Background(), resource.UpdateRequest{
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
		Config: configFor(t, res, &cfgModel),
	}, updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", updateResp.Diagnostics)
	}

	if len(f.calls) != 2 || f.calls[0].Op != "app_update" || f.calls[1].Op != "app_set_upstream_auth" {
		var ops []string
		for _, c := range f.calls {
			ops = append(ops, c.Op)
		}
		t.Fatalf("op sequence = %v, want [app_update app_set_upstream_auth] — app_update must wipe the oauth bundle first", ops)
	}
}

// TestProxyAppUpdateSetUpstreamAuthFailureRetainsPriorAppliedVersion is Update's half of the
// same partial-apply guard as the Create test above: headers_applied_version is §22.2's
// convergence witness, and it must advance only after app_set_upstream_auth actually succeeds.
// If the assignment ever moved above the err != nil check, a failed push would still persist
// the new version — reporting false convergence and preventing any retry.
func TestProxyAppUpdateSetUpstreamAuthFailureRetainsPriorAppliedVersion(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_set_upstream_auth" {
			t.Fatalf("a headers_version-only change must not call app_update, got %q", op)
		}
		return nil, &fakeRPCError{Code: -32000, Message: "upstream unreachable"}
	}}
	client := testClient(t, f)
	res := NewProxyAppResource()
	configure(t, res, client)

	state := baseProxyModel()
	state.HeadersVersion = types.Int64Value(1)
	state.HeadersAppliedVersion = types.Int64Value(1)

	plan := baseProxyModel()
	plan.HeadersVersion = types.Int64Value(2)
	plan.HeadersAppliedVersion = types.Int64Unknown() // ModifyPlan's mark, simulated directly

	cfgModel := plan
	cfgModel.HeadersWo = types.MapValueMust(types.StringType, map[string]attr.Value{"Authorization": types.StringValue("Bearer new")})

	updateResp := &resource.UpdateResponse{State: stateFor(t, res, &state)}
	res.Update(context.Background(), resource.UpdateRequest{
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
		Config: configFor(t, res, &cfgModel),
	}, updateResp)

	if !updateResp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic when app_set_upstream_auth fails")
	}

	var got proxyAppModel
	if diags := updateResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.HeadersAppliedVersion.ValueInt64() != 1 {
		t.Errorf("headers_applied_version = %v, want the prior witnessed value 1 retained after a failed app_set_upstream_auth", got.HeadersAppliedVersion)
	}
}

func TestProxyAppUpdateArchivedOnlyDoesNotCallAppUpdate(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "app_archive" {
			t.Fatalf("an apply touching only archived must not call %q", op)
		}
		return map[string]any{"slug": "app1"}, nil
	}}
	client := testClient(t, f)
	res := NewProxyAppResource()
	configure(t, res, client)

	state := baseProxyModel()
	state.Archived = types.BoolValue(false)

	plan := baseProxyModel()
	plan.Archived = types.BoolValue(true)

	updateResp := &resource.UpdateResponse{State: stateFor(t, res, &state)}
	res.Update(context.Background(), resource.UpdateRequest{
		Plan:   planFor(t, res, &plan),
		State:  stateFor(t, res, &state),
		Config: configFor(t, res, &plan),
	}, updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", updateResp.Diagnostics)
	}
	if len(f.calls) != 1 {
		t.Fatalf("op sequence = %+v, want exactly one app_archive call", f.calls)
	}

	var got proxyAppModel
	if diags := updateResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.Archived.ValueBool() {
		t.Error("archived should be true in state after app_archive succeeds")
	}
}
