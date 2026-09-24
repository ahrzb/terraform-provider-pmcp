// Package provider — the pmcp_app data source. §22.4: "returns every app_get field ... and
// carries no header attributes; the hub never returns them." It is a thin read over AppGet;
// there is no write path, so there is nothing here to reconcile against a prior state.
package provider

import (
	"context"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &appDataSource{}
	_ datasource.DataSourceWithConfigure = &appDataSource{}
)

// NewAppDataSource returns a fresh pmcp_app data source. No memoization, matching the
// resources: the framework serves RPCs concurrently against one provider instance.
func NewAppDataSource() datasource.DataSource { return &appDataSource{} }

type appDataSource struct {
	client *pmcp.Client
}

// appDataSourceModel is every field app_get returns. Endpoint/auth/forward_identity/roles/
// capabilities are proxy-only on the wire and simply read as the zero value for a tunneled or
// builtin app, the same way AppRow itself represents them.
type appDataSourceModel struct {
	Slug              types.String `tfsdk:"slug"`
	Kind              types.String `tfsdk:"kind"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	Archived          types.Bool   `tfsdk:"archived"`
	LogBodies         types.Bool   `tfsdk:"log_bodies"`
	Redact            types.Map    `tfsdk:"redact"`
	RedactResults     types.Map    `tfsdk:"redact_results"`
	Endpoint          types.String `tfsdk:"endpoint"`
	Auth              types.String `tfsdk:"auth"`
	ForwardIdentity   types.Bool   `tfsdk:"forward_identity"`
	Roles             types.Map    `tfsdk:"roles"`
	Capabilities      types.Set    `tfsdk:"capabilities"`
	TypescriptAliases types.Object `tfsdk:"typescript_aliases"`
	// OwnerRoles is null, not empty, for a row with no `ownerRoles` key: the hub omits it on
	// proxied apps, which have no owner map at all.
	OwnerRoles types.Map `tfsdk:"owner_roles"`
}

// roleFamiliesAttrTypes is the object shape of one role's per-family patterns — the framework
// mirror of pmcp.RoleFamilies. §22.4 types pmcp_proxy_app's `roles` the same way for the same
// reason: the wire's oneOf (bare pattern list or this object) cannot be expressed statically,
// and the object form is the one the client always sends and the hub always returns.
var roleFamiliesAttrTypes = map[string]attr.Type{
	"tools":     types.ListType{ElemType: types.StringType},
	"prompts":   types.ListType{ElemType: types.StringType},
	"resources": types.ListType{ElemType: types.StringType},
}

// roleFamiliesValue is roleFamiliesAttrTypes' Go-native counterpart, driving the framework's
// reflection-based conversion in both directions.
//
// The three fields are `types.List` and not `[]string` for a reason a unit test did not catch
// and a real `tofu apply` did: the schema declares each family Optional+Computed, so a role that
// configures only `tools` presents `prompts` and `resources` as **unknown** during apply, and a
// `[]string` target cannot represent unknown — the framework fails the whole conversion with
// "Received unknown value, however the target type cannot handle unknown values". Optional+
// Computed is the right schema (it is what lets the hub omit an empty family without an
// inconsistent-result error), so the conversion is what has to cope.
type roleFamiliesValue struct {
	Tools     types.List `tfsdk:"tools"`
	Prompts   types.List `tfsdk:"prompts"`
	Resources types.List `tfsdk:"resources"`
}

// patternList renders one family's wire patterns. A family the hub omitted becomes a null list
// rather than an empty one: "this role declares no prompts" is what the hub said, and an empty
// list would claim it declared an empty set.
func patternList(ctx context.Context, in []string) (types.List, diag.Diagnostics) {
	if in == nil {
		return types.ListNull(types.StringType), nil
	}
	return types.ListValueFrom(ctx, types.StringType, in)
}

// patternsOf is patternList's inverse: a null or unknown list yields nil, which rolesToArgs
// omits from the wire object so an unset family is never sent as an empty array.
func patternsOf(ctx context.Context, in types.List) ([]string, diag.Diagnostics) {
	if in.IsNull() || in.IsUnknown() {
		return nil, nil
	}
	out := make([]string, 0, len(in.Elements()))
	diags := in.ElementsAs(ctx, &out, false)
	return out, diags
}

// rolesFrom converts app_get's roles map to the schema's map(object) shape. A nil/empty wire
// map becomes an empty (not null) framework map: the hub always includes `roles` in app_get's
// result, so absence of any role is an observed fact, not an unknown one — the same reasoning
// redactFrom applies to the redaction maps.
func rolesFrom(ctx context.Context, in map[string]pmcp.RoleFamilies) (types.Map, error) {
	nullMap := types.MapNull(types.ObjectType{AttrTypes: roleFamiliesAttrTypes})
	values := make(map[string]roleFamiliesValue, len(in))
	for name, families := range in {
		var v roleFamiliesValue
		var diags diag.Diagnostics
		if v.Tools, diags = patternList(ctx, families.Tools); diags.HasError() {
			return nullMap, fmt.Errorf("%v", diags)
		}
		if v.Prompts, diags = patternList(ctx, families.Prompts); diags.HasError() {
			return nullMap, fmt.Errorf("%v", diags)
		}
		if v.Resources, diags = patternList(ctx, families.Resources); diags.HasError() {
			return nullMap, fmt.Errorf("%v", diags)
		}
		values[name] = v
	}
	out, diags := types.MapValueFrom(ctx, types.ObjectType{AttrTypes: roleFamiliesAttrTypes}, values)
	if diags.HasError() {
		return nullMap, fmt.Errorf("%v", diags)
	}
	return out, nil
}

func (d *appDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_app"
}

func (d *appDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads one app's current configuration via `app_get`. Carries no " +
			"header attributes — the hub never returns upstream credentials on any read path. " +
			"Looking up the builtin `pmcp` slug is an error, not an empty result.",
		Attributes: map[string]schema.Attribute{
			"slug": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The app's slug, unique in this namespace.",
			},
			"kind": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`tunnel` or `proxy`.",
			},
			"name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Display name.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Free-text note.",
			},
			"archived": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the app is archived.",
			},
			"log_bodies": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether tool call bodies are logged for this app.",
			},
			"redact": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.ListType{ElemType: types.StringType},
				MarkdownDescription: "Anchored regex patterns redacted from logged call arguments, keyed by field name.",
			},
			"redact_results": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.ListType{ElemType: types.StringType},
				MarkdownDescription: "Anchored regex patterns redacted from logged call results, keyed by field name.",
			},
			"endpoint": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The upstream endpoint. Proxied apps only; empty otherwise.",
			},
			"auth": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`headers` or `oauth`. Proxied apps only; empty otherwise.",
			},
			"forward_identity": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether caller identity is forwarded upstream. Proxied apps only; false otherwise.",
			},
			"roles": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.ObjectType{AttrTypes: roleFamiliesAttrTypes},
				MarkdownDescription: "Virtual role definitions, keyed by role name. Proxied apps only; empty otherwise.",
			},
			"capabilities": schema.SetAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The declared MCP capability families. Null means the " +
					"owner never declared any, which the hub reads as tools-only — distinct " +
					"from an explicitly empty set. Proxied apps only.",
			},
			"typescript_aliases": schema.SingleNestedAttribute{
				Computed: true,
				MarkdownDescription: "The owner's hub-local TypeScript names for this app's " +
					"canonical service and tools (§23.6), read separately from the hub's " +
					"resolved reservations. Empty when the owner never configured any.",
				Attributes: map[string]schema.Attribute{
					"service": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "TypeScript name of the canonical service, or null.",
					},
					"tools": schema.MapAttribute{
						Computed:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Canonical tool name → TypeScript name.",
					},
				},
			},
			"owner_roles": schema.MapAttribute{
				Computed:    true,
				ElementType: types.ObjectType{AttrTypes: roleFamiliesAttrTypes},
				MarkdownDescription: "The owner's own roles, keyed by role name, as " +
					"`pmcp_tunnel_app.owner_roles` sets them. Empty when the owner defined " +
					"none. Tunneled apps only; null otherwise.",
			},
		},
	}
}

func (d *appDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *appDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg appDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := d.client.AppGet(ctx, cfg.Slug.ValueString())
	if err != nil {
		// The builtin `pmcp` slug is refused by app_get itself (a distinct -32602, not the
		// absence sentence IsNotFound recognizes), so it surfaces here exactly like any other
		// failure: an error, never an empty result.
		hubDiag(&resp.Diagnostics, "app_get", err)
		return
	}

	redact, diags := redactFrom(ctx, row.Redact)
	resp.Diagnostics.Append(diags...)
	redactResults, diags := redactFrom(ctx, row.RedactResults)
	resp.Diagnostics.Append(diags...)
	roles, err := rolesFrom(ctx, row.Roles)
	if err != nil {
		resp.Diagnostics.AddError("Could not convert roles", err.Error())
	}
	ownerRoles, err := ownerRolesFrom(ctx, row.OwnerRoles)
	if err != nil {
		resp.Diagnostics.AddError("Could not convert owner_roles", err.Error())
	}
	var capabilities []string
	if row.Capabilities != nil {
		capabilities = *row.Capabilities
	}
	capSet, diags := stringSetFrom(ctx, capabilities)
	resp.Diagnostics.Append(diags...)
	aliases, diags := typescriptAliasesFrom(ctx, row.TypescriptAliases)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	model := appDataSourceModel{
		Slug:              types.StringValue(row.Slug),
		Kind:              types.StringValue(row.Kind),
		Name:              types.StringValue(row.Name),
		Description:       types.StringValue(row.Description),
		Archived:          types.BoolValue(row.Archived),
		LogBodies:         types.BoolValue(row.LogBodies),
		Redact:            redact,
		RedactResults:     redactResults,
		Endpoint:          types.StringValue(row.Endpoint),
		Auth:              types.StringValue(row.Auth),
		ForwardIdentity:   types.BoolValue(row.ForwardIdentity),
		Roles:             roles,
		Capabilities:      capSet,
		TypescriptAliases: aliases,
		OwnerRoles:        ownerRoles,
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
