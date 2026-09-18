// Package provider — pmcp_hub_settings: the singleton owner-wide hub execution settings §23.3
// pins in D1 (at most one row per owner, absent row meaning `30_000/30_000`). It is the
// declarative front over `hub_settings_get`/`hub_settings_update`; the same pair is reachable
// from the hub's `/settings/execution` pane and the CLI. Nothing here is bearer-scoped — the
// settings belong to the owner, and the credential in use only proves which owner that is.
package provider

import (
	"context"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = &hubSettingsResource{}
	_ resource.ResourceWithConfigure      = &hubSettingsResource{}
	_ resource.ResourceWithImportState    = &hubSettingsResource{}
	_ resource.ResourceWithValidateConfig = &hubSettingsResource{}
)

// §23.3's pinned numbers, restated here rather than fetched: the range is a compiled contract
// (the hub refuses anything outside it with a payload-free -32602), and the default pair is
// what an absent row means and what Delete restores. The upper bound is the hub's HARD maximum,
// not an owner setting — only `default_timeout_ms`/`max_timeout_ms` rows are configurable, and
// neither may exceed it.
const (
	hubSettingsMinTimeoutMs     = 1_000
	hubSettingsDefaultTimeoutMs = 30_000
	hubSettingsHardMaxMs        = 300_000
)

// NewHubSettingsResource returns the pmcp_hub_settings resource constructor, registered by the
// provider.
func NewHubSettingsResource() resource.Resource { return &hubSettingsResource{} }

// hubSettingsResource holds only the client handed in at Configure — no cache, matching every
// other resource in this package (§22.4, "no memoization").
type hubSettingsResource struct {
	client *pmcp.Client
}

// hubSettingsModel is pmcp_hub_settings' schema, one Go value per attribute. owner_id is not an
// input and never becomes one: the owner is whoever the credential authenticates as, which is
// also why no attribute here can name another namespace's settings.
type hubSettingsModel struct {
	OwnerID          types.String `tfsdk:"owner_id"`
	DefaultTimeoutMs types.Int64  `tfsdk:"default_timeout_ms"`
	MaxTimeoutMs     types.Int64  `tfsdk:"max_timeout_ms"`
}

func (r *hubSettingsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hub_settings"
}

func (r *hubSettingsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The owner's hub execution settings (§23.3): the default and " +
			"maximum wall-clock budget, in milliseconds, for one hub execution call. One row " +
			"per owner — a singleton, not a per-app setting. `hub_settings_update` writes both " +
			"values together; destroy restores the pinned `30000/30000` pair. Settings are " +
			"snapshotted when an execution is admitted, so changing them never extends a run " +
			"already in flight.",
		Attributes: map[string]schema.Attribute{
			"owner_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The authenticated owner this singleton belongs to: the " +
					"namespace `/api/whoami` reports, and the owner segment the hub's own admin " +
					"path embeds. Also the import identifier. The raw D1 user id never reaches " +
					"the provider or its state.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"default_timeout_ms": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Milliseconds an execution gets when its arguments omit " +
					"`timeout_ms`. 1,000–300,000, and never above `max_timeout_ms`.",
				Validators: []validator.Int64{timeoutRangeValidator{}},
			},
			"max_timeout_ms": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "The largest `timeout_ms` this owner may request. " +
					"1,000–300,000, and at least `default_timeout_ms`.",
				Validators: []validator.Int64{timeoutRangeValidator{}},
			},
		},
	}
}

// timeoutRangeValidator bounds each timeout attribute by §23.3's compiled range. The pair rule
// (`default <= max`) is ValidateConfig's, because no single-attribute validator can see the
// other value.
type timeoutRangeValidator struct{}

func (timeoutRangeValidator) Description(context.Context) string {
	return fmt.Sprintf("must be between %d and %d milliseconds", hubSettingsMinTimeoutMs, hubSettingsHardMaxMs)
}

func (v timeoutRangeValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (timeoutRangeValidator) ValidateInt64(_ context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueInt64()
	if value < hubSettingsMinTimeoutMs || value > hubSettingsHardMaxMs {
		resp.Diagnostics.AddAttributeError(req.Path, "Timeout out of range",
			fmt.Sprintf("%d ms is outside the %d–%d ms range §23.3 pins: the ceiling is "+
				"compiled, and both the default and the maximum are owner settings below it.",
				value, hubSettingsMinTimeoutMs, hubSettingsHardMaxMs))
	}
}

// ValidateConfig refuses a pair whose default exceeds its maximum. The hub refuses the same
// input with a payload-free -32602, but this runs at `tofu plan` and can name both attributes,
// which the hub's wire error deliberately does not.
func (r *hubSettingsResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg hubSettingsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.DefaultTimeoutMs.IsNull() || cfg.DefaultTimeoutMs.IsUnknown() ||
		cfg.MaxTimeoutMs.IsNull() || cfg.MaxTimeoutMs.IsUnknown() {
		return
	}
	if cfg.DefaultTimeoutMs.ValueInt64() > cfg.MaxTimeoutMs.ValueInt64() {
		resp.Diagnostics.AddAttributeError(path.Root("default_timeout_ms"), "Default exceeds maximum",
			fmt.Sprintf("default_timeout_ms (%d) must not exceed max_timeout_ms (%d): an "+
				"execution with no explicit timeout could otherwise never run at its own default.",
				cfg.DefaultTimeoutMs.ValueInt64(), cfg.MaxTimeoutMs.ValueInt64()))
	}
}

func (r *hubSettingsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*pmcp.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected resource configure type",
			fmt.Sprintf("expected *pmcp.Client, got %T. Report this issue to the provider developers.", req.ProviderData),
		)
		return
	}
	r.client = client
}

// model renders one settings result as state. owner_id comes from the credential's namespace —
// the identity this provider authenticated as — never from a configurable field, so state can
// never claim an owner the credential does not prove.
func (r *hubSettingsResource) model(settings pmcp.HubSettings) hubSettingsModel {
	return hubSettingsModel{
		OwnerID:          types.StringValue(r.client.Namespace),
		DefaultTimeoutMs: types.Int64Value(settings.DefaultTimeoutMs),
		MaxTimeoutMs:     types.Int64Value(settings.MaxTimeoutMs),
	}
}

// write sends both timeouts through `hub_settings_update` and returns the committed pair as
// state. The hub's answer is authoritative — it validates the pair itself — so state reflects
// what was stored, not merely what was requested.
func (r *hubSettingsResource) write(ctx context.Context, plan hubSettingsModel, diags *diag.Diagnostics) (hubSettingsModel, bool) {
	settings, err := r.client.HubSettingsUpdate(
		ctx, plan.DefaultTimeoutMs.ValueInt64(), plan.MaxTimeoutMs.ValueInt64())
	if err != nil {
		hubDiag(diags, "hub_settings_update", err)
		return hubSettingsModel{}, false
	}
	return r.model(settings), true
}

func (r *hubSettingsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hubSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	model, ok := r.write(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// Read refreshes from `hub_settings_get`, which always answers — an absent row IS the default
// pair — so there is no not-found state to remove.
func (r *hubSettingsResource) Read(ctx context.Context, _ resource.ReadRequest, resp *resource.ReadResponse) {
	settings, err := r.client.HubSettingsGet(ctx)
	if err != nil {
		hubDiag(&resp.Diagnostics, "hub_settings_get", err)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, r.model(settings))...)
}

func (r *hubSettingsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hubSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	model, ok := r.write(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// Delete restores §23.3's default pair rather than deleting a row: there is no
// `hub_settings_delete` op, and an absent row and an explicit `30000/30000` pair are
// indistinguishable to every reader, so the explicit write is both the honest and the only
// possible expression of "no longer managed here".
func (r *hubSettingsResource) Delete(ctx context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	if _, err := r.client.HubSettingsUpdate(ctx, hubSettingsDefaultTimeoutMs, hubSettingsDefaultTimeoutMs); err != nil {
		hubDiag(&resp.Diagnostics, "hub_settings_update", err)
	}
}

// ImportState imports by the authenticated owner id — the singleton's identity. The id must
// match whoami's namespace: this provider only ever speaks for one owner, and accepting any
// other string would put a namespace this credential does not own into state.
func (r *hubSettingsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID != r.client.Namespace {
		resp.Diagnostics.AddError("Wrong owner",
			fmt.Sprintf("pmcp_hub_settings is the singleton of the owner this provider is "+
				"authenticated as (%q); import it with that id.", r.client.Namespace))
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("owner_id"), req, resp)
}
