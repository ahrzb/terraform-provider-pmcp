// Package provider is the Terraform Plugin Framework provider for a personal MCP hub.
//
// Six resources and three data sources, whose schema and lifecycle rules are specified in §22
// of the hub's design spec. This file carries the provider block, the configure path every
// resource depends on, and the two registration lists — which are the only place a new resource
// becomes reachable, so an implemented-but-unregistered type is the failure to look for here.
package provider

import (
	"context"
	"os"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// New returns the provider factory. `version` is baked in at build time and reported to
// Terraform/OpenTofu as the provider version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &hubProvider{version: version} }
}

type hubProvider struct {
	version string
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

func (p *hubProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "pmcp"
	resp.Version = p.version
}

func (p *hubProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the contents of a personal MCP hub — apps, agents, grants, " +
			"and upstream credentials. The hub's own deployment is owned by Wrangler, not by this provider.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Hub origin, e.g. `https://hub.example.workers.dev`. " +
					"Falls back to `PMCP_URL`.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "An admin token (`pmcp_adm_…`). Falls back to " +
					"`PMCP_ADMIN_TOKEN`. Deliberately not `PMCP_TOKEN`: that variable holds the " +
					"CLI's session bearer, which is a different credential family with a " +
					"different acceptance surface and a sliding expiry.",
			},
		},
	}
}

// Configure resolves credentials config-then-env — no config-file fallback, so an operator's
// stale `~/.config/pmcp/config.toml` profile can never shadow a CI credential — and then asks
// the hub which namespace the credential owns.
func (p *hubProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv("PMCP_URL"))
	token := firstNonEmpty(cfg.Token.ValueString(), os.Getenv("PMCP_ADMIN_TOKEN"))

	if endpoint == "" {
		resp.Diagnostics.AddError(
			"No hub endpoint",
			"Set the provider's `endpoint` attribute or the PMCP_URL environment variable.",
		)
	}
	if token == "" {
		resp.Diagnostics.AddError(
			"No admin token",
			"Set the provider's `token` attribute or the PMCP_ADMIN_TOKEN environment variable. "+
				"Mint one with `pmcp admin-token issue` from a signed-in session.",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := pmcp.New(ctx, endpoint, token)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the hub as an administrator", err.Error())
		return
	}

	resp.ResourceData = client
	resp.DataSourceData = client
}

// The §22.4 surface: six resources and three data sources. `pmcp_token` is listed with the
// resources it is issued against rather than beside the apps, because its lifecycle is the odd
// one — see §22.2.
func (p *hubProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewTunnelAppResource,
		NewProxyAppResource,
		NewAgentResource,
		NewGrantResource,
		NewTokenResource,
		NewHubSettingsResource,
	}
}

func (p *hubProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewAppDataSource,
		NewAgentDataSource,
		NewTokensDataSource,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
