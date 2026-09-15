// Package provider: schema fragments, model conversion, and validators shared by
// pmcp_tunnel_app and pmcp_proxy_app (§22.4). Reuses roleFamiliesAttrTypes/roleFamiliesValue/
// rolesFrom from data_app.go rather than redeclaring the map(object) shape a second time.
package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// commonAppModel is every attribute `pmcp_tunnel_app` has and `pmcp_proxy_app` inherits
// (§22.4: "The above with `log_bodies` default `false`, plus: ..."). Embedded anonymously in
// both resources' models so the framework's reflection-based Get/Set promotes its fields —
// getStructTags in terraform-plugin-framework's internal/reflect package supports this
// explicitly, which is what makes one copy of these seven fields possible.
type commonAppModel struct {
	Slug          types.String `tfsdk:"slug"`
	Name          types.String `tfsdk:"name"`
	Description   types.String `tfsdk:"description"`
	Archived      types.Bool   `tfsdk:"archived"`
	Redact        types.Map    `tfsdk:"redact"`
	RedactResults types.Map    `tfsdk:"redact_results"`
	LogBodies     types.Bool   `tfsdk:"log_bodies"`
}

// slugPattern mirrors registry.ts's SLUG_CHARSET exactly — the constraint the hub's own tools
// advertise and enforce, restated here so a malformed slug fails at `tofu plan`, not at apply.
var slugPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// slugValidator refuses a slug that would not survive app_create/app_get anyway: the charset
// registry.ts enforces, and the `pmcp` slug reserved for the builtin app (§22.4).
type slugValidator struct{}

func (slugValidator) Description(context.Context) string {
	return `must match [a-z0-9-]+ and must not be "pmcp"`
}

func (v slugValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (slugValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	if !slugPattern.MatchString(value) {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid slug",
			fmt.Sprintf("%q must match [a-z0-9-]+.", value))
		return
	}
	if value == pmcpBuiltinSlug {
		resp.Diagnostics.AddAttributeError(req.Path, "Reserved slug",
			`"pmcp" is reserved for the hub's own builtin app.`)
	}
}

// pmcpBuiltinSlug is the hub's virtual admin app — never a real app_create target (§8/§22.4).
const pmcpBuiltinSlug = "pmcp"

// anchoredPatternKeysValidator rejects a `redact`/`redact_results` key that cannot compile as
// an anchored pattern, mirroring registry.ts's compilePattern (`^(?:pattern)$`, with `*`
// aliasing to `.*` before compilation — approximated here as a plain compile, since the two
// engines' feature sets coincide for every pattern this validator needs to catch). Without
// this, a malformed key is stored as a RegistryRefusal at apply time, well after the operator
// could have been told at `tofu plan`.
type anchoredPatternKeysValidator struct{}

func (anchoredPatternKeysValidator) Description(context.Context) string {
	return "every key must compile as an anchored regular expression"
}

func (v anchoredPatternKeysValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (anchoredPatternKeysValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for key := range req.ConfigValue.Elements() {
		if _, err := regexp.Compile("^(?:" + key + ")$"); err != nil {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid pattern key",
				fmt.Sprintf("%q does not compile as a pattern: %s", key, err))
		}
	}
}

// commonAppAttributes is `pmcp_tunnel_app`'s full schema and `pmcp_proxy_app`'s starting point
// (§22.4). None of the optional+computed fields carries a static `Default`: the hub resolves
// every one of them (slug for `name`, `""` for `description`, `false` for `archived`, `{}` for
// the redaction maps, by-kind for `log_bodies`), so Create/Update simply echo back whatever the
// row that RPC returned — the same pattern agentResourceModel uses one file over. Attaching
// `UseStateForUnknown` is still required: without it, ANY other attribute changing on the same
// resource would re-mark every one of these unknown for no reason (a computed attribute
// differing from prior state is otherwise assumed changed on every apply that touches the
// resource at all — see ModifyPlan's doc comment on `pmcp_proxy_app` for the mechanism this
// sidesteps).
func commonAppAttributes() map[string]schema.Attribute {
	useStateForUnknownString := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	useStateForUnknownBool := []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}
	useStateForUnknownMap := []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}

	return map[string]schema.Attribute{
		"slug": schema.StringAttribute{
			Required: true,
			MarkdownDescription: "The app's slug, unique in this namespace. `[a-z0-9-]+`; the " +
				"builtin `pmcp` slug is reserved. Forces replacement.",
			PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			Validators:    []validator.String{slugValidator{}},
		},
		"name": schema.StringAttribute{
			Optional:            true,
			Computed:            true,
			MarkdownDescription: "Display name. The hub defaults it to `slug` when omitted at create.",
			PlanModifiers:       useStateForUnknownString,
		},
		"description": schema.StringAttribute{
			Optional:            true,
			Computed:            true,
			MarkdownDescription: "Free-text note. Defaults to `\"\"`.",
			PlanModifiers:       useStateForUnknownString,
		},
		"archived": schema.BoolAttribute{
			Optional: true,
			Computed: true,
			MarkdownDescription: "Hides the app from consumers, retaining everything. Defaults " +
				"to `false`. Fires `app_archive`/`app_unarchive` — never `app_update`.",
			PlanModifiers: useStateForUnknownBool,
		},
		"redact": schema.MapAttribute{
			ElementType:         types.ListType{ElemType: types.StringType},
			Optional:            true,
			Computed:            true,
			MarkdownDescription: "Tool-or-pattern → sensitive ARGUMENT paths. Keys are anchored regular expressions and must compile.",
			PlanModifiers:       useStateForUnknownMap,
			Validators:          []validator.Map{anchoredPatternKeysValidator{}},
		},
		"redact_results": schema.MapAttribute{
			ElementType:         types.ListType{ElemType: types.StringType},
			Optional:            true,
			Computed:            true,
			MarkdownDescription: "Tool-or-pattern → sensitive RESULT paths. Same key grammar as `redact`.",
			PlanModifiers:       useStateForUnknownMap,
			Validators:          []validator.Map{anchoredPatternKeysValidator{}},
		},
		"log_bodies": schema.BoolAttribute{
			Optional: true,
			Computed: true,
			MarkdownDescription: "Records call bodies in the audit ledger. Defaults by kind " +
				"when omitted: `true` for a tunneled app, `false` for a proxied one.",
			PlanModifiers: useStateForUnknownBool,
		},
	}
}

// commonFromRow converts app_get/app_create/app_update/app_archive's row into the fields both
// app resources share. Proxy-only fields are the caller's job.
func commonFromRow(ctx context.Context, row pmcp.AppRow) (commonAppModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	redact, d := redactFrom(ctx, row.Redact)
	diags.Append(d...)
	redactResults, d2 := redactFrom(ctx, row.RedactResults)
	diags.Append(d2...)
	return commonAppModel{
		Slug:          types.StringValue(row.Slug),
		Name:          types.StringValue(row.Name),
		Description:   types.StringValue(row.Description),
		Archived:      types.BoolValue(row.Archived),
		Redact:        redact,
		RedactResults: redactResults,
		LogBodies:     types.BoolValue(row.LogBodies),
	}, diags
}

// appendCommonArgs adds the fields both app resources share to an app_create/app_update
// argument map, using whatever the plan already resolved them to. It never diffs against
// state — matching admin.ts's own commonFields (keyed on presence, not change) and this
// package's agent.go precedent (Update sends name/description unconditionally). Callers that
// need to avoid a no-op app_update entirely (because only `archived` or headers changed) gate
// the call itself on commonAppChanged rather than filtering fields here.
func appendCommonArgs(ctx context.Context, args map[string]any, plan commonAppModel, diags *diag.Diagnostics) {
	if name := optionalString(plan.Name); name != nil {
		args["name"] = *name
	}
	if description := optionalString(plan.Description); description != nil {
		args["description"] = *description
	}
	if logBodies := optionalBool(plan.LogBodies); logBodies != nil {
		args["log_bodies"] = *logBodies
	}
	redact, d := redactTo(ctx, plan.Redact)
	diags.Append(d...)
	if redact != nil {
		args["redact"] = redact
	}
	redactResults, d2 := redactTo(ctx, plan.RedactResults)
	diags.Append(d2...)
	if redactResults != nil {
		args["redact_results"] = redactResults
	}
}

// commonAppChanged reports whether any field app_update patches (as opposed to the ones with
// their own op — `archived`, and the proxy resource's headers pair) differs between plan and
// state. Update() gates its app_update call on this so that an apply touching only `archived`
// does not also resend every other field — which would misrepresent them as touched in the
// hub's own audit row (admin.ts's app_update handler audits `Object.keys(patch)` verbatim, so a
// resent-but-unchanged field is indistinguishable from a real edit).
func commonAppChanged(plan, state commonAppModel) bool {
	return !plan.Name.Equal(state.Name) ||
		!plan.Description.Equal(state.Description) ||
		!plan.Redact.Equal(state.Redact) ||
		!plan.RedactResults.Equal(state.RedactResults) ||
		!plan.LogBodies.Equal(state.LogBodies)
}

// rolesAttribute is `pmcp_proxy_app`'s `roles` (§22.4): the typed object form only, never the
// wire's bare-list sugar, which a static schema cannot express. `tools`/`prompts`/`resources`
// are individually optional+computed — not merely optional — so the provider may normalize an
// explicitly empty family list away to null (matching the hub's own canonical rendering, which
// omits an empty family entirely) without a "provider produced inconsistent result" error;
// Terraform only requires an Optional (non-computed) leaf to be echoed back byte-for-byte, and
// dropping that requirement for these three is what "comparison drops empty families" means in
// practice, not merely in prose.
func rolesAttribute() schema.MapNestedAttribute {
	listUseState := []planmodifier.List{listplanmodifier.UseStateForUnknown()}
	return schema.MapNestedAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Virtual role definitions, keyed by role name. Each entry is the " +
			"typed per-family object — never the wire's bare-pattern-list sugar, which the " +
			"plugin framework cannot express as one static type; the terranix module " +
			"normalizes the bare-list form into this shape. Comparison drops empty families, " +
			"so the hub's canonical rendering (a bare list for a tools-only role) never diffs.",
		PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
		NestedObject: schema.NestedAttributeObject{
			Attributes: map[string]schema.Attribute{
				"tools": schema.ListAttribute{
					ElementType:         types.StringType,
					Optional:            true,
					Computed:            true,
					MarkdownDescription: "Tool-name patterns this role grants.",
					PlanModifiers:       listUseState,
				},
				"prompts": schema.ListAttribute{
					ElementType:         types.StringType,
					Optional:            true,
					Computed:            true,
					MarkdownDescription: "Prompt-name patterns this role grants.",
					PlanModifiers:       listUseState,
				},
				"resources": schema.ListAttribute{
					ElementType:         types.StringType,
					Optional:            true,
					Computed:            true,
					MarkdownDescription: "Resource-URI patterns this role grants.",
					PlanModifiers:       listUseState,
				},
			},
		},
	}
}

// capabilitiesAttribute is `pmcp_proxy_app`'s `capabilities` (§22.4). Absent means `["tools"]`,
// not `[]` — the hub omits the key entirely when undeclared and §20.2 reads that as tools-only
// — so this provider represents "never declared" as a null set (never an empty one) and never
// normalizes it away on read. Typed as a Set so element ordering never diffs.
func capabilitiesAttribute() schema.SetAttribute {
	return schema.SetAttribute{
		ElementType: types.StringType,
		Optional:    true,
		Computed:    true,
		MarkdownDescription: "Declared MCP capability families (`tools`, `prompts`, " +
			"`resources`, `completions`). Null means never declared, which the hub reads as " +
			"tools-only — distinct from an explicitly empty set. Because `app_update` has no " +
			"unset, an update that changes this sends the resolved set explicitly.",
		PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
	}
}

// rolesToArgs converts the `roles` attribute to app_create/app_update's wire shape. A null or
// unknown map yields nil, which callers omit from the op arguments — the same "leave it alone"
// convention convert.go's redactTo/stringSetTo use. A known map — even an empty one, `{}` —
// yields a non-nil map, so a declared-empty `roles` is still sent explicitly.
func rolesToArgs(ctx context.Context, in types.Map) (map[string]pmcp.RoleFamilies, diag.Diagnostics) {
	if in.IsNull() || in.IsUnknown() {
		return nil, nil
	}
	var values map[string]roleFamiliesValue
	diags := in.ElementsAs(ctx, &values, false)
	out := make(map[string]pmcp.RoleFamilies, len(values))
	for name, v := range values {
		out[name] = pmcp.RoleFamilies{Tools: v.Tools, Prompts: v.Prompts, Resources: v.Resources}
	}
	return out, diags
}
