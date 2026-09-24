// Package provider: pmcp_tunnel_app manages a tunneled app — a bot or script that dials into
// the hub and registers its own tools. See §22.4 of the hub's design spec.
package provider

import (
	"context"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &tunnelAppResource{}
	_ resource.ResourceWithConfigure   = &tunnelAppResource{}
	_ resource.ResourceWithImportState = &tunnelAppResource{}
)

// NewTunnelAppResource returns the pmcp_tunnel_app resource constructor, registered by the provider.
func NewTunnelAppResource() resource.Resource { return &tunnelAppResource{} }

// tunnelAppResource holds only the client handed in at Configure — no cache, matching every
// other resource in this package (§22.4, "no memoization").
type tunnelAppResource struct {
	client *pmcp.Client
}

// tunnelAppModel is commonAppModel plus the one attribute §22.4 gives a tunneled app alone.
type tunnelAppModel struct {
	commonAppModel
	// OwnerRoles is `owner_roles`, in rolesAttribute's map-of-object shape. Null in state means
	// the row carried no `ownerRoles` key; `{}` means the owner has defined none.
	OwnerRoles types.Map `tfsdk:"owner_roles"`
}

func tunnelModelFromRow(ctx context.Context, row pmcp.AppRow) (tunnelAppModel, diag.Diagnostics) {
	common, diags := commonFromRow(ctx, row)
	ownerRoles, err := ownerRolesFrom(ctx, row.OwnerRoles)
	if err != nil {
		diags.AddError("Could not convert owner_roles", err.Error())
	}
	return tunnelAppModel{commonAppModel: common, OwnerRoles: ownerRoles}, diags
}

// ownerRolesAttribute is `owner_roles` (§22.4, decision 32): `pmcp_proxy_app.roles`' typed shape
// and plan modifiers, on the one app kind whose hub row stores an owner map. It is absent from
// pmcp_proxy_app's schema rather than refused there: the hub refuses `owner_roles` on a proxied
// app, whose `roles` already belong to the owner.
func ownerRolesAttribute() schema.MapNestedAttribute {
	attribute := rolesAttribute()
	attribute.MarkdownDescription = "The owner's own roles on this app, keyed by role name, in " +
		"the typed per-family form `pmcp_proxy_app.roles` takes. They sit beside the roles the " +
		"app declares when it connects, which are never an attribute here: for a name both " +
		"define, the app's definition replaces this one whole. The value is the complete set " +
		"and apply replaces the hub's with it, so `{}` means no owner roles. Leaving the " +
		"attribute out stops managing it: nothing is sent, and the hub keeps what it has, " +
		"including roles set from the web UI. Removing a role that a `pmcp_grant` names " +
		"leaves the grant in place, granting nothing through that name until the role is " +
		"defined again or the app declares it."
	return attribute
}

// ownerRolesFrom converts a row's `ownerRoles` to the attribute. It differs from rolesFrom only
// for a nil map: the row carried no key, which on a proxied app means no owner map exists at
// all, so it becomes null rather than an empty map claiming the app has one.
func ownerRolesFrom(ctx context.Context, in map[string]pmcp.RoleFamilies) (types.Map, error) {
	if in == nil {
		return types.MapNull(types.ObjectType{AttrTypes: roleFamiliesAttrTypes}), nil
	}
	return rolesFrom(ctx, in)
}

// appendOwnerRoles adds `owner_roles` to an app_create/app_update argument map when value is
// known, and leaves the map alone when it is null or unknown. Whatever is sent replaces the
// hub's stored map whole, so the caller decides when to call it: Create always, since an
// unconfigured attribute is unknown there and drops out on its own; Update only when the plan
// differs from state.
func appendOwnerRoles(ctx context.Context, args map[string]any, value types.Map, diags *diag.Diagnostics) {
	roles, d := rolesToArgs(ctx, value)
	diags.Append(d...)
	if roles != nil {
		args["owner_roles"] = roles
	}
}

func (r *tunnelAppResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tunnel_app"
}

func (r *tunnelAppResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attributes := commonAppAttributes()
	attributes["owner_roles"] = ownerRolesAttribute()
	resp.Schema = schema.Schema{
		MarkdownDescription: "A tunneled app: a bot or script that dials into the hub over its " +
			"own connection and registers its own tools, prompts, and resources at connect " +
			"time. See §22.4 of the hub's design spec.",
		Attributes: attributes,
	}
}

func (r *tunnelAppResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *tunnelAppResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan tunnelAppModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	args := map[string]any{"slug": plan.Slug.ValueString(), "kind": "tunnel"}
	appendCommonArgs(ctx, args, plan.commonAppModel, &resp.Diagnostics)
	appendOwnerRoles(ctx, args, plan.OwnerRoles, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := r.client.AppCreate(ctx, args)
	if err != nil {
		hubDiag(&resp.Diagnostics, "app_create", err)
		return
	}

	model, diags := tunnelModelFromRow(ctx, row)
	resp.Diagnostics.Append(diags...)
	// Reflect the created (always unarchived) app first — partial-apply safety (§22.4): if the
	// archive step below fails, the state already returned here is not lost.
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
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

func (r *tunnelAppResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state tunnelAppModel
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
	if row.Kind != "tunnel" {
		resp.Diagnostics.AddError(
			"App is not a tunnel app",
			fmt.Sprintf("%q is a %q app; manage it with pmcp_proxy_app instead.", state.Slug.ValueString(), row.Kind),
		)
		return
	}

	model, diags := tunnelModelFromRow(ctx, row)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// Update calls app_update only when a field it owns actually changed (commonAppChanged, or
// `owner_roles`), then handles `archived` as its own, unconditional step — §22.4's "archived ...
// fires app_archive/app_unarchive, not app_update". State is set after each successful RPC so a
// later failure does not discard an earlier success (§22.4, "partial apply").
//
// Unlike the common fields, `owner_roles` rides app_update only when it changed. With the
// attribute unconfigured the plan carries the map read at refresh, and resending it would
// overwrite, whole, any edit the web UI's Roles pane made between that refresh and this apply.
func (r *tunnelAppResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state tunnelAppModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ownerRolesChanged := !plan.OwnerRoles.Equal(state.OwnerRoles)
	if commonAppChanged(plan.commonAppModel, state.commonAppModel) || ownerRolesChanged {
		args := map[string]any{"slug": plan.Slug.ValueString()}
		appendCommonArgs(ctx, args, plan.commonAppModel, &resp.Diagnostics)
		if ownerRolesChanged {
			appendOwnerRoles(ctx, args, plan.OwnerRoles, &resp.Diagnostics)
		}
		if resp.Diagnostics.HasError() {
			return
		}

		row, err := r.client.AppUpdate(ctx, args)
		if err != nil {
			hubDiag(&resp.Diagnostics, "app_update", err)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}

		model, diags := tunnelModelFromRow(ctx, row)
		resp.Diagnostics.Append(diags...)
		state = model
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

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
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	}
}

// Delete is idempotent: a not-found app (already gone, or removed out of band) is success,
// matching §22.4's "Delete is idempotent: NotFound on delete succeeds."
func (r *tunnelAppResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state tunnelAppModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.AppDelete(ctx, state.Slug.ValueString()); err != nil && !pmcp.IsNotFound(err) {
		hubDiag(&resp.Diagnostics, "app_delete", err)
	}
}

// ImportState imports by slug (§22.4: "Import IDs: apps and agents by slug").
func (r *tunnelAppResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}
