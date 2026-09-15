// Package provider — the pmcp_agent data source. §22.4: returns slug, name, description,
// created_at, and grants normalized the same way pmcp_grant normalizes — splitting the wire's
// combined "role" / "role:approval" syntax into two sets per app.
package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &agentDataSource{}
	_ datasource.DataSourceWithConfigure = &agentDataSource{}
)

// NewAgentDataSource returns a fresh pmcp_agent data source.
func NewAgentDataSource() datasource.DataSource { return &agentDataSource{} }

type agentDataSource struct {
	client *pmcp.Client
}

type agentDataSourceModel struct {
	Slug        types.String `tfsdk:"slug"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	CreatedAt   types.Int64  `tfsdk:"created_at"`
	Grants      types.Map    `tfsdk:"grants"`
}

// agentGrantAttrTypes is one app's grant, split into pmcp_grant's two sets.
var agentGrantAttrTypes = map[string]attr.Type{
	"allow":    types.SetType{ElemType: types.StringType},
	"approval": types.SetType{ElemType: types.StringType},
}

type agentGrantValue struct {
	Allow    []string `tfsdk:"allow"`
	Approval []string `tfsdk:"approval"`
}

// splitGrantEntry classifies one wire grant string. Confirmed against pmcp_grant's own
// rolesFromWire (grant.go): a plain suffix check, no "all" special-case on read — that exemption
// is a write-side undeclared-role rule, not part of how an existing grant is parsed back.
func splitGrantEntry(entry string) (role string, approval bool) {
	if role, ok := strings.CutSuffix(entry, ":approval"); ok {
		return role, true
	}
	return entry, false
}

// grantsFrom converts agent_list's wire grants to the schema's map(object) shape. A nil/empty
// wire map becomes an empty (not null) framework map, matching redactFrom's reasoning:
// agent_list always includes `grants`, even as `{}`, so "no grants" is an observed fact.
func grantsFrom(ctx context.Context, in map[string][]string) (types.Map, error) {
	if in == nil {
		in = map[string][]string{}
	}
	values := make(map[string]agentGrantValue, len(in))
	for appSlug, entries := range in {
		allow, approval := []string{}, []string{}
		for _, entry := range entries {
			role, isApproval := splitGrantEntry(entry)
			if isApproval {
				approval = append(approval, role)
			} else {
				allow = append(allow, role)
			}
		}
		values[appSlug] = agentGrantValue{Allow: allow, Approval: approval}
	}
	out, diags := types.MapValueFrom(ctx, types.ObjectType{AttrTypes: agentGrantAttrTypes}, values)
	if diags.HasError() {
		return types.MapNull(types.ObjectType{AttrTypes: agentGrantAttrTypes}), fmt.Errorf("%v", diags)
	}
	return out, nil
}

func (d *agentDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (d *agentDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads one agent and its grants via `agent_list`, which is the " +
			"only read path — there is no `agent_get`.",
		Attributes: map[string]schema.Attribute{
			"slug": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The agent's slug, unique in this namespace.",
			},
			"name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Display name.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Free-text note.",
			},
			"created_at": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Epoch milliseconds when the agent was created.",
			},
			"grants": schema.MapAttribute{
				Computed:    true,
				ElementType: types.ObjectType{AttrTypes: agentGrantAttrTypes},
				MarkdownDescription: "This agent's grants, keyed by app slug, each split into " +
					"`allow` and `approval` role sets the way `pmcp_grant` itself does.",
			},
		},
	}
}

func (d *agentDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *agentDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg agentDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := d.client.Agent(ctx, cfg.Slug.ValueString())
	if err != nil {
		if pmcp.IsNotFound(err) {
			resp.Diagnostics.AddAttributeError(
				path.Root("slug"),
				"No such agent",
				fmt.Sprintf("No agent %q exists in this namespace.", cfg.Slug.ValueString()),
			)
			return
		}
		hubDiag(&resp.Diagnostics, "agent_list", err)
		return
	}

	grants, err := grantsFrom(ctx, row.Grants)
	if err != nil {
		resp.Diagnostics.AddError("Could not convert grants", err.Error())
		return
	}

	model := agentDataSourceModel{
		Slug:        types.StringValue(row.Slug),
		Name:        types.StringValue(row.Name),
		Description: types.StringValue(row.Description),
		CreatedAt:   types.Int64Value(row.CreatedAt),
		Grants:      grants,
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
