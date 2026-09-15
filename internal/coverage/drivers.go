package coverage

import (
	"context"
	"fmt"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/ahrzb/terraform-provider-pmcp/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
)

// pathExpectation is assertion 1's declared expectation (§22.5, "Per-path"): one scenario's
// resource/action and the exact op sequence it must produce, in order. This part of the oracle
// is deliberately hand-authored — there are only ~20 resource/action pairs and they change
// rarely — but only the *op sequence per path* is authored here; which *fields* each op carries
// is never hand-declared anywhere in this package, only read back from what the real call
// recorded (see checkFieldCoverage in oracle.go). §22.5 draws exactly this line: "A union of
// names would survive swapping app_update into Delete and app_delete into Update," which is why
// per-path checks the sequence, not a set.
type pathExpectation struct {
	Path    string
	WantOps []string
	GotOps  []string
}

// driverResult is one resource/data-source scenario's output: every op call it made (feeding
// totality and field coverage), the per-action expectations it produced (feeding per-path), and
// any contract-schema validation failures the fake observed along the way.
type driverResult struct {
	Calls          []recordedCall
	Expectations   []pathExpectation
	ValidationErrs []error
}

func opsSince(f *fakeHub, from int) []string {
	ops := make([]string, 0, len(f.calls)-from)
	for _, c := range f.calls[from:] {
		ops = append(ops, c.Op)
	}
	return ops
}

func finish(out *driverResult, f *fakeHub) driverResult {
	out.Calls = f.calls
	out.ValidationErrs = f.validationErrs
	return *out
}

// allDrivers runs every resource and data source scenario against contract, aggregating their
// results. One failed scenario does not abort the rest — each is independent, and reporting
// every failure in one pass is more useful than stopping at the first.
func allDrivers(ctx context.Context, contract *Contract) (driverResult, []error) {
	var total driverResult
	var fatal []error
	for _, drive := range []func(context.Context, *Contract) (driverResult, error){
		driveAgent,
		driveTunnelApp,
		driveProxyApp,
		driveGrant,
		driveToken,
		driveAppDataSource,
		driveAgentDataSource,
		driveTokensDataSource,
	} {
		r, err := drive(ctx, contract)
		if err != nil {
			fatal = append(fatal, err)
			continue
		}
		total.Calls = append(total.Calls, r.Calls...)
		total.Expectations = append(total.Expectations, r.Expectations...)
		total.ValidationErrs = append(total.ValidationErrs, r.ValidationErrs...)
	}
	return total, fatal
}

// driveAgent drives pmcp_agent through Create, Read, Update, Delete.
func driveAgent(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	res := provider.NewAgentResource()
	sch := resourceSchemaOf(ctx, res)
	ty := objectTypeOf(ctx, sch.Attributes)

	var row pmcp.AgentRow
	f := &fakeHub{contract: contract}
	f.reply = func(op string, args map[string]any) (any, *rpcErrorBody) {
		switch op {
		case "agent_create":
			row = pmcp.AgentRow{Slug: str(args, "slug"), Name: strDefault(args, "name", str(args, "slug")), Description: strDefault(args, "description", "")}
			return map[string]any{"agent": row}, nil
		case "agent_update":
			row.Name = strDefault(args, "name", row.Name)
			row.Description = strDefault(args, "description", row.Description)
			return map[string]any{"agent": row}, nil
		case "agent_list":
			return map[string]any{"agents": []pmcp.AgentRow{row}}, nil
		case "agent_delete":
			return map[string]any{}, nil
		default:
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureResource(ctx, res, client); err != nil {
		return out, err
	}

	at := len(f.calls)
	createPlan := tfsdk.Plan{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"slug": "agentx", "name": "Agent X", "description": "does stuff",
	})}
	createResp := &resource.CreateResponse{State: nullResourceState(sch, ty)}
	res.Create(ctx, resource.CreateRequest{Plan: createPlan}, createResp)
	if createResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_agent Create: %v", createResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_agent/Create", WantOps: []string{"agent_create"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	readState := tfsdk.State{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"slug": "agentx", "name": "Agent X", "description": "does stuff",
	})}
	readResp := &resource.ReadResponse{State: readState}
	res.Read(ctx, resource.ReadRequest{State: readState}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_agent Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_agent/Read", WantOps: []string{"agent_list"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	updatePlan := tfsdk.Plan{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"slug": "agentx", "name": "Agent X Renamed", "description": "does more stuff",
	})}
	updateResp := &resource.UpdateResponse{State: nullResourceState(sch, ty)}
	res.Update(ctx, resource.UpdateRequest{Plan: updatePlan, State: readState}, updateResp)
	if updateResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_agent Update: %v", updateResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_agent/Update", WantOps: []string{"agent_update"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	deleteResp := &resource.DeleteResponse{State: readState}
	res.Delete(ctx, resource.DeleteRequest{State: readState}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_agent Delete: %v", deleteResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_agent/Delete", WantOps: []string{"agent_delete"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}

// driveTunnelApp drives pmcp_tunnel_app through Create (with archived=true, exercising
// app_archive in the same apply), Read, Update (flipping back to unarchived), Delete.
func driveTunnelApp(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	res := provider.NewTunnelAppResource()
	sch := resourceSchemaOf(ctx, res)
	ty := objectTypeOf(ctx, sch.Attributes)

	var row pmcp.AppRow
	f := &fakeHub{contract: contract}
	f.reply = func(op string, args map[string]any) (any, *rpcErrorBody) {
		switch op {
		case "app_create":
			row = pmcp.AppRow{
				Slug: str(args, "slug"), Kind: "tunnel",
				Name: strDefault(args, "name", str(args, "slug")), Description: strDefault(args, "description", ""),
				LogBodies: boolDefault(args, "log_bodies", true),
				Redact:    mapListFromArgs(args, "redact"), RedactResults: mapListFromArgs(args, "redact_results"),
			}
			return map[string]any{"app": row}, nil
		case "app_update":
			row.Name = strDefault(args, "name", row.Name)
			row.Description = strDefault(args, "description", row.Description)
			row.LogBodies = boolDefault(args, "log_bodies", row.LogBodies)
			if r, ok := args["redact"]; ok {
				row.Redact = mapListFromAny(r)
			}
			if r, ok := args["redact_results"]; ok {
				row.RedactResults = mapListFromAny(r)
			}
			return map[string]any{"app": row}, nil
		case "app_get":
			return map[string]any{"app": row}, nil
		case "app_archive":
			row.Archived = true
			return map[string]any{}, nil
		case "app_unarchive":
			row.Archived = false
			return map[string]any{}, nil
		case "app_delete":
			return map[string]any{}, nil
		default:
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureResource(ctx, res, client); err != nil {
		return out, err
	}

	at := len(f.calls)
	createPlan := tfsdk.Plan{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"slug": "bot1", "name": "Bot One", "description": "does things",
		"archived": true, "log_bodies": false,
		"redact":         map[string]rawVal{"^get_.*$": []rawVal{"path"}},
		"redact_results": map[string]rawVal{"^get_.*$": []rawVal{"secret"}},
	})}
	createResp := &resource.CreateResponse{State: nullResourceState(sch, ty)}
	res.Create(ctx, resource.CreateRequest{Plan: createPlan}, createResp)
	if createResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_tunnel_app Create: %v", createResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_tunnel_app/Create", WantOps: []string{"app_create", "app_archive"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	readState := tfsdk.State{Schema: sch, Raw: buildValue(ty, map[string]rawVal{"slug": "bot1"})}
	readResp := &resource.ReadResponse{State: readState}
	res.Read(ctx, resource.ReadRequest{State: readState}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_tunnel_app Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_tunnel_app/Read", WantOps: []string{"app_get"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	priorState := tfsdk.State{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"slug": "bot1", "name": "Bot One", "description": "does things",
		"archived": true, "log_bodies": false,
		"redact":         map[string]rawVal{"^get_.*$": []rawVal{"path"}},
		"redact_results": map[string]rawVal{"^get_.*$": []rawVal{"secret"}},
	})}
	updatePlan := tfsdk.Plan{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"slug": "bot1", "name": "Bot One Updated", "description": "still doing things",
		"archived": false, "log_bodies": true,
		"redact":         map[string]rawVal{"^set_.*$": []rawVal{"password"}},
		"redact_results": map[string]rawVal{"^err_.*$": []rawVal{"stack"}},
	})}
	updateResp := &resource.UpdateResponse{State: nullResourceState(sch, ty)}
	res.Update(ctx, resource.UpdateRequest{Plan: updatePlan, State: priorState}, updateResp)
	if updateResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_tunnel_app Update: %v", updateResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_tunnel_app/Update", WantOps: []string{"app_update", "app_unarchive"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	deleteResp := &resource.DeleteResponse{State: readState}
	res.Delete(ctx, resource.DeleteRequest{State: readState}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_tunnel_app Delete: %v", deleteResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_tunnel_app/Delete", WantOps: []string{"app_delete"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}

// driveProxyApp drives pmcp_proxy_app through Create (auth=headers, converging on
// app_set_upstream_auth), Read, Update (every app_update-owned field changes, and headers
// converge to a new version by marking headers_applied_version unknown in Plan — simulating
// what ModifyPlan would have produced, §22.2 — rather than invoking ModifyPlan itself), Delete.
// archived stays false throughout: pmcp_tunnel_app's scenario already covers app_archive/
// app_unarchive, and keeping this one at two ops keeps its per-path expectation legible.
func driveProxyApp(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	res := provider.NewProxyAppResource()
	sch := resourceSchemaOf(ctx, res)
	ty := objectTypeOf(ctx, sch.Attributes)

	var row pmcp.AppRow
	f := &fakeHub{contract: contract}
	f.reply = func(op string, args map[string]any) (any, *rpcErrorBody) {
		switch op {
		case "app_create":
			row = pmcp.AppRow{
				Slug: str(args, "slug"), Kind: "proxy",
				Name: strDefault(args, "name", str(args, "slug")), Description: strDefault(args, "description", ""),
				LogBodies: boolDefault(args, "log_bodies", false),
				Redact:    mapListFromArgs(args, "redact"), RedactResults: mapListFromArgs(args, "redact_results"),
				Endpoint: str(args, "endpoint"), Auth: strDefault(args, "auth", "headers"),
				ForwardIdentity: boolDefault(args, "forward_identity", false),
				Roles:           rolesFromArgs(args, "roles"),
			}
			if caps, ok := args["capabilities"]; ok {
				c := strSlice(caps)
				row.Capabilities = &c
			}
			return map[string]any{"app": row}, nil
		case "app_update":
			row.Name = strDefault(args, "name", row.Name)
			row.Description = strDefault(args, "description", row.Description)
			row.LogBodies = boolDefault(args, "log_bodies", row.LogBodies)
			if r, ok := args["redact"]; ok {
				row.Redact = mapListFromAny(r)
			}
			if r, ok := args["redact_results"]; ok {
				row.RedactResults = mapListFromAny(r)
			}
			if e, ok := args["endpoint"].(string); ok {
				row.Endpoint = e
			}
			row.Auth = strDefault(args, "auth", row.Auth)
			row.ForwardIdentity = boolDefault(args, "forward_identity", row.ForwardIdentity)
			if r, ok := args["roles"]; ok {
				row.Roles = rolesFromAny(r)
			}
			if c, ok := args["capabilities"]; ok {
				s := strSlice(c)
				row.Capabilities = &s
			}
			return map[string]any{"app": row}, nil
		case "app_get":
			return map[string]any{"app": row}, nil
		case "app_set_upstream_auth":
			return map[string]any{}, nil
		case "app_delete":
			return map[string]any{}, nil
		default:
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureResource(ctx, res, client); err != nil {
		return out, err
	}

	createTop := map[string]rawVal{
		"slug": "papp1", "name": "Proxy One", "description": "proxies things",
		"archived": false, "log_bodies": true,
		"redact":         map[string]rawVal{"^set_.*$": []rawVal{"password"}},
		"redact_results": map[string]rawVal{"^err_.*$": []rawVal{"stack"}},
		"endpoint":       "https://upstream.example/mcp", "auth": "headers", "forward_identity": true,
		"roles": map[string]rawVal{
			"reader": map[string]rawVal{
				"tools": []rawVal{"get_.*"}, "prompts": []rawVal{"summarize_.*"}, "resources": []rawVal{"docs://.*"},
			},
		},
		"capabilities":    []rawVal{"tools", "prompts"},
		"headers_wo":      map[string]rawVal{"Authorization": "Bearer up"},
		"headers_version": 1,
	}
	at := len(f.calls)
	createResp := &resource.CreateResponse{State: nullResourceState(sch, ty)}
	res.Create(ctx, resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: buildValue(ty, createTop)},
		Config: tfsdk.Config{Schema: sch, Raw: buildValue(ty, createTop)},
	}, createResp)
	if createResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_proxy_app Create: %v", createResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_proxy_app/Create", WantOps: []string{"app_create", "app_set_upstream_auth"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	readState := tfsdk.State{Schema: sch, Raw: buildValue(ty, map[string]rawVal{"slug": "papp1"})}
	readResp := &resource.ReadResponse{State: readState}
	res.Read(ctx, resource.ReadRequest{State: readState}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_proxy_app Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_proxy_app/Read", WantOps: []string{"app_get"}, GotOps: opsSince(f, at)})

	priorState := tfsdk.State{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"slug": "papp1", "name": "Proxy One", "description": "proxies things",
		"archived": false, "log_bodies": true,
		"redact":         map[string]rawVal{"^set_.*$": []rawVal{"password"}},
		"redact_results": map[string]rawVal{"^err_.*$": []rawVal{"stack"}},
		"endpoint":       "https://upstream.example/mcp", "auth": "headers", "forward_identity": true,
		"roles": map[string]rawVal{
			"reader": map[string]rawVal{"tools": []rawVal{"get_.*"}, "prompts": []rawVal{"summarize_.*"}, "resources": []rawVal{"docs://.*"}},
		},
		"capabilities":            []rawVal{"tools", "prompts"},
		"headers_version":         1,
		"headers_applied_version": 1,
	})}
	updateTop := map[string]rawVal{
		"slug": "papp1", "name": "Proxy One", "description": "proxies more things",
		"archived": false, "log_bodies": false,
		"redact":         map[string]rawVal{"^auth_.*$": []rawVal{"token"}},
		"redact_results": map[string]rawVal{"^fail_.*$": []rawVal{"trace"}},
		"endpoint":       "https://upstream.example/v2", "auth": "headers", "forward_identity": false,
		"roles": map[string]rawVal{
			"reader": map[string]rawVal{"tools": []rawVal{"get_.*", "list_.*"}},
		},
		"capabilities":            []rawVal{"tools"},
		"headers_wo":              map[string]rawVal{"Authorization": "Bearer up2"},
		"headers_version":         2,
		"headers_applied_version": unknownVal,
	}
	at = len(f.calls)
	updateResp := &resource.UpdateResponse{State: nullResourceState(sch, ty)}
	res.Update(ctx, resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: buildValue(ty, updateTop)},
		State:  priorState,
		Config: tfsdk.Config{Schema: sch, Raw: buildValue(ty, updateTop)},
	}, updateResp)
	if updateResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_proxy_app Update: %v", updateResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_proxy_app/Update", WantOps: []string{"app_update", "app_set_upstream_auth"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	deleteResp := &resource.DeleteResponse{State: readState}
	res.Delete(ctx, resource.DeleteRequest{State: readState}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_proxy_app Delete: %v", deleteResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_proxy_app/Delete", WantOps: []string{"app_delete"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}

// driveGrant drives pmcp_grant through Create, Read, Update, Delete against a proxy app that
// declares the roles this scenario grants, so §22.4's undeclared-role check (an error on a
// proxy app) never fires.
func driveGrant(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	res := provider.NewGrantResource()
	sch := resourceSchemaOf(ctx, res)
	ty := objectTypeOf(ctx, sch.Attributes)

	appRow := pmcp.AppRow{Slug: "papp1", Kind: "proxy", Roles: map[string]pmcp.RoleFamilies{
		"reader": {Tools: []string{"get_.*"}},
		"writer": {Tools: []string{"set_.*"}},
	}}
	agentGrants := map[string][]string{"papp1": {"reader", "writer:approval"}}

	f := &fakeHub{contract: contract}
	f.reply = func(op string, args map[string]any) (any, *rpcErrorBody) {
		switch op {
		case "app_get":
			return map[string]any{"app": appRow}, nil
		case "grant_set":
			roles := strSlice(args["roles"])
			agentGrants[str(args, "app")] = roles
			return pmcp.GrantSetResult{Agent: str(args, "agent"), App: str(args, "app"), Roles: roles}, nil
		case "agent_list":
			return map[string]any{"agents": []pmcp.AgentRow{{Slug: "agentx", Grants: agentGrants}}}, nil
		default:
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureResource(ctx, res, client); err != nil {
		return out, err
	}

	at := len(f.calls)
	createPlan := tfsdk.Plan{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"agent": "agentx", "app": "papp1", "allow": []rawVal{"reader"}, "approval": []rawVal{"writer"},
	})}
	createResp := &resource.CreateResponse{State: nullResourceState(sch, ty)}
	res.Create(ctx, resource.CreateRequest{Plan: createPlan}, createResp)
	if createResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_grant Create: %v", createResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_grant/Create", WantOps: []string{"app_get", "grant_set"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	readState := tfsdk.State{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"agent": "agentx", "app": "papp1", "allow": []rawVal{"reader"}, "approval": []rawVal{"writer"},
	})}
	readResp := &resource.ReadResponse{State: readState}
	res.Read(ctx, resource.ReadRequest{State: readState}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_grant Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_grant/Read", WantOps: []string{"agent_list"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	updatePlan := tfsdk.Plan{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"agent": "agentx", "app": "papp1", "allow": []rawVal{"reader", "all"}, "approval": []rawVal{},
	})}
	updateResp := &resource.UpdateResponse{State: nullResourceState(sch, ty)}
	res.Update(ctx, resource.UpdateRequest{Plan: updatePlan}, updateResp)
	if updateResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_grant Update: %v", updateResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_grant/Update", WantOps: []string{"app_get", "grant_set"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	deleteResp := &resource.DeleteResponse{State: readState}
	res.Delete(ctx, resource.DeleteRequest{State: readState}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_grant Delete: %v", deleteResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_grant/Delete", WantOps: []string{"grant_set"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}

// driveToken drives pmcp_token through Create, Read, Update (no RPC — every configurable
// attribute is RequiresReplace, §22.2), Delete.
func driveToken(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	res := provider.NewTokenResource()
	sch := resourceSchemaOf(ctx, res)
	ty := objectTypeOf(ctx, sch.Attributes)

	var issued pmcp.TokenRow
	f := &fakeHub{contract: contract}
	f.reply = func(op string, args map[string]any) (any, *rpcErrorBody) {
		switch op {
		case "token_issue":
			issued = pmcp.TokenRow{ID: "tok1", Kind: str(args, "kind"), RefSlug: str(args, "slug"), Prefix: "pmcp_agt_abc", CreatedAt: 1700000000000}
			return pmcp.IssuedToken{ID: issued.ID, Token: "pmcp_agt_abcplaintext", Prefix: issued.Prefix, Kind: issued.Kind, Slug: issued.RefSlug}, nil
		case "token_list":
			return map[string]any{"tokens": []pmcp.TokenRow{issued}}, nil
		case "token_revoke":
			return map[string]any{"id": args["id"]}, nil
		default:
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureResource(ctx, res, client); err != nil {
		return out, err
	}

	at := len(f.calls)
	createPlan := tfsdk.Plan{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"agent": "agentx", "expires_in": "90000", "rotation": 1,
	})}
	createResp := &resource.CreateResponse{State: nullResourceState(sch, ty)}
	res.Create(ctx, resource.CreateRequest{Plan: createPlan}, createResp)
	if createResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_token Create: %v", createResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_token/Create", WantOps: []string{"token_issue", "token_list"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	readState := tfsdk.State{Schema: sch, Raw: buildValue(ty, map[string]rawVal{
		"agent": "agentx", "id": "tok1", "token": "pmcp_agt_abcplaintext",
	})}
	readResp := &resource.ReadResponse{State: readState}
	res.Read(ctx, resource.ReadRequest{State: readState}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_token Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_token/Read", WantOps: []string{"token_list"}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	updateResp := &resource.UpdateResponse{State: nullResourceState(sch, ty)}
	res.Update(ctx, resource.UpdateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: readState.Raw}}, updateResp)
	if updateResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_token Update: %v", updateResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_token/Update", WantOps: []string{}, GotOps: opsSince(f, at)})

	at = len(f.calls)
	deleteResp := &resource.DeleteResponse{State: readState}
	res.Delete(ctx, resource.DeleteRequest{State: readState}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		return out, fmt.Errorf("pmcp_token Delete: %v", deleteResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "pmcp_token/Delete", WantOps: []string{"token_revoke"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}

func driveAppDataSource(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	ds := provider.NewAppDataSource()
	sch := dataSourceSchemaOf(ctx, ds)
	ty := objectTypeOf(ctx, sch.Attributes)

	f := &fakeHub{contract: contract}
	f.reply = func(op string, args map[string]any) (any, *rpcErrorBody) {
		if op != "app_get" {
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
		return map[string]any{"app": pmcp.AppRow{Slug: str(args, "slug"), Kind: "proxy", Name: str(args, "slug")}}, nil
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureDataSource(ctx, ds, client); err != nil {
		return out, err
	}

	at := len(f.calls)
	readResp := &datasource.ReadResponse{State: nullDataSourceState(sch, ty)}
	ds.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: buildValue(ty, map[string]rawVal{"slug": "papp1"})}}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("data.pmcp_app Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "data.pmcp_app/Read", WantOps: []string{"app_get"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}

func driveAgentDataSource(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	ds := provider.NewAgentDataSource()
	sch := dataSourceSchemaOf(ctx, ds)
	ty := objectTypeOf(ctx, sch.Attributes)

	f := &fakeHub{contract: contract}
	f.reply = func(op string, _ map[string]any) (any, *rpcErrorBody) {
		if op != "agent_list" {
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
		return map[string]any{"agents": []pmcp.AgentRow{{Slug: "agentx", Name: "Agent X", Grants: map[string][]string{"papp1": {"reader"}}}}}, nil
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureDataSource(ctx, ds, client); err != nil {
		return out, err
	}

	at := len(f.calls)
	readResp := &datasource.ReadResponse{State: nullDataSourceState(sch, ty)}
	ds.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: buildValue(ty, map[string]rawVal{"slug": "agentx"})}}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("data.pmcp_agent Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "data.pmcp_agent/Read", WantOps: []string{"agent_list"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}

func driveTokensDataSource(ctx context.Context, contract *Contract) (driverResult, error) {
	var out driverResult
	ds := provider.NewTokensDataSource()
	sch := dataSourceSchemaOf(ctx, ds)
	ty := objectTypeOf(ctx, sch.Attributes)

	f := &fakeHub{contract: contract}
	f.reply = func(op string, _ map[string]any) (any, *rpcErrorBody) {
		if op != "token_list" {
			return nil, &rpcErrorBody{Code: -32601, Message: "unexpected op " + op}
		}
		return map[string]any{"tokens": []pmcp.TokenRow{{ID: "tok1", Kind: "agent", RefSlug: "agentx", Prefix: "pmcp_agt_abc", CreatedAt: 1700000000000}}}, nil
	}
	client, closeFn, err := newTestClient(ctx, f)
	if err != nil {
		return out, err
	}
	defer closeFn()
	if err := configureDataSource(ctx, ds, client); err != nil {
		return out, err
	}

	at := len(f.calls)
	readResp := &datasource.ReadResponse{State: nullDataSourceState(sch, ty)}
	ds.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: buildValue(ty, map[string]rawVal{"agent": "agentx"})}}, readResp)
	if readResp.Diagnostics.HasError() {
		return out, fmt.Errorf("data.pmcp_tokens Read: %v", readResp.Diagnostics)
	}
	out.Expectations = append(out.Expectations, pathExpectation{Path: "data.pmcp_tokens/Read", WantOps: []string{"token_list"}, GotOps: opsSince(f, at)})

	return finish(&out, f), nil
}
