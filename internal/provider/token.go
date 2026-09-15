// Package provider — pmcp_token. §22.2 accepts the state cost deliberately: token_issue
// returns its plaintext exactly once, so a managed resource has no way to represent that
// credential without persisting it, and the write-only/ephemeral alternatives were rejected
// there (write-only cannot carry a returned value; ephemeral would orphan a credential every
// run). Every lifecycle rule below traces back to that document.
package provider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = &tokenResource{}
	_ resource.ResourceWithConfigure      = &tokenResource{}
	_ resource.ResourceWithImportState    = &tokenResource{}
	_ resource.ResourceWithValidateConfig = &tokenResource{}
)

// NewTokenResource returns a fresh pmcp_token resource. The framework may hold several
// instances concurrently; nothing on this type is shared, cached, or lazily filled — see
// §22.4's "no memoization" rule.
func NewTokenResource() resource.Resource { return &tokenResource{} }

type tokenResource struct {
	client *pmcp.Client
}

// tokenResourceModel is pmcp_token's schema, one Go value per attribute. revoked_at is not in
// §22.2's attribute table — see its doc comment on the schema attribute for why it is here
// anyway.
type tokenResourceModel struct {
	Agent     types.String `tfsdk:"agent"`
	App       types.String `tfsdk:"app"`
	ExpiresIn types.String `tfsdk:"expires_in"`
	Rotation  types.Int64  `tfsdk:"rotation"`
	Token     types.String `tfsdk:"token"`
	ID        types.String `tfsdk:"id"`
	Prefix    types.String `tfsdk:"prefix"`
	CreatedAt types.Int64  `tfsdk:"created_at"`
	ExpiresAt types.Int64  `tfsdk:"expires_at"`
	RevokedAt types.Int64  `tfsdk:"revoked_at"`
}

func (r *tokenResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_token"
}

func (r *tokenResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Issues and manages one hub credential — an agent key or a " +
			"tunneled app token — through `token_issue`/`token_revoke`. The plaintext is only " +
			"ever returned by the apply that creates it; `pmcp_tokens` surfaces every " +
			"credential this resource does not manage. See §22.2 of the hub's design spec.",
		Attributes: map[string]schema.Attribute{
			"agent": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The agent slug this key authenticates. Exactly one of " +
					"`agent`/`app` is required.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"app": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The tunneled app slug this token authenticates — the hub " +
					"refuses a proxied app. Exactly one of `agent`/`app` is required.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"expires_in": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Seconds until expiry, or the literal `\"never\"`. An " +
					"unquoted number is accepted — Terraform coerces it to this attribute's " +
					"string type. Omitted takes the hub's default: 90 days for an agent key, " +
					"never for an app token.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"rotation": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Bump to reissue. The hub has no rotate op, so any change " +
					"here replaces the resource — under `create_before_destroy` the new token is " +
					"issued before the old one is revoked; under the default ordering the old " +
					"one is revoked first.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"token": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The plaintext credential, set once by the apply that " +
					"issued it. The hub can never return it again, so every later `Read` " +
					"preserves this value verbatim rather than clearing it — and `terraform " +
					"import` leaves it null, since the hub has nothing to give an import either.",
			},
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The token row's id, as `token_list`/`token_revoke` use " +
					"it. Also the import identifier.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"prefix": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The credential's display prefix, as `token_list` shows it.",
			},
			"created_at": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Epoch milliseconds when the token was issued.",
			},
			"expires_at": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Epoch milliseconds when the token expires, or null when " +
					"it never does.",
			},
			"revoked_at": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Epoch milliseconds when the token was revoked out of " +
					"band (e.g. `pmcp token revoke`), or null while live. §22.2 states plainly " +
					"that a revoked-or-expired row read back is not drift — it stays in state " +
					"with `revoked_at`/`expires_at` set rather than being removed — but its own " +
					"attribute table lists only `expires_at`. Without this attribute that " +
					"sentence has nowhere to put the revocation half of the fact, so it is added " +
					"here; flagged in the implementation report as a spec gap.",
			},
		},
	}
}

func (r *tokenResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*pmcp.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected resource configure type",
			fmt.Sprintf("Expected *pmcp.Client, got %T. Report this as a provider bug.", req.ProviderData),
		)
		return
	}
	r.client = client
}

// ValidateConfig enforces "exactly one of agent/app", which RequiresReplace on both attributes
// cannot express, and rejects an expires_in that is neither "never" nor a plain integer before
// any RPC runs.
func (r *tokenResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg tokenResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// IsNull() is false for an unknown value, which is deliberate here: an agent/app sourced
	// from another resource's (non-computed, required) slug attribute is normally known at
	// plan time anyway, and treating an unknown as "present" avoids a false "neither is set"
	// error in the rarer case it is not.
	agentSet, appSet := !cfg.Agent.IsNull(), !cfg.App.IsNull()
	if agentSet == appSet {
		resp.Diagnostics.AddError(
			"Exactly one of `agent` or `app` is required",
			"pmcp_token issues a credential for exactly one referent. Set `agent` to bind an "+
				"agent key, or `app` to bind a tunneled app's token — never both, never neither.",
		)
	}

	if _, err := expiresInArg(cfg.ExpiresIn); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("expires_in"), "Invalid expires_in", err.Error())
	}
}

func (r *tokenResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan tokenResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	kind, slug := "agent", plan.Agent.ValueString()
	if !plan.App.IsNull() {
		kind, slug = "app", plan.App.ValueString()
	}

	args := map[string]any{"kind": kind, "slug": slug}
	expiresIn, err := expiresInArg(plan.ExpiresIn)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("expires_in"), "Invalid expires_in", err.Error())
		return
	}
	if expiresIn != nil {
		args["expires_in"] = expiresIn
	}

	// Create issues and revokes nothing (§22.2's create_before_destroy guarantee): one RPC,
	// nothing to order.
	issued, err := r.client.TokenIssue(ctx, args)
	if err != nil {
		hubDiag(&resp.Diagnostics, "token_issue", err)
		return
	}

	// The plaintext and id exist now regardless of what happens next — persist them before the
	// follow-up read, per §22.4's partial-apply rule, so a failure below does not orphan an
	// already-issued credential invisibly.
	plan.ID = types.StringValue(issued.ID)
	plan.Token = types.StringValue(issued.Token)

	row, err := r.client.Token(ctx, issued.ID)
	if err != nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		hubDiag(&resp.Diagnostics, "token_list", err)
		return
	}
	applyTokenRow(&plan, row)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes metadata by id and preserves the prior `token` value verbatim — it is never
// reassigned here, on either the normal-refresh or post-import path, which is what makes both
// cases correct with the same code: a normal refresh already has the plaintext in state, and an
// import starts with it null. A revoked or expired row is not drift (§22.2): only ErrNotFound —
// the row absent from token_list entirely — removes the resource from state.
func (r *tokenResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state tokenResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := r.client.Token(ctx, state.ID.ValueString())
	if err != nil {
		if pmcp.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		hubDiag(&resp.Diagnostics, "token_list", err)
		return
	}

	applyTokenRow(&state, row)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update exists only to satisfy resource.Resource: every configurable attribute carries
// RequiresReplace, so a changed value is never planned as an in-place update — Terraform plans
// a replace instead, which runs Create and Delete, not this. Reaching this method means nothing
// user-visible differs, so it carries the plan forward with no RPC — matching Create/Delete's
// "issues and revokes nothing" / "revokes exactly its own id" contract, which a stray reissue or
// revoke here would break.
func (r *tokenResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan tokenResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete is token_revoke, idempotent: a row already revoked but still present succeeds again
// (the hub re-sets revoked_at), and a row already gone — the referenced agent or app was
// deleted, cascading its tokens — answers absent, which IsNotFound recognizes as success too.
func (r *tokenResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state tokenResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.TokenRevoke(ctx, state.ID.ValueString()); err != nil && !pmcp.IsNotFound(err) {
		hubDiag(&resp.Diagnostics, "token_revoke", err)
	}
}

// ImportState takes the token's id. Everything else — including agent/app, derived from the
// row's kind/refSlug — is filled by the Read that the framework runs immediately afterward;
// token starts null there for the same reason Read never touches it: the hub has nothing to
// hand back.
func (r *tokenResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyTokenRow copies a TokenRow's metadata into the model, deriving agent/app from
// kind/refSlug so a freshly imported resource (which starts with both null) ends up correct
// without a separate import code path. It never touches Token, Rotation, or ExpiresIn — the
// first because the hub cannot return it again, the other two because they have no wire
// counterpart to refresh from.
func applyTokenRow(m *tokenResourceModel, row pmcp.TokenRow) {
	switch row.Kind {
	case "agent":
		m.Agent, m.App = types.StringValue(row.RefSlug), types.StringNull()
	case "app":
		m.App, m.Agent = types.StringValue(row.RefSlug), types.StringNull()
	}
	m.Prefix = types.StringValue(row.Prefix)
	m.CreatedAt = types.Int64Value(row.CreatedAt)
	m.ExpiresAt = int64PtrValue(row.ExpiresAt)
	m.RevokedAt = int64PtrValue(row.RevokedAt)
}

// int64PtrValue converts token_list's nullable epoch-ms fields to a computed Int64 attribute.
func int64PtrValue(v *int64) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*v)
}

// expiresInArg parses `expires_in` into what token_issue's wire field accepts: the literal
// string "never", or a JSON integer number of seconds. A null/unknown attribute — omitted —
// returns (nil, nil), the signal callers use to leave the key out of the op arguments entirely
// (the hub's own default then applies).
func expiresInArg(v types.String) (any, error) {
	if v.IsNull() || v.IsUnknown() {
		return nil, nil
	}
	raw := v.ValueString()
	if raw == "never" {
		return "never", nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds < 0 {
		return nil, fmt.Errorf("must be a non-negative number of seconds, or the string \"never\"; got %q", raw)
	}
	return seconds, nil
}
