package coverage

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// rawVal is a plan/config/state literal used to build a tftypes.Value tree without the
// resource package's own (unexported) tfsdk-tagged model structs — this package is deliberately
// outside internal/provider, so it drives each resource purely through the framework's public
// resource.Resource/datasource.DataSource surface and the Terraform-protocol value
// representation every provider is ultimately tested against.
//
// A rawVal is one of: nil (null), unknownVal (Terraform's "unknown"), a Go native scalar
// (string, bool, int, int64), []rawVal for a list/set, or map[string]rawVal for a map/object.
type rawVal any

type unknownSentinel struct{}

// unknownVal marks an attribute as unknown — what Terraform Core presents to Create/Update for
// an optional+computed attribute nothing configured, and what this package uses to simulate
// pmcp_proxy_app's ModifyPlan marking headers_applied_version unknown (§22.2) without actually
// invoking ModifyPlan.
var unknownVal rawVal = unknownSentinel{}

// buildValue renders v as a tftypes.Value of type ty, recursing through Object/Map/List/Set.
// An Object fills any attribute absent from v with a null of its own type, so a scenario needs
// to spell out only the attributes it cares about.
func buildValue(ty tftypes.Type, v rawVal) tftypes.Value {
	if v == nil {
		return tftypes.NewValue(ty, nil)
	}
	if _, ok := v.(unknownSentinel); ok {
		return tftypes.NewValue(ty, tftypes.UnknownValue)
	}
	switch t := ty.(type) {
	case tftypes.Object:
		m, ok := v.(map[string]rawVal)
		if !ok {
			panic(fmt.Sprintf("buildValue: %s needs map[string]rawVal, got %T", ty, v))
		}
		full := make(map[string]tftypes.Value, len(t.AttributeTypes))
		for name, at := range t.AttributeTypes {
			if val, present := m[name]; present {
				full[name] = buildValue(at, val)
			} else {
				full[name] = tftypes.NewValue(at, nil)
			}
		}
		return tftypes.NewValue(ty, full)
	case tftypes.Map:
		m, ok := v.(map[string]rawVal)
		if !ok {
			panic(fmt.Sprintf("buildValue: %s needs map[string]rawVal, got %T", ty, v))
		}
		full := make(map[string]tftypes.Value, len(m))
		for k, val := range m {
			full[k] = buildValue(t.ElementType, val)
		}
		return tftypes.NewValue(ty, full)
	case tftypes.List:
		items, ok := v.([]rawVal)
		if !ok {
			panic(fmt.Sprintf("buildValue: %s needs []rawVal, got %T", ty, v))
		}
		vals := make([]tftypes.Value, len(items))
		for i, val := range items {
			vals[i] = buildValue(t.ElementType, val)
		}
		return tftypes.NewValue(ty, vals)
	case tftypes.Set:
		items, ok := v.([]rawVal)
		if !ok {
			panic(fmt.Sprintf("buildValue: %s needs []rawVal, got %T", ty, v))
		}
		vals := make([]tftypes.Value, len(items))
		for i, val := range items {
			vals[i] = buildValue(t.ElementType, val)
		}
		return tftypes.NewValue(ty, vals)
	default:
		// String, Bool, Number, DynamicPseudoType: NewValue accepts the Go-native
		// representation (string/bool/int/int64/...) directly.
		return tftypes.NewValue(ty, v)
	}
}

// typedAttribute is the method both resource/schema.Attribute and datasource/schema.Attribute
// promote from their shared (unexported, hence not directly importable) fwschema.Attribute —
// enough surface to derive a tftypes.Object from either schema flavor generically.
type typedAttribute interface {
	GetType() attr.Type
}

// objectTypeOf derives the Terraform object type for a schema's top-level Attributes map,
// working for both resource and datasource schemas via the shared GetType() method.
func objectTypeOf[A typedAttribute](ctx context.Context, attrs map[string]A) tftypes.Object {
	types := make(map[string]tftypes.Type, len(attrs))
	for name, a := range attrs {
		types[name] = a.GetType().TerraformType(ctx)
	}
	return tftypes.Object{AttributeTypes: types}
}

// resourceSchemaOf calls a resource's Schema method the way the framework itself would, without
// needing terraform-plugin-testing's acceptance harness (§22.5: "the parity oracle is this
// httptest fake, not an acceptance-test harness").
func resourceSchemaOf(ctx context.Context, res resource.Resource) rschema.Schema {
	var resp resource.SchemaResponse
	res.Schema(ctx, resource.SchemaRequest{}, &resp)
	return resp.Schema
}

func dataSourceSchemaOf(ctx context.Context, ds datasource.DataSource) dschema.Schema {
	var resp datasource.SchemaResponse
	ds.Schema(ctx, datasource.SchemaRequest{}, &resp)
	return resp.Schema
}

// configureResource drives a resource's Configure method against client, mirroring
// internal/provider's own test helper of the same shape.
func configureResource(ctx context.Context, res resource.Resource, client any) error {
	c, ok := res.(resource.ResourceWithConfigure)
	if !ok {
		return fmt.Errorf("%T does not implement resource.ResourceWithConfigure", res)
	}
	var resp resource.ConfigureResponse
	c.Configure(ctx, resource.ConfigureRequest{ProviderData: client}, &resp)
	if resp.Diagnostics.HasError() {
		return fmt.Errorf("Configure: %v", resp.Diagnostics)
	}
	return nil
}

func configureDataSource(ctx context.Context, ds datasource.DataSource, client any) error {
	c, ok := ds.(datasource.DataSourceWithConfigure)
	if !ok {
		return fmt.Errorf("%T does not implement datasource.DataSourceWithConfigure", ds)
	}
	var resp datasource.ConfigureResponse
	c.Configure(ctx, datasource.ConfigureRequest{ProviderData: client}, &resp)
	if resp.Diagnostics.HasError() {
		return fmt.Errorf("Configure: %v", resp.Diagnostics)
	}
	return nil
}

// nullResourceState returns a zero-value tfsdk.State carrying sch, ready for a
// Create/Read/Update response to Set into — mirroring how the framework pre-populates
// CreateResponse.State/UpdateResponse.State from the request's schema before calling the
// resource, and how it initializes ReadResponse.State from the request's prior State.
func nullResourceState(sch rschema.Schema, ty tftypes.Object) tfsdk.State {
	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(ty, nil)}
}

func nullDataSourceState(sch dschema.Schema, ty tftypes.Object) tfsdk.State {
	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(ty, nil)}
}
