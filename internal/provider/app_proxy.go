// Package provider: pmcp_proxy_app manages a proxied app — one the hub forwards calls to over
// its own outbound connection. See §22.2 (upstream headers) and §22.4 (schema/lifecycle) of the
// hub's design spec.
package provider

import (
	"context"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = &proxyAppResource{}
	_ resource.ResourceWithConfigure      = &proxyAppResource{}
	_ resource.ResourceWithImportState    = &proxyAppResource{}
	_ resource.ResourceWithValidateConfig = &proxyAppResource{}
	_ resource.ResourceWithModifyPlan     = &proxyAppResource{}
)

// NewProxyAppResource returns the pmcp_proxy_app resource constructor, registered by the provider.
func NewProxyAppResource() resource.Resource { return &proxyAppResource{} }

// proxyAppResource holds only the client handed in at Configure — no cache, matching every
// other resource in this package (§22.4, "no memoization").
type proxyAppResource struct {
	client *pmcp.Client
}

// proxyAppModel is commonAppModel plus §22.4's proxy-only attributes and §22.2's headers state
// machine. HeadersWo is always null once read back — the framework nulls every write-only
// attribute in the plan and in Create/Update's response state — so it exists on this struct
// only so Get/Set have somewhere to route it; nothing here ever inspects its value except
// headersWoToArgs, which reads it from request Config specifically because Plan/State never
// carry it.
type proxyAppModel struct {
	commonAppModel
	Endpoint              types.String `tfsdk:"endpoint"`
	Auth                  types.String `tfsdk:"auth"`
	ForwardIdentity       types.Bool   `tfsdk:"forward_identity"`
	Roles                 types.Map    `tfsdk:"roles"`
	Capabilities          types.Set    `tfsdk:"capabilities"`
	HeadersWo             types.Map    `tfsdk:"headers_wo"`
	HeadersVersion        types.Int64  `tfsdk:"headers_version"`
	HeadersAppliedVersion types.Int64  `tfsdk:"headers_applied_version"`
}

// proxyModelFromRow overlays app_get/app_create/app_update's row onto `base`, leaving the
// headers trio (HeadersWo/HeadersVersion/HeadersAppliedVersion) exactly as `base` had them:
// the row never carries any of the three, so a caller passing the current state as `base`
// carries the operator's configured intent and the convergence witness forward untouched.
func proxyModelFromRow(ctx context.Context, base proxyAppModel, row pmcp.AppRow) (proxyAppModel, diag.Diagnostics) {
	common, diags := commonFromRow(ctx, row)

	roles, err := rolesFrom(ctx, row.Roles)
	if err != nil {
		diags.AddError("Could not convert roles", err.Error())
	}

	var capabilities []string
	if row.Capabilities != nil {
		capabilities = *row.Capabilities
	}
	capSet, d := stringSetFrom(ctx, capabilities)
	diags.Append(d...)

	base.commonAppModel = common
	base.Endpoint = types.StringValue(row.Endpoint)
	base.Auth = types.StringValue(row.Auth)
	base.ForwardIdentity = types.BoolValue(row.ForwardIdentity)
	base.Roles = roles
	base.Capabilities = capSet
	return base, diags
}

func (r *proxyAppResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_proxy_app"
}

func (r *proxyAppResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := commonAppAttributes()
	attrs["endpoint"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "The upstream MCP endpoint URL the hub forwards calls to.",
	}
	attrs["auth"] = schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "`headers` or `oauth`. Defaults to `headers`. Flipping this wipes " +
			"any stored upstream credential in the same write (§22.2).",
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		Validators:    []validator.String{authValueValidator{}},
	}
	attrs["forward_identity"] = schema.BoolAttribute{
		Optional:            true,
		Computed:            true,
		MarkdownDescription: "Sends `X-Pmcp-*` identity headers upstream. Defaults to `false`.",
		PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
	}
	attrs["roles"] = rolesAttribute()
	attrs["capabilities"] = capabilitiesAttribute()
	attrs["headers_wo"] = schema.MapAttribute{
		ElementType: types.StringType,
		Optional:    true,
		Sensitive:   true,
		WriteOnly:   true,
		MarkdownDescription: "Static upstream headers, name → value, sent with every forwarded " +
			"call. Write-only: null in plan and state always, readable only from the request " +
			"that configured it. Co-required with `headers_version` — configure both or " +
			"neither (§22.2).",
	}
	attrs["headers_version"] = schema.Int64Attribute{
		Optional: true,
		MarkdownDescription: "The operator's intent: bump this whenever `headers_wo` changes, " +
			"so the provider can tell a real edit from an unrelated apply. Co-required with " +
			"`headers_wo`.",
	}
	attrs["headers_applied_version"] = schema.Int64Attribute{
		Computed: true,
		MarkdownDescription: "The `headers_version` that `app_set_upstream_auth` last applied " +
			"successfully. Null under `auth = \"oauth\"`, or if headers have never been set. " +
			"This is the convergence witness ModifyPlan compares against `headers_version` — " +
			"see its doc comment for the mechanism.",
		PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A proxied app: the hub forwards calls to an upstream MCP " +
			"endpoint over its own connection. See §22.2 and §22.4 of the hub's design spec.",
		Attributes: attrs,
	}
}

func (r *proxyAppResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig enforces §22.2's two config-shape rules that need only this resource's own
// configuration: headers_wo/headers_version are co-required (both or neither), and headers_wo
// is rejected outright under auth = "oauth" (an oauth-mode app authenticates through the
// browser Connect flow, not static headers). The third rule — removing the pair while a prior
// version is still witnessed applied — needs prior STATE, which ValidateConfig never has;
// ModifyPlan enforces that one instead.
func (r *proxyAppResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg proxyAppModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	woSet := !cfg.HeadersWo.IsNull()
	versionSet := !cfg.HeadersVersion.IsNull()
	if woSet != versionSet {
		resp.Diagnostics.AddError(
			"headers_wo and headers_version are co-required",
			"Configure both `headers_wo` and `headers_version`, or neither (§22.2). Setting "+
				"one without the other leaves the operator's intent ambiguous.",
		)
	}

	auth := "headers"
	if !cfg.Auth.IsNull() && !cfg.Auth.IsUnknown() {
		auth = cfg.Auth.ValueString()
	}
	if auth == "oauth" && woSet {
		resp.Diagnostics.AddAttributeError(
			path.Root("headers_wo"),
			`headers_wo is not valid under auth = "oauth"`,
			"An oauth-mode app authenticates through the browser Connect flow, not static "+
				"headers. Remove headers_wo/headers_version, or set auth back to \"headers\".",
		)
	}
}

// ModifyPlan is what makes §22.2's convergence machinery work. A computed attribute differing
// from a configured one produces no diff by itself — OpenTofu does not compare sibling values
// to invent work, and would retain headers_applied_version's prior state and schedule nothing.
// So this method compares the operator's current intent (headers_version, read from Config
// because it is co-required with the write-only headers_wo) against the last witnessed success
// (headers_applied_version, read from prior State) and marks the latter unknown in the plan
// whenever they differ — which produces the diff that schedules Update. Every §22.2 transition
// row reduces to that one comparison: a genuine version bump, oauth -> headers picking up a
// newly configured pair, and headers -> oauth needing the witness to become null all show up as
// planVersion != appliedVersion; "versions equal" and every oauth<->oauth case do not.
func (r *proxyAppResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy plan: nothing to reconcile
	}

	var cfg proxyAppModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var plan proxyAppModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown auth or headers_version depends on a value not yet known (e.g. another
	// resource's attribute); defer the decision to the plan that resolves it.
	if plan.Auth.IsUnknown() || cfg.HeadersVersion.IsUnknown() {
		return
	}
	auth := plan.Auth.ValueString()
	pairConfigured := !cfg.HeadersWo.IsNull() || !cfg.HeadersVersion.IsNull()
	var planVersion *int64
	if !cfg.HeadersVersion.IsNull() {
		v := cfg.HeadersVersion.ValueInt64()
		planVersion = &v
	}

	if req.State.Raw.IsNull() {
		// Create: headers_applied_version has no prior state to compare against — it is
		// already unknown, and Create resolves it once app_set_upstream_auth (if any) settles.
		if auth == "headers" && !pairConfigured {
			resp.Diagnostics.AddWarning(
				"App will have no upstream credentials",
				`auth = "headers" with neither headers_wo nor headers_version configured. `+
					"The app is created but forwards no upstream authentication until both are set.",
			)
		}
		return
	}

	var state proxyAppModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	priorAuth := state.Auth.ValueString()

	if priorAuth == "headers" && auth == "oauth" {
		resp.Diagnostics.AddWarning(
			"App will be disconnected",
			`Flipping auth from "headers" to "oauth" wipes the stored headers envelope. The `+
				"app is not_connected until a human runs the browser Connect flow.",
		)
	}
	if priorAuth == "oauth" && auth == "headers" && !pairConfigured {
		resp.Diagnostics.AddWarning(
			"App will have no upstream credentials",
			`auth is becoming "headers" but neither headers_wo nor headers_version is `+
				"configured. The app forwards no upstream authentication until both are set.",
		)
	}

	var appliedVersion *int64
	if !state.HeadersAppliedVersion.IsNull() {
		v := state.HeadersAppliedVersion.ValueInt64()
		appliedVersion = &v
	}

	decision := decideHeadersPlan(auth, pairConfigured, planVersion, appliedVersion)
	if decision.Error != "" {
		resp.Diagnostics.AddAttributeError(path.Root("headers_version"), "Cannot clear the applied headers envelope", decision.Error)
		return
	}
	if decision.MarkApplyUnknown {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("headers_applied_version"), types.Int64Unknown())...)
	}
}

// headersDecision is ModifyPlan's verdict for one instance of §22.2's transition table, kept
// separate from the framework's request/response plumbing so every row is a plain Go value a
// test can construct directly rather than a full tfsdk.Plan/Config/State.
type headersDecision struct {
	// MarkApplyUnknown, true, means headers_applied_version must be marked unknown in the
	// plan so OpenTofu schedules Update — see ModifyPlan's doc comment for why a computed
	// attribute differing from a configured one produces no diff on its own.
	MarkApplyUnknown bool
	// Error, non-empty, means the plan itself must be refused: the pair was removed while
	// headers mode never left "headers" and a prior version is still witnessed applied. The
	// hub has no operation that clears a stored headers envelope.
	Error string
}

// decideHeadersPlan implements §22.2's transition table as one comparison: the operator's
// current intent (planVersion — the configured headers_version, or nil under oauth or a
// bare-app config) against the last witnessed success (appliedVersion, or nil if headers have
// never been applied). auth is the PLANNED auth mode ("headers" or "oauth"); pairConfigured is
// whether headers_wo/headers_version are present in Config — ValidateConfig has already
// guaranteed they are both-or-neither, so this function never sees exactly one of them set.
// Create (no prior state) is not this function's concern: a brand-new resource's
// headers_applied_version is already unknown with nothing to compare against.
func decideHeadersPlan(auth string, pairConfigured bool, planVersion, appliedVersion *int64) headersDecision {
	if auth == "headers" && !pairConfigured && appliedVersion != nil {
		return headersDecision{Error: "the headers_wo/headers_version pair was removed, but a " +
			"version is still recorded as applied. The hub has no operation that clears a " +
			`stored headers envelope: flip auth to "oauth" to wipe it, or destroy the app.`}
	}
	if !int64PtrEqual(planVersion, appliedVersion) {
		return headersDecision{MarkApplyUnknown: true}
	}
	return headersDecision{}
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// headersWoToArgs reads headers_wo from request Config — the only place §22.2 lets a write-only
// attribute's value be read; Plan and State always carry it null. Returns (nil, nil) if the
// attribute is null or unknown, matching convert.go's "leave it alone" convention, though
// callers here only reach this once ModifyPlan/ValidateConfig have already established the pair
// is present.
func headersWoToArgs(ctx context.Context, config tfsdk.Config) (map[string]string, diag.Diagnostics) {
	var headersWo types.Map
	diags := config.GetAttribute(ctx, path.Root("headers_wo"), &headersWo)
	if diags.HasError() || headersWo.IsNull() || headersWo.IsUnknown() {
		return nil, diags
	}
	headers := make(map[string]string, len(headersWo.Elements()))
	diags.Append(headersWo.ElementsAs(ctx, &headers, false)...)
	return headers, diags
}

// appendProxyArgs adds the proxy-only fields to an app_create/app_update argument map, using
// whatever the plan already resolved — the same unconditional-send convention
// appendCommonArgs uses. `endpoint` is Required, so it is always known and always sent.
func appendProxyArgs(ctx context.Context, args map[string]any, plan proxyAppModel, diags *diag.Diagnostics) {
	args["endpoint"] = plan.Endpoint.ValueString()
	if auth := optionalString(plan.Auth); auth != nil {
		args["auth"] = *auth
	}
	if forwardIdentity := optionalBool(plan.ForwardIdentity); forwardIdentity != nil {
		args["forward_identity"] = *forwardIdentity
	}
	roles, d := rolesToArgs(ctx, plan.Roles)
	diags.Append(d...)
	if roles != nil {
		args["roles"] = roles
	}
	capabilities, d2 := stringSetTo(ctx, plan.Capabilities)
	diags.Append(d2...)
	if capabilities != nil {
		args["capabilities"] = capabilities
	}
}

// proxyAppChanged reports whether any field app_update patches — commonAppModel's fields, plus
// every proxy-only one except the headers trio and archived, which have their own ops — differs
// between plan and state. Update gates its app_update call on this for the same reason
// commonAppChanged exists: an apply that only touches archived or headers must not also resend
// (and thereby misrepresent as touched, in the hub's own audit row) every other field.
func proxyAppChanged(plan, state proxyAppModel) bool {
	return commonAppChanged(plan.commonAppModel, state.commonAppModel) ||
		!plan.Endpoint.Equal(state.Endpoint) ||
		!plan.Auth.Equal(state.Auth) ||
		!plan.ForwardIdentity.Equal(state.ForwardIdentity) ||
		!plan.Roles.Equal(state.Roles) ||
		!plan.Capabilities.Equal(state.Capabilities)
}

func (r *proxyAppResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan proxyAppModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	args := map[string]any{"slug": plan.Slug.ValueString(), "kind": "proxy"}
	appendCommonArgs(ctx, args, plan.commonAppModel, &resp.Diagnostics)
	appendProxyArgs(ctx, args, plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := r.client.AppCreate(ctx, args)
	if err != nil {
		hubDiag(&resp.Diagnostics, "app_create", err)
		return
	}

	model, diags := proxyModelFromRow(ctx, proxyAppModel{}, row)
	resp.Diagnostics.Append(diags...)
	model.HeadersWo = types.MapNull(types.StringType)
	model.HeadersVersion = plan.HeadersVersion
	model.HeadersAppliedVersion = types.Int64Null()
	// Reflect the created app first — partial-apply safety (§22.4): if either step below
	// fails, the state already returned here is not lost.
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// §22.2, "Create, auth = headers, pair configured": app_create then app_set_upstream_auth.
	if model.Auth.ValueString() == "headers" && !plan.HeadersVersion.IsNull() {
		headers, d := headersWoToArgs(ctx, req.Config)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := r.client.AppSetUpstreamAuth(ctx, plan.Slug.ValueString(), headers); err != nil {
			hubDiag(&resp.Diagnostics, "app_set_upstream_auth", err)
			return
		}
		model.HeadersAppliedVersion = plan.HeadersVersion
		resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	if plan.Archived.ValueBool() {
		if err := r.client.AppArchive(ctx, plan.Slug.ValueString()); err != nil {
			hubDiag(&resp.Diagnostics, "app_archive", err)
			return
		}
		model.Archived = plan.Archived
		resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
	}
}

func (r *proxyAppResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state proxyAppModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := r.client.AppGet(ctx, state.Slug.ValueString())
	if err != nil {
		if pmcp.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		hubDiag(&resp.Diagnostics, "app_get", err)
		return
	}
	if row.Kind != "proxy" {
		resp.Diagnostics.AddError(
			"App is not a proxy app",
			fmt.Sprintf("%q is a %q app; manage it with pmcp_tunnel_app instead.", state.Slug.ValueString(), row.Kind),
		)
		return
	}

	model, diags := proxyModelFromRow(ctx, state, row)
	resp.Diagnostics.Append(diags...)
	model.HeadersWo = types.MapNull(types.StringType)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// Update runs the field patch first (least destructive: it may itself flip `auth`, which the
// headers step depends on having already settled — §22.2's oauth -> headers row), then headers
// convergence (only when ModifyPlan flagged a mismatch), then archived — its own op, decoupled
// from app_update. State is set after each successful RPC so a later failure does not discard
// an earlier success (§22.4, "partial apply").
func (r *proxyAppResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state proxyAppModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if proxyAppChanged(plan, state) {
		args := map[string]any{"slug": plan.Slug.ValueString()}
		appendCommonArgs(ctx, args, plan.commonAppModel, &resp.Diagnostics)
		appendProxyArgs(ctx, args, plan, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}

		row, err := r.client.AppUpdate(ctx, args)
		if err != nil {
			hubDiag(&resp.Diagnostics, "app_update", err)
			state.HeadersWo = types.MapNull(types.StringType)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}

		model, diags := proxyModelFromRow(ctx, state, row)
		resp.Diagnostics.Append(diags...)
		state = model
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// Co-requirement (ValidateConfig) means headers_version's presence alone tells us whether
	// the pair is configured; headers_wo itself is always null in Plan, so it cannot be used
	// for this check.
	if plan.HeadersAppliedVersion.IsUnknown() {
		if state.Auth.ValueString() == "headers" && !plan.HeadersVersion.IsNull() {
			headers, d := headersWoToArgs(ctx, req.Config)
			resp.Diagnostics.Append(d...)
			if resp.Diagnostics.HasError() {
				state.HeadersWo = types.MapNull(types.StringType)
				resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
				return
			}
			if err := r.client.AppSetUpstreamAuth(ctx, plan.Slug.ValueString(), headers); err != nil {
				hubDiag(&resp.Diagnostics, "app_set_upstream_auth", err)
				state.HeadersWo = types.MapNull(types.StringType)
				resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
				return
			}
			state.HeadersAppliedVersion = plan.HeadersVersion
		} else {
			// headers -> oauth, or (now) headers with the pair absent: whatever the witness
			// pointed to no longer exists.
			state.HeadersAppliedVersion = types.Int64Null()
		}
	}
	state.HeadersVersion = plan.HeadersVersion
	state.HeadersWo = types.MapNull(types.StringType)

	if !plan.Archived.Equal(state.Archived) {
		var (
			op  string
			err error
		)
		if plan.Archived.ValueBool() {
			op, err = "app_archive", r.client.AppArchive(ctx, plan.Slug.ValueString())
		} else {
			op, err = "app_unarchive", r.client.AppUnarchive(ctx, plan.Slug.ValueString())
		}
		if err != nil {
			hubDiag(&resp.Diagnostics, op, err)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
		state.Archived = plan.Archived
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete is idempotent: a not-found app (already gone, or removed out of band) is success,
// matching §22.4's "Delete is idempotent: NotFound on delete succeeds."
func (r *proxyAppResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state proxyAppModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.AppDelete(ctx, state.Slug.ValueString()); err != nil && !pmcp.IsNotFound(err) {
		hubDiag(&resp.Diagnostics, "app_delete", err)
	}
}

// ImportState imports by slug (§22.4). headers_wo/headers_version/headers_applied_version are
// unavailable on import — left null by the Read the framework runs immediately afterward, since
// the hub never returns header state on any read path. §22.4 states this is intended: an import
// followed by a plan with a configured pair re-sends on the first apply.
func (r *proxyAppResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}

// authValueValidator restricts `auth` to the hub's two upstream credential modes, matching
// admin.ts's own `values: ["headers", "oauth"]` enum so a typo is a plan-time diagnostic
// instead of a round trip to app_create/app_update.
type authValueValidator struct{}

func (authValueValidator) Description(context.Context) string { return `must be "headers" or "oauth"` }

func (v authValueValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (authValueValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if v := req.ConfigValue.ValueString(); v != "headers" && v != "oauth" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid auth", fmt.Sprintf(`%q must be "headers" or "oauth".`, v))
	}
}
