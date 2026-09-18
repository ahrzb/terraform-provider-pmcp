package provider

import (
	"context"
	"testing"

	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
)

// ptConfigWithSlug builds a tfsdk.Config for a data source schema shaped like appDataSourceModel
// (one required "slug" string, the rest computed), with every other attribute null — as a real
// Terraform Config would have it, since a data source's computed attributes never appear in
// configuration. tfsdk.Config has no Set method (unlike Plan/State): Terraform is always the one
// writing a data source's config, so building one by hand means going through tftypes directly.
func ptConfigWithSlug(ctx context.Context, sch dschema.Schema, slug string) tfsdk.Config {
	objType := sch.Type().TerraformType(ctx).(tftypes.Object)
	values := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, attrType := range objType.AttributeTypes {
		if name == "slug" {
			values[name] = tftypes.NewValue(attrType, slug)
			continue
		}
		values[name] = tftypes.NewValue(attrType, nil)
	}
	return tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(objType, values)}
}

// TestAppDataSourceBuiltinSlugErrors covers §22.4: "Looking up the builtin pmcp slug is an
// error, not an empty result." app_get itself refuses that slug (assertSlugNotReserved) with a
// distinct -32602, not the absence sentence IsNotFound recognizes, so the data source's only
// correct behaviour is to let it surface as a diagnostic rather than returning a zero-value app.
func TestAppDataSourceBuiltinSlugErrors(t *testing.T) {
	ctx := context.Background()
	client := ptFakeHub(t, func(op string, args map[string]any) (any, *ptRPCError) {
		if op != "app_get" {
			t.Fatalf("expected app_get, got %q", op)
		}
		return nil, &ptRPCError{Code: -32602, Message: `the slug "pmcp" is reserved for the builtin admin app`}
	})

	d := &appDataSource{client: client}
	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)

	cfg := ptConfigWithSlug(ctx, schemaResp.Schema, "pmcp")

	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: schemaResp.Schema}}
	d.Read(ctx, datasource.ReadRequest{Config: cfg}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("looking up the builtin pmcp slug must be an error, not a successful empty result")
	}
}

// TestAppDataSourceExposesOwnerAliases covers §22.4's read duty for the alias attribute: the
// data source surfaces the owner's hub-local names from app_get's `typescriptAliases`, in the
// same typed object shape the resources use, so a consumer can read what a program would call
// without the hub's resolved reservations leaking into the typed surface.
func TestAppDataSourceExposesOwnerAliases(t *testing.T) {
	ctx := context.Background()
	client := ptFakeHub(t, func(op string, _ map[string]any) (any, *ptRPCError) {
		if op != "app_get" {
			t.Fatalf("expected app_get, got %q", op)
		}
		return map[string]any{"app": map[string]any{
			"slug": "app1", "kind": "tunnel", "name": "app1",
			"typescriptAliases": map[string]any{
				"service": "tools",
				"tools":   map[string]any{"paper_list": "paperList"},
			},
			"typescriptReservations": map[string]any{"service": "tools"},
		}}, nil
	})

	d := &appDataSource{client: client}
	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)

	objType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	resp := &datasource.ReadResponse{State: tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(objType, nil),
	}}
	d.Read(ctx, datasource.ReadRequest{Config: ptConfigWithSlug(ctx, schemaResp.Schema, "app1")}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	var got appDataSourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.TypescriptAliases.IsNull() {
		t.Fatal("typescript_aliases must carry the owner configuration, not null")
	}
	if !got.TypescriptAliases.Equal(ownerAliases("tools", map[string]string{"paper_list": "paperList"})) {
		t.Errorf("typescript_aliases = %v, want the row's owner configuration", got.TypescriptAliases)
	}
}

// TestTokensDataSourceSchemaOmitsLastUsedAt covers §22.4's explicit carve-out: last_used_at
// changes on every use, so including it in pmcp_tokens would make the data source dirty on
// refreshes unrelated to configuration. This pins the omission directly against the schema
// rather than the wire, so it fails if the attribute is ever added back.
func TestTokensDataSourceSchemaOmitsLastUsedAt(t *testing.T) {
	ctx := context.Background()
	d := &tokensDataSource{}
	var resp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &resp)

	tokens, ok := resp.Schema.Attributes["tokens"].(dschema.ListNestedAttribute)
	if !ok {
		t.Fatalf("tokens attribute is %T, want ListNestedAttribute", resp.Schema.Attributes["tokens"])
	}
	if _, present := tokens.NestedObject.Attributes["last_used_at"]; present {
		t.Error("pmcp_tokens must never expose last_used_at — see §22.4")
	}
	for _, want := range []string{"id", "kind", "ref_slug", "prefix", "created_at", "expires_at", "revoked_at"} {
		if _, present := tokens.NestedObject.Attributes[want]; !present {
			t.Errorf("tokens element is missing %q", want)
		}
	}
}
