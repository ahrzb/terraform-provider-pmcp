package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// One spelling of every conversion that crosses the framework/wire boundary, and one spelling of
// how a hub failure becomes a diagnostic. Both exist because the alternative is per-resource
// copies that drift: five resources each deciding what null means, or each writing its own
// sentence for the same -32601.

// hubDiag turns a client error into a diagnostic against the op that produced it.
//
// The -32601 branch is the reason this is not a one-line fmt.Errorf at each call site. The hub
// pins that code to "method not found", which for this provider means exactly one thing: the hub
// is older than the provider and is missing an operation §22 requires. A bare "rpc error -32601"
// leaves the operator guessing; naming the op and the required change is actionable.
func hubDiag(diags *diag.Diagnostics, op string, err error) {
	var rpc *pmcp.RPCError
	if errors.As(err, &rpc) && rpc.MethodNotFound() {
		diags.AddError(
			fmt.Sprintf("Hub does not support %q", op),
			fmt.Sprintf(
				"The hub rejected %q as an unknown operation (-32601). This provider requires it, "+
					"so the hub is older than the provider. Deploy a hub that implements %q, then "+
					"re-run. No change was made.",
				op, op,
			),
		)
		return
	}

	var httpErr *pmcp.HTTPError
	if errors.As(err, &httpErr) {
		diags.AddError(
			fmt.Sprintf("Hub rejected the %s request", op),
			fmt.Sprintf(
				"The request never reached the hub's MCP dispatcher: %s. Check the provider's "+
					"endpoint and admin token.",
				httpErr.Error(),
			),
		)
		return
	}

	diags.AddError(fmt.Sprintf("Hub call %q failed", op), err.Error())
}

// redactTo converts a `map(list(string))` attribute to the wire's shape. A null or unknown map
// yields nil, which callers omit from op arguments — "leave it alone", not "set it empty".
func redactTo(ctx context.Context, in types.Map) (map[string][]string, diag.Diagnostics) {
	if in.IsNull() || in.IsUnknown() {
		return nil, nil
	}
	out := make(map[string][]string, len(in.Elements()))
	diags := in.ElementsAs(ctx, &out, false)
	return out, diags
}

// redactFrom converts the wire's shape back to a `map(list(string))`. An absent map becomes an
// empty map rather than null, because the hub always reports both redaction maps: the hub's
// answer to "nothing redacted" is `{}`, and rendering that as null would diff forever.
func redactFrom(ctx context.Context, in map[string][]string) (types.Map, diag.Diagnostics) {
	if in == nil {
		in = map[string][]string{}
	}
	return types.MapValueFrom(ctx, types.ListType{ElemType: types.StringType}, in)
}

// stringSetTo converts a `set(string)` attribute to a slice. Null or unknown yields nil, which
// is distinguishable from an explicitly empty set — a distinction `capabilities` depends on.
func stringSetTo(ctx context.Context, in types.Set) ([]string, diag.Diagnostics) {
	if in.IsNull() || in.IsUnknown() {
		return nil, nil
	}
	out := make([]string, 0, len(in.Elements()))
	diags := in.ElementsAs(ctx, &out, false)
	return out, diags
}

// stringSetFrom converts a slice to a `set(string)`. A nil slice becomes a null set, preserving
// "the owner declared nothing" for attributes where absence carries meaning.
func stringSetFrom(ctx context.Context, in []string) (types.Set, diag.Diagnostics) {
	if in == nil {
		return types.SetNull(types.StringType), nil
	}
	return types.SetValueFrom(ctx, types.StringType, in)
}

// optionalString is the argument-building idiom for ops whose omitted fields mean "leave
// unchanged". Null and unknown both yield nil; only a known value is sent.
//
// Unknown maps to nil deliberately: an unknown here means the value depends on something not yet
// applied, and sending it would serialize a placeholder. The framework never presents an unknown
// to Create or Update for a configured attribute, so this is a guard, not a code path.
func optionalString(in types.String) *string {
	if in.IsNull() || in.IsUnknown() {
		return nil
	}
	v := in.ValueString()
	return &v
}

// optionalBool is optionalString for booleans: nil unless explicitly set.
func optionalBool(in types.Bool) *bool {
	if in.IsNull() || in.IsUnknown() {
		return nil
	}
	v := in.ValueBool()
	return &v
}
