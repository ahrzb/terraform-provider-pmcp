// Package provider: pmcp_grant manages one (agent, app) pair's full role set. See §22.4 of the
// hub's design spec.
package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = &grantResource{}
	_ resource.ResourceWithConfigure      = &grantResource{}
	_ resource.ResourceWithImportState    = &grantResource{}
	_ resource.ResourceWithValidateConfig = &grantResource{}
)

// NewGrantResource returns the pmcp_grant resource constructor, registered by the provider.
func NewGrantResource() resource.Resource { return &grantResource{} }

// grantResource holds only the client handed in at Configure — no cache, for the same reason as
// agentResource (§22.4, "no memoization").
type grantResource struct {
	client *pmcp.Client
}

// grantResourceModel mirrors the schema below. `allow` and `approval` are separate sets rather
// than the wire's single `role`/`role:approval` list: the suffix is an encoding detail the
// provider owns, and it lets "the same role in both modes" be a plan-time diagnostic instead of
// a server round trip (§22.4).
type grantResourceModel struct {
	Agent    types.String `tfsdk:"agent"`
	App      types.String `tfsdk:"app"`
	Allow    types.Set    `tfsdk:"allow"`
	Approval types.Set    `tfsdk:"approval"`
}

func (r *grantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_grant"
}

func (r *grantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "The full role grant for one (agent, app) pair. Create/Update replace the whole set; there is no incremental grant/revoke.",
		Attributes: map[string]schema.Attribute{
			"agent": schema.StringAttribute{
				Required:      true,
				Description:   "The agent's slug. Reference `pmcp_agent.<name>.slug` so the graph orders the agent before its grants.",
				PlanModifiers: replace,
			},
			"app": schema.StringAttribute{
				Required:      true,
				Description:   "The app's slug. Reference `pmcp_*_app.<name>.slug` so the graph orders the app before its grants.",
				PlanModifiers: replace,
			},
			"allow": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Role names granted without approval gating. At least one of `allow`/`approval` must be non-empty; the two must not share a role.",
			},
			"approval": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Role names granted with approval gating (`role:approval` on the wire). At least one of `allow`/`approval` must be non-empty; the two must not share a role.",
			},
		},
	}
}

// ValidateConfig enforces §22.4's two config-shape rules at plan time, before any RPC: at least
// one of `allow`/`approval` must be non-empty, and the two must be disjoint. Both are checkable
// from the grant's own configuration, unlike the undeclared-role rule below, which needs the
// referenced app and therefore waits for apply.
func (r *grantResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg grantResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown set depends on a value not yet known (e.g. another resource's attribute);
	// nothing to validate until apply resolves it.
	if cfg.Allow.IsUnknown() || cfg.Approval.IsUnknown() {
		return
	}

	allow, diags := stringSetTo(ctx, cfg.Allow)
	resp.Diagnostics.Append(diags...)
	approval, diags2 := stringSetTo(ctx, cfg.Approval)
	resp.Diagnostics.Append(diags2...)
	if resp.Diagnostics.HasError() {
		return
	}

	if len(allow) == 0 && len(approval) == 0 {
		resp.Diagnostics.AddError(
			"Empty grant",
			"`allow` and `approval` are both empty (or unset). A pmcp_grant must declare at least "+
				"one role; to revoke every role for this pair, destroy the resource instead.",
		)
	}

	// An explicitly empty set is refused even when its sibling carries roles, and the reason is
	// a perpetual diff rather than taste. Both attributes are Optional and not Computed, so
	// OpenTofu requires the applied state to echo the configuration exactly — and `[]` and null
	// are different values. Read cannot tell which the operator wrote: it reconstructs both sets
	// from the wire's flat role list, where "no allow roles" and "allow omitted" are the same
	// absence, and stringSetFrom renders that absence as null. So `allow = []` beside a
	// non-empty `approval` applies cleanly and then plans `null -> []` on every subsequent run,
	// forever. Reproduced with a real `tofu plan` before this guard existed.
	//
	// Refusing it is the total fix: omitting the attribute says the same thing and round-trips.
	for _, set := range []struct {
		name  string
		value types.Set
		roles []string
	}{
		{"allow", cfg.Allow, allow},
		{"approval", cfg.Approval, approval},
	} {
		if !set.value.IsNull() && len(set.roles) == 0 {
			resp.Diagnostics.AddAttributeError(
				path.Root(set.name),
				"Empty role set",
				fmt.Sprintf(
					"`%s` is set to an empty list. Omit the attribute instead: the hub sends one "+
						"flat role list, so a read cannot distinguish an empty set from an absent "+
						"one, and an explicit `[]` would re-plan as a change on every run.",
					set.name,
				),
			)
		}
	}

	inAllow := make(map[string]bool, len(allow))
	for _, role := range allow {
		inAllow[role] = true
	}
	for _, role := range approval {
		if inAllow[role] {
			resp.Diagnostics.AddAttributeError(
				path.Root("approval"),
				"Role granted in both modes",
				fmt.Sprintf("role %q appears in both `allow` and `approval`; a role can only be granted in one mode at a time.", role),
			)
		}
	}
}

func (r *grantResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *grantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan grantResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applyGrant(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *grantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan grantResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applyGrant(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// applyGrant is Create and Update's shared body: both are grant_set (§22.4, "Create and Update
// are both grant_set"). It validates the referenced app's declared roles, writes the grant, and
// leaves `model` unchanged on the wire's side — grant_set echoes back exactly the roles sent, so
// there is nothing to read back into the model on success.
func (r *grantResource) applyGrant(ctx context.Context, model *grantResourceModel, diags *diag.Diagnostics) {
	allow, d := stringSetTo(ctx, model.Allow)
	diags.Append(d...)
	approval, d2 := stringSetTo(ctx, model.Approval)
	diags.Append(d2...)
	if diags.HasError() {
		return
	}

	appSlug := model.App.ValueString()
	if !r.checkDeclaredRoles(ctx, appSlug, allow, approval, diags) {
		return
	}

	result, err := r.client.GrantSet(ctx, model.Agent.ValueString(), appSlug, rolesToWire(allow, approval))
	if err != nil {
		hubDiag(diags, "grant_set", err)
		return
	}
	// The hub's own undeclared-role notices for tunneled apps (registry.setGrants), on top of
	// the provider's own pre-check above — the two can both fire if a role is undeclared and the
	// pre-check already warned; that duplication is accepted rather than suppressing the hub's.
	for _, warning := range result.Warnings {
		diags.AddWarning("Undeclared role", warning)
	}
}

// checkDeclaredRoles is §22.4's undeclared-role rule, run at apply time after an app_get: a
// provider cannot read a sibling resource's configuration during plan, and on first create the
// app may not exist yet. `all` is exempt — the built-in role, never declarable. A name is
// declared when it is in the app's `roles` or its `ownerRoles`: the hub's own check reads that
// union (§20.3's effective map), so a grant on an owner role draws no warning there either.
// Anything else undeclared warns on a tunneled app (roles arrive at connect time, so config may
// legitimately lead the first connection) and errors on a proxy app (its roles live in the same
// config, so an undeclared one is an owner mistake, not a race). Returns false if it added any
// error, so the caller skips the grant_set write rather than sending a request already known to
// be wrong.
func (r *grantResource) checkDeclaredRoles(ctx context.Context, appSlug string, allow, approval []string, diags *diag.Diagnostics) bool {
	app, err := r.client.AppGet(ctx, appSlug)
	if err != nil {
		hubDiag(diags, "app_get", err)
		return false
	}

	ok := true
	check := func(attr string, roles []string) {
		for _, role := range roles {
			if role == "all" {
				continue
			}
			if _, declared := app.Roles[role]; declared {
				continue
			}
			if _, owned := app.OwnerRoles[role]; owned {
				continue
			}
			message := fmt.Sprintf("app %q does not declare role %q.", appSlug, role)
			if app.Kind == "tunnel" {
				diags.AddAttributeWarning(path.Root(attr), "Undeclared role",
					message+" Roles for tunneled apps arrive at connect time, so config may legitimately lead the first connection.")
				continue
			}
			diags.AddAttributeError(path.Root(attr), "Undeclared role",
				message+" A proxy app's roles live in the same config, so this is an error rather than a race.")
			ok = false
		}
	}
	check("allow", allow)
	check("approval", approval)
	return ok
}

// Read applies §22.4's three-way "gone" rule: the agent is absent, the agent is present with no
// key for this app, or the key is present with an empty role list. All three mean RemoveResource.
func (r *grantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state grantResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	agentSlug := state.Agent.ValueString()
	appSlug := state.App.ValueString()

	row, err := r.client.Agent(ctx, agentSlug)
	if err != nil {
		if pmcp.IsNotFound(err) {
			// Gone, case 1: the agent itself is absent.
			resp.State.RemoveResource(ctx)
			return
		}
		hubDiag(&resp.Diagnostics, "agent_list", err)
		return
	}

	roles, hasKey := row.Grants[appSlug]
	if !hasKey || len(roles) == 0 {
		// Gone, case 2 (no key for this app) and case 3 (key present, empty role list).
		resp.State.RemoveResource(ctx)
		return
	}

	allow, approval := rolesFromWire(roles)
	allowSet, d := stringSetFrom(ctx, allow)
	resp.Diagnostics.Append(d...)
	approvalSet, d2 := stringSetFrom(ctx, approval)
	resp.Diagnostics.Append(d2...)
	if resp.Diagnostics.HasError() {
		return
	}

	state.Allow = allowSet
	state.Approval = approvalSet
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete is grant_set with an empty roles list (§22.4). Idempotent: a not-found agent or app —
// already gone, or removed out of band — is success, matching every other resource's Delete.
func (r *grantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state grantResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	_, err := r.client.GrantSet(ctx, state.Agent.ValueString(), state.App.ValueString(), []string{})
	if err != nil && !pmcp.IsNotFound(err) {
		hubDiag(&resp.Diagnostics, "grant_set", err)
	}
}

// ImportState imports by `<agent>/<app>` (§22.4), unambiguous under the slug grammar since
// neither slug contains a slash.
func (r *grantResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	agentSlug, appSlug, ok := strings.Cut(req.ID, "/")
	if !ok || agentSlug == "" || appSlug == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("expected `<agent>/<app>`, got %q.", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("agent"), agentSlug)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("app"), appSlug)...)
}

// rolesToWire joins allow+approval into grant_set's flat role list, encoding approval mode with
// the wire's own `:approval` suffix (§9's syntax; admin.ts's grantEntries reads it back).
func rolesToWire(allow, approval []string) []string {
	roles := make([]string, 0, len(allow)+len(approval))
	roles = append(roles, allow...)
	for _, role := range approval {
		roles = append(roles, role+":approval")
	}
	return roles
}

// rolesFromWire splits grant_set/agent_list's flat role list back into allow/approval sets —
// "the provider's job, not the client's" per rows.go's AgentRow comment. Role names never
// contain a colon (admin.ts), so the suffix is unambiguous.
func rolesFromWire(roles []string) (allow, approval []string) {
	for _, role := range roles {
		if name, ok := strings.CutSuffix(role, ":approval"); ok {
			approval = append(approval, name)
			continue
		}
		allow = append(allow, role)
	}
	return allow, approval
}
