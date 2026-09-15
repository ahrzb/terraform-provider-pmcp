package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// fakeHub stands in for the hub's admin surface, in the style of internal/pmcp/client_test.go's
// fake. Shared across this package's resource tests rather than copied per file.
type fakeHub struct {
	t     *testing.T
	calls []fakeCall
	reply func(op string, args map[string]any) (any, *fakeRPCError)
}

type fakeCall struct {
	Op   string
	Args map[string]any
}

type fakeRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (f *fakeHub) server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"principal": "user:owner", "namespace": "owner"})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Fatalf("admin request was not JSON: %v", err)
		}
		f.calls = append(f.calls, fakeCall{Op: req.Params.Name, Args: req.Params.Arguments})

		value, rpcErr := f.reply(req.Params.Name, req.Params.Arguments)
		if rpcErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": rpcErr})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"structuredContent": value},
		})
	})

	return httptest.NewServer(mux)
}

// testClient builds a *pmcp.Client against a fake hub, failing the test on error.
func testClient(t *testing.T, f *fakeHub) *pmcp.Client {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)
	client, err := pmcp.New(context.Background(), srv.URL, "pmcp_adm_test")
	if err != nil {
		t.Fatalf("pmcp.New: %v", err)
	}
	return client
}

// resourceSchema returns res's schema, for building tfsdk.Plan/State in tests without going
// through Terraform Core's plan cycle (this repo has no terraform-plugin-testing rig; per §22.5
// the parity oracle is this httptest fake, not an acceptance-test harness).
func resourceSchema(t *testing.T, res resource.Resource) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	res.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	return resp.Schema
}

// planFor builds a tfsdk.Plan from a model value using res's schema.
func planFor(t *testing.T, res resource.Resource, model any) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: resourceSchema(t, res)}
	if diags := plan.Set(context.Background(), model); diags.HasError() {
		t.Fatalf("plan.Set: %v", diags)
	}
	return plan
}

// stateFor builds a tfsdk.State from a model value using res's schema.
func stateFor(t *testing.T, res resource.Resource, model any) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: resourceSchema(t, res)}
	if diags := state.Set(context.Background(), model); diags.HasError() {
		t.Fatalf("state.Set: %v", diags)
	}
	return state
}

// emptyState returns a zero-value tfsdk.State carrying res's schema, ready for a
// Create/Read/Update response to Set into — mirroring how the framework pre-populates
// CreateResponse.State/UpdateResponse.State from the request's schema before calling the
// resource. Raw must be a fully-null value of the schema's type, matching how the framework
// initializes state before Create/ImportState populate it and RemoveResource clears it —
// a zero tftypes.Value (no type at all) rejects SetAttribute.
func emptyState(t *testing.T, res resource.Resource) tfsdk.State {
	t.Helper()
	s := resourceSchema(t, res)
	ctx := context.Background()
	return tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
}

// configure drives a resource's Configure method against client, failing the test on error.
func configure(t *testing.T, res resource.Resource, client *pmcp.Client) {
	t.Helper()
	configurable, ok := res.(resource.ResourceWithConfigure)
	if !ok {
		t.Fatalf("%T does not implement ResourceWithConfigure", res)
	}
	var resp resource.ConfigureResponse
	configurable.Configure(context.Background(), resource.ConfigureRequest{ProviderData: client}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure: %v", resp.Diagnostics)
	}
}

func TestAgentCreateOmitsUnconfiguredOptionalFields(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "agent_create" {
			t.Fatalf("expected agent_create, got %q", op)
		}
		return map[string]any{"agent": pmcp.AgentRow{Slug: "bot", Name: "bot", Description: ""}}, nil
	}}
	client := testClient(t, f)

	res := NewAgentResource()
	configure(t, res, client)

	// Mirrors what Terraform Core would plan for an omitted optional+computed attribute at
	// first create: unknown, since there is no prior state to reuse.
	plan := planFor(t, res, &agentResourceModel{
		Slug:        types.StringValue("bot"),
		Name:        types.StringUnknown(),
		Description: types.StringUnknown(),
	})

	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	if len(f.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(f.calls))
	}
	args := f.calls[0].Args
	if _, ok := args["name"]; ok {
		t.Errorf("name should be omitted when unconfigured, got %v", args["name"])
	}
	if _, ok := args["description"]; ok {
		t.Errorf("description should be omitted when unconfigured, got %v", args["description"])
	}

	var state agentResourceModel
	if diags := createResp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if state.Name.ValueString() != "bot" || state.Description.ValueString() != "" {
		t.Errorf("state = %+v, want hub-defaulted name=bot description=\"\"", state)
	}
}

func TestAgentCreateSendsConfiguredOptionalFields(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		return map[string]any{"agent": pmcp.AgentRow{
			Slug: args["slug"].(string), Name: args["name"].(string), Description: args["description"].(string),
		}}, nil
	}}
	client := testClient(t, f)
	res := NewAgentResource()
	configure(t, res, client)

	plan := planFor(t, res, &agentResourceModel{
		Slug:        types.StringValue("bot"),
		Name:        types.StringValue("Bot Display Name"),
		Description: types.StringValue("does things"),
	})
	createResp := &resource.CreateResponse{State: emptyState(t, res)}
	res.Create(context.Background(), resource.CreateRequest{Plan: plan}, createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", createResp.Diagnostics)
	}

	args := f.calls[0].Args
	if args["name"] != "Bot Display Name" || args["description"] != "does things" {
		t.Errorf("args = %+v, want configured name/description sent through", args)
	}
}

func TestAgentReadRemovesResourceWhenAgentAbsent(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "agent_list" {
			t.Fatalf("expected agent_list, got %q", op)
		}
		return map[string]any{"agents": []pmcp.AgentRow{}}, nil
	}}
	client := testClient(t, f)
	res := NewAgentResource()
	configure(t, res, client)

	state := stateFor(t, res, &agentResourceModel{
		Slug:        types.StringValue("gone"),
		Name:        types.StringValue("gone"),
		Description: types.StringValue(""),
	})
	readResp := &resource.ReadResponse{State: state}
	res.Read(context.Background(), resource.ReadRequest{State: state}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Errorf("expected state removed (null) for an absent agent, got %v", readResp.State.Raw)
	}
}

// The hub's -32601 for a missing agent_update is not cosmetic: without it, correcting a
// display-name typo would require RequiresReplace and silently revoke every live consumer
// credential for the agent (§22.4). Update must therefore surface it as an actionable message
// naming the op, not a bare RPC error.
func TestAgentUpdateSurfacesMethodNotFoundAsActionableHubUpgradeError(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		return nil, &fakeRPCError{Code: -32601, Message: "method not found"}
	}}
	client := testClient(t, f)
	res := NewAgentResource()
	configure(t, res, client)

	plan := planFor(t, res, &agentResourceModel{
		Slug:        types.StringValue("bot"),
		Name:        types.StringValue("Renamed Bot"),
		Description: types.StringValue(""),
	})
	updateResp := &resource.UpdateResponse{State: emptyState(t, res)}
	res.Update(context.Background(), resource.UpdateRequest{Plan: plan}, updateResp)

	if !updateResp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic for a hub missing agent_update")
	}
	found := false
	for _, d := range updateResp.Diagnostics {
		if strings.Contains(d.Summary(), "agent_update") || strings.Contains(d.Detail(), "agent_update") {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics = %v, want one naming agent_update as the missing hub op", updateResp.Diagnostics)
	}
}

func TestAgentDeleteIsIdempotentWhenAgentAlreadyGone(t *testing.T) {
	f := &fakeHub{t: t, reply: func(op string, args map[string]any) (any, *fakeRPCError) {
		if op != "agent_delete" {
			t.Fatalf("expected agent_delete, got %q", op)
		}
		return nil, &fakeRPCError{Code: -32602, Message: `no such agent "gone" in this namespace`}
	}}
	client := testClient(t, f)
	res := NewAgentResource()
	configure(t, res, client)

	state := stateFor(t, res, &agentResourceModel{
		Slug:        types.StringValue("gone"),
		Name:        types.StringValue("gone"),
		Description: types.StringValue(""),
	})
	deleteResp := &resource.DeleteResponse{State: state}
	res.Delete(context.Background(), resource.DeleteRequest{State: state}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("Delete on an already-gone agent should succeed, got %v", deleteResp.Diagnostics)
	}
}
