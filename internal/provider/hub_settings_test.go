package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The settings resource is the one place a provider call WRITES something owner-wide rather
// than app-scoped, so these tests pin three things the §23.3 contract makes load-bearing: the
// pair travels as two JSON integers under the contract's own snake_case names, `owner_id` comes
// from the credential rather than any attribute, and Delete restores the documented default pair
// instead of removing state it cannot describe.

// TestHubSettingsCreateWritesBothTimeoutsOnce covers Create: one `hub_settings_update` carrying
// exactly the contract's two fields, and a state built from the hub's committed answer plus the
// authenticated owner.
func TestHubSettingsCreateWritesBothTimeoutsOnce(t *testing.T) {
	var sent map[string]any
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "hub_settings_update" {
			t.Fatalf("expected hub_settings_update, got %q", op)
		}
		sent = args
		return map[string]any{"settings": map[string]any{
			"defaultTimeoutMs": 45000,
			"maxTimeoutMs":     120000,
		}}, nil
	}}
	client := testClient(t, f)
	res := NewHubSettingsResource()
	configure(t, res, client)

	plan := hubSettingsModel{
		OwnerID:          types.StringUnknown(),
		DefaultTimeoutMs: types.Int64Value(45000),
		MaxTimeoutMs:     types.Int64Value(120000),
	}
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: planFor(t, res, &plan)}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	if len(sent) != 2 {
		t.Errorf("args = %v, want exactly the contract's two fields", sent)
	}
	if got, ok := sent["default_timeout_ms"].(float64); !ok || got != 45000 {
		t.Errorf("default_timeout_ms = %#v, want the number 45000", sent["default_timeout_ms"])
	}
	if got, ok := sent["max_timeout_ms"].(float64); !ok || got != 120000 {
		t.Errorf("max_timeout_ms = %#v, want the number 120000", sent["max_timeout_ms"])
	}

	var got hubSettingsModel
	if diags := createResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.OwnerID.ValueString() != "owner" {
		t.Errorf("owner_id = %q, want the authenticated namespace %q", got.OwnerID.ValueString(), "owner")
	}
	if got.DefaultTimeoutMs.ValueInt64() != 45000 || got.MaxTimeoutMs.ValueInt64() != 120000 {
		t.Errorf("state pair = %d/%d, want the committed 45000/120000",
			got.DefaultTimeoutMs.ValueInt64(), got.MaxTimeoutMs.ValueInt64())
	}
}

// TestHubSettingsReadCallsGetOnly covers Read: `hub_settings_get` takes the empty object, and
// the response always answers — an absent row is the default pair, so there is no not-found path
// that could remove the resource from state.
func TestHubSettingsReadCallsGetOnly(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "hub_settings_get" {
			t.Fatalf("expected hub_settings_get, got %q", op)
		}
		if len(args) != 0 {
			t.Errorf("args = %v, want the empty object", args)
		}
		return map[string]any{"settings": map[string]any{
			"defaultTimeoutMs": 30000,
			"maxTimeoutMs":     90000,
		}}, nil
	}}
	client := testClient(t, f)
	res := NewHubSettingsResource()
	configure(t, res, client)

	state := hubSettingsModel{
		OwnerID:          types.StringValue("owner"),
		DefaultTimeoutMs: types.Int64Value(60000),
		MaxTimeoutMs:     types.Int64Value(120000),
	}
	readResp := &resource.ReadResponse{State: stateFor(t, res, &state)}
	res.Read(context.Background(), resource.ReadRequest{State: stateFor(t, res, &state)}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	var got hubSettingsModel
	if diags := readResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.DefaultTimeoutMs.ValueInt64() != 30000 || got.MaxTimeoutMs.ValueInt64() != 90000 {
		t.Errorf("state pair = %d/%d, want the hub's 30000/90000",
			got.DefaultTimeoutMs.ValueInt64(), got.MaxTimeoutMs.ValueInt64())
	}
}

// TestHubSettingsDeleteRestoresDefaultPair pins §23.3's destroy rule: there is no
// `hub_settings_delete` op, so Delete has to write the documented default pair back.
func TestHubSettingsDeleteRestoresDefaultPair(t *testing.T) {
	var sent map[string]any
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "hub_settings_update" {
			t.Fatalf("expected hub_settings_update, got %q", op)
		}
		sent = args
		return map[string]any{"settings": map[string]any{
			"defaultTimeoutMs": 30000,
			"maxTimeoutMs":     30000,
		}}, nil
	}}
	client := testClient(t, f)
	res := NewHubSettingsResource()
	configure(t, res, client)

	state := hubSettingsModel{
		OwnerID:          types.StringValue("owner"),
		DefaultTimeoutMs: types.Int64Value(60000),
		MaxTimeoutMs:     types.Int64Value(300000),
	}
	deleteResp := &resource.DeleteResponse{State: stateFor(t, res, &state)}
	res.Delete(context.Background(), resource.DeleteRequest{State: stateFor(t, res, &state)}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("Delete: %v", deleteResp.Diagnostics)
	}

	if got, ok := sent["default_timeout_ms"].(float64); !ok || got != 30000 {
		t.Errorf("default_timeout_ms = %#v, want 30000", sent["default_timeout_ms"])
	}
	if got, ok := sent["max_timeout_ms"].(float64); !ok || got != 30000 {
		t.Errorf("max_timeout_ms = %#v, want 30000", sent["max_timeout_ms"])
	}
}

// TestHubSettingsImportStateRequiresTheAuthenticatedOwner covers the singleton identity: the
// import id is the owner this provider authenticated as, and any other string is refused rather
// than written into state as if it named something this credential owns.
func TestHubSettingsImportStateRequiresTheAuthenticatedOwner(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, _ map[string]any) (any, *fakeRPCError) {
		t.Fatalf("import must not call %q", op)
		return nil, nil
	}}
	client := testClient(t, f)
	res := NewHubSettingsResource()
	configure(t, res, client)

	okResp := &resource.ImportStateResponse{State: emptyState(t, res)}
	importer := res.(resource.ResourceWithImportState)
	importer.ImportState(context.Background(), resource.ImportStateRequest{ID: "owner"}, okResp)
	if okResp.Diagnostics.HasError() {
		t.Fatalf("ImportState(owner): %v", okResp.Diagnostics)
	}
	var got hubSettingsModel
	if diags := okResp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.OwnerID.ValueString() != "owner" {
		t.Errorf("owner_id = %q, want %q", got.OwnerID.ValueString(), "owner")
	}

	badResp := &resource.ImportStateResponse{State: emptyState(t, res)}
	importer.ImportState(context.Background(), resource.ImportStateRequest{ID: "somebody-else"}, badResp)
	if !badResp.Diagnostics.HasError() {
		t.Error("importing another owner's namespace must be an error, not a silent passthrough")
	}
}

// TestHubSettingsValidateConfigRejectsDefaultAboveMax covers the pair rule, which no
// single-attribute validator can see. The hub refuses the same input with a payload-free -32602;
// this is the plan-time version that can name both attributes.
func TestHubSettingsValidateConfigRejectsDefaultAboveMax(t *testing.T) {
	for _, tc := range []struct {
		name      string
		defaultMs int64
		maxMs     int64
		wantErr   bool
	}{
		{name: "equal is legal", defaultMs: 30000, maxMs: 30000},
		{name: "default below max is legal", defaultMs: 1000, maxMs: 300000},
		{name: "default above max is refused", defaultMs: 60000, maxMs: 30000, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := NewHubSettingsResource()
			cfg := hubSettingsModel{
				OwnerID:          types.StringNull(),
				DefaultTimeoutMs: types.Int64Value(tc.defaultMs),
				MaxTimeoutMs:     types.Int64Value(tc.maxMs),
			}
			resp := &resource.ValidateConfigResponse{}
			res.(resource.ResourceWithValidateConfig).ValidateConfig(context.Background(),
				resource.ValidateConfigRequest{Config: configFor(t, res, &cfg)}, resp)
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Errorf("HasError = %v, want %v (%v)", resp.Diagnostics.HasError(), tc.wantErr, resp.Diagnostics)
			}
		})
	}
}

// TestHubSettingsTimeoutRangeValidator pins §23.3's compiled bounds at both ends, including the
// off-by-one rows a "reasonable range" refactor would quietly drop.
func TestHubSettingsTimeoutRangeValidator(t *testing.T) {
	for _, tc := range []struct {
		value   int64
		wantErr bool
	}{
		{value: 1000},
		{value: 999, wantErr: true},
		{value: 300000},
		{value: 300001, wantErr: true},
	} {
		resp := &validator.Int64Response{}
		timeoutRangeValidator{}.ValidateInt64(context.Background(), validator.Int64Request{
			Path:        path.Root("default_timeout_ms"),
			ConfigValue: types.Int64Value(tc.value),
		}, resp)
		if resp.Diagnostics.HasError() != tc.wantErr {
			t.Errorf("%d: HasError = %v, want %v", tc.value, resp.Diagnostics.HasError(), tc.wantErr)
		}
	}
}

// TestHubSettingsSchemaCarriesOnlyTheOwnerPair is the "metadata-only state" check: the
// resource's whole surface is the identity plus the two timeouts, and nothing on it is marked
// sensitive — there is no token, no source, and no upstream credential to leak into state.
func TestHubSettingsSchemaCarriesOnlyTheOwnerPair(t *testing.T) {
	res := NewHubSettingsResource()
	sch := resourceSchema(t, res)

	want := map[string]bool{"owner_id": true, "default_timeout_ms": true, "max_timeout_ms": true}
	for name := range sch.Attributes {
		if !want[name] {
			t.Errorf("unexpected attribute %q", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("missing attribute %q", name)
	}
	for name, attr := range sch.Attributes {
		if s, ok := attr.(schema.StringAttribute); ok && s.Sensitive {
			t.Errorf("%q is sensitive; the settings singleton must carry nothing secret", name)
		}
		if b, ok := attr.(schema.Int64Attribute); ok && b.Sensitive {
			t.Errorf("%q is sensitive; timeouts are not secrets", name)
		}
	}
}

// TestNewHubSettingsResourceIsRegisteredWithTheProvider guards provider.go's stated failure
// mode — an implemented-but-unregistered type never reaches Terraform at all, and no schema or
// lifecycle test can see that.
func TestNewHubSettingsResourceIsRegisteredWithTheProvider(t *testing.T) {
	regs := New("test")().(*hubProvider).Resources(context.Background())
	names := make(map[string]bool, len(regs))
	for _, reg := range regs {
		var resp resource.MetadataResponse
		reg().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "pmcp"}, &resp)
		names[resp.TypeName] = true
	}
	if !names["pmcp_hub_settings"] {
		t.Errorf("registered resources = %v, want pmcp_hub_settings among them", names)
	}
}
