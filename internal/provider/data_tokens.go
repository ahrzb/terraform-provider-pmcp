// Package provider — the pmcp_tokens data source. §22.4's one plural data source: its purpose
// is to surface credentials pmcp_token did not create, so it wraps token_list unfiltered by the
// wire (the op itself takes no arguments) and applies agent/app as a provider-side filter.
// last_used_at is deliberately omitted — it changes on every use and would make this data
// source dirty on refreshes unrelated to configuration; that operator question belongs to
// `pmcp token list`, not a plan.
package provider

import (
	"context"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource                   = &tokensDataSource{}
	_ datasource.DataSourceWithConfigure      = &tokensDataSource{}
	_ datasource.DataSourceWithValidateConfig = &tokensDataSource{}
)

// NewTokensDataSource returns a fresh pmcp_tokens data source.
func NewTokensDataSource() datasource.DataSource { return &tokensDataSource{} }

type tokensDataSource struct {
	client *pmcp.Client
}

type tokensDataSourceModel struct {
	Agent  types.String         `tfsdk:"agent"`
	App    types.String         `tfsdk:"app"`
	Tokens []tokenMetadataModel `tfsdk:"tokens"`
}

// tokenMetadataModel is one token_list row, metadata only — never a value, because the hub has
// none to give for a credential this resource did not issue. last_used_at is not here; see the
// package comment.
type tokenMetadataModel struct {
	ID        types.String `tfsdk:"id"`
	Kind      types.String `tfsdk:"kind"`
	RefSlug   types.String `tfsdk:"ref_slug"`
	Prefix    types.String `tfsdk:"prefix"`
	CreatedAt types.Int64  `tfsdk:"created_at"`
	ExpiresAt types.Int64  `tfsdk:"expires_at"`
	RevokedAt types.Int64  `tfsdk:"revoked_at"`
}

func (d *tokensDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tokens"
}

func (d *tokensDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists this namespace's credentials via `token_list` — including " +
			"ones no `pmcp_token` resource manages. Use it to assert on inventory: that no " +
			"unmanaged key exists for a sensitive agent, or that nothing expired is still live.",
		Attributes: map[string]schema.Attribute{
			"agent": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Limit to this agent's tokens. `token_list` itself takes " +
					"no filter; this narrows the result client-side. At most one of `agent`/`app`.",
			},
			"app": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Limit to this app's tokens. At most one of `agent`/`app`.",
			},
			"tokens": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The matching credentials, metadata only.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The token row's id, as `token_revoke` takes it.",
						},
						"kind": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "`agent` or `app`.",
						},
						"ref_slug": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The agent or app slug this credential is bound to.",
						},
						"prefix": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The credential's display prefix. Never the value.",
						},
						"created_at": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Epoch milliseconds when the token was issued.",
						},
						"expires_at": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Epoch milliseconds when the token expires, or null when it never does.",
						},
						"revoked_at": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Epoch milliseconds when the token was revoked, or null while live.",
						},
					},
				},
			},
		},
	}
}

func (d *tokensDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*pmcp.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected data source configure type",
			fmt.Sprintf("Expected *pmcp.Client, got %T. Report this as a provider bug.", req.ProviderData),
		)
		return
	}
	d.client = client
}

// ValidateConfig rejects agent+app together rather than silently defining what their
// intersection would mean: a token's kind is exactly one of the two, so combining the filters
// could only ever mean "match neither" or an ambiguous "match either", and the spec states this
// as a single choice ("filtered by agent or app").
func (d *tokensDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var cfg tokensDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !cfg.Agent.IsNull() && !cfg.App.IsNull() {
		resp.Diagnostics.AddError(
			"At most one of `agent` or `app`",
			"pmcp_tokens filters by one referent at a time. Set agent, or app, or neither to list everything.",
		)
	}
}

func (d *tokensDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg tokensDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	rows, err := d.client.TokenList(ctx, nil)
	if err != nil {
		hubDiag(&resp.Diagnostics, "token_list", err)
		return
	}

	agent, app := optionalString(cfg.Agent), optionalString(cfg.App)
	tokens := make([]tokenMetadataModel, 0, len(rows))
	for _, row := range rows {
		if agent != nil && !(row.Kind == "agent" && row.RefSlug == *agent) {
			continue
		}
		if app != nil && !(row.Kind == "app" && row.RefSlug == *app) {
			continue
		}
		tokens = append(tokens, tokenMetadataModel{
			ID:        types.StringValue(row.ID),
			Kind:      types.StringValue(row.Kind),
			RefSlug:   types.StringValue(row.RefSlug),
			Prefix:    types.StringValue(row.Prefix),
			CreatedAt: types.Int64Value(row.CreatedAt),
			ExpiresAt: int64PtrValue(row.ExpiresAt),
			RevokedAt: int64PtrValue(row.RevokedAt),
		})
	}

	cfg.Tokens = tokens
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
