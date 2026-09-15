// Package provider: pmcp_agent manages one agent — a non-human MCP consumer identity that
// pmcp_grant attaches roles to. See §22.4 of the hub's design spec.
package provider

import (
	"context"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &agentResource{}
	_ resource.ResourceWithConfigure   = &agentResource{}
	_ resource.ResourceWithImportState = &agentResource{}
)

// NewAgentResource returns the pmcp_agent resource constructor, registered by the provider.
func NewAgentResource() resource.Resource { return &agentResource{} }

// agentResource holds only the client handed in at Configure — no cache. The framework serves
// Create/Read/Update/Delete concurrently against one instance, and there is no plan/apply
// boundary a provider can observe to safely invalidate one (§22.4, "no memoization").
type agentResource struct {
	client *pmcp.Client
}

// agentResourceModel mirrors the schema below. `name` is optional+computed because the hub
// defaults it to `slug` at create; `description` defaults to "" the same way. Neither field
// forces replacement — only `slug` does, because the hub's agent_update op exists to change
// them in place (§22.4's second permitted hub change, alongside admin_token).
type agentResourceModel struct {
	Slug        types.String `tfsdk:"slug"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
}

func (r *agentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (r *agentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A non-human MCP consumer identity. Attach roles to it with pmcp_grant.",
		Attributes: map[string]schema.Attribute{
			"slug": schema.StringAttribute{
				Required:    true,
				Description: "Unique in this namespace, `[a-z0-9-]+`. Forces replacement: it is the only field agent_update cannot change.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Display name. The hub defaults it to `slug` when omitted at create.",
				PlanModifiers: []planmodifier.String{
					// Without this, an unconfigured `name` replans as unknown on every apply —
					// the hub's default is resolved once, at create, not recomputed per plan.
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Free-text note. Defaults to empty.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *agentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *agentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan agentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	args := map[string]any{"slug": plan.Slug.ValueString()}
	if name := optionalString(plan.Name); name != nil {
		args["name"] = *name
	}
	if description := optionalString(plan.Description); description != nil {
		args["description"] = *description
	}

	row, err := r.client.AgentCreate(ctx, args)
	if err != nil {
		hubDiag(&resp.Diagnostics, "agent_create", err)
		return
	}

	plan.Slug = types.StringValue(row.Slug)
	plan.Name = types.StringValue(row.Name)
	plan.Description = types.StringValue(row.Description)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read goes through agent_list (via the client's Agent lookup) — there is no agent_get, per
// §22.4's "Reads use app_get for apps. Agents and grants read through agent_list."
func (r *agentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := r.client.Agent(ctx, state.Slug.ValueString())
	if err != nil {
		if pmcp.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		hubDiag(&resp.Diagnostics, "agent_list", err)
		return
	}

	state.Name = types.StringValue(row.Name)
	state.Description = types.StringValue(row.Description)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update calls agent_update, the hub op §22.4 grows specifically so this can be a plain field
// update rather than a replace. A hub that does not have it yet reports -32601, and hubDiag turns
// that into an actionable message naming the op the hub is missing.
func (r *agentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan agentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	args := map[string]any{"slug": plan.Slug.ValueString()}
	if name := optionalString(plan.Name); name != nil {
		args["name"] = *name
	}
	if description := optionalString(plan.Description); description != nil {
		args["description"] = *description
	}

	row, err := r.client.AgentUpdate(ctx, args)
	if err != nil {
		hubDiag(&resp.Diagnostics, "agent_update", err)
		return
	}

	plan.Name = types.StringValue(row.Name)
	plan.Description = types.StringValue(row.Description)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete is idempotent: a not-found agent (already gone, or removed out of band) is success,
// matching §22.4's "Delete is idempotent: NotFound on delete succeeds." The framework calls
// State.RemoveResource automatically once Delete returns without an error diagnostic.
func (r *agentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.AgentDelete(ctx, state.Slug.ValueString()); err != nil && !pmcp.IsNotFound(err) {
		hubDiag(&resp.Diagnostics, "agent_delete", err)
	}
}

// ImportState imports by slug (§22.4: "Import IDs: apps and agents by slug").
func (r *agentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}
