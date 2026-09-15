package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// ptRPCError is one JSON-RPC error body for this file's fake hub. Named with a "pt" (this
// task's owner, ProviderTokenAndData) prefix deliberately: several sibling _test.go files in
// this same package each need their own httptest fake, and Go's single-package namespace would
// collide on a plain "fake"/"rpcErrorBody" if two of us picked the same name.
type ptRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ptFakeHub starts an httptest server standing in for the hub's admin surface, in the style of
// internal/pmcp/client_test.go's fake, and returns a configured *pmcp.Client plus the reply
// function's own op argument for callers that want to assert on it.
func ptFakeHub(t *testing.T, reply func(op string, args map[string]any) (any, *ptRPCError)) *pmcp.Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/whoami", func(w http.ResponseWriter, r *http.Request) {
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
			t.Fatalf("admin request was not JSON: %v", err)
		}
		value, rpcErr := reply(req.Params.Name, req.Params.Arguments)
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := pmcp.New(context.Background(), srv.URL, "pmcp_adm_test")
	if err != nil {
		t.Fatalf("pmcp.New: %v", err)
	}
	return client
}

// ptTokenResource builds a configured tokenResource plus its schema, ready for a Read/Create/
// Delete call built by hand — the unit-test substitute for the full acceptance-test harness,
// which needs a real terraform/tofu binary this repo's `go test` does not have.
func ptTokenResource(t *testing.T, client *pmcp.Client) (*tokenResource, rschema.Schema) {
	t.Helper()
	r := &tokenResource{client: client}
	var schemaResp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &schemaResp)
	return r, schemaResp.Schema
}

func TestTokenResourceReadPreservesPlaintext(t *testing.T) {
	ctx := context.Background()
	client := ptFakeHub(t, func(op string, args map[string]any) (any, *ptRPCError) {
		if op != "token_list" {
			t.Fatalf("Read should only call token_list, got %q", op)
		}
		return map[string]any{"tokens": []map[string]any{
			{
				"id": "tok1", "kind": "agent", "refSlug": "bot", "prefix": "pmcp_agt_new",
				"createdAt": 1000, "expiresAt": nil, "lastUsedAt": nil, "revokedAt": nil,
			},
		}}, nil
	})
	r, sch := ptTokenResource(t, client)

	prior := tokenResourceModel{
		Agent: types.StringValue("bot"), App: types.StringNull(),
		ExpiresIn: types.StringNull(), Rotation: types.Int64Null(),
		Token: types.StringValue("plaintext-secret-only-known-once"),
		ID:    types.StringValue("tok1"), Prefix: types.StringValue("pmcp_agt_old"),
		CreatedAt: types.Int64Value(1), ExpiresAt: types.Int64Null(), RevokedAt: types.Int64Null(),
	}
	state := tfsdk.State{Schema: sch}
	if diags := state.Set(ctx, &prior); diags.HasError() {
		t.Fatalf("seeding state: %v", diags)
	}

	resp := &resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	var got tokenResourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("reading back state: %v", diags)
	}
	if got.Token.ValueString() != "plaintext-secret-only-known-once" {
		t.Errorf("token = %q, want the prior plaintext preserved verbatim", got.Token.ValueString())
	}
	// The rest of the metadata must still refresh — otherwise "preserved" would really mean
	// "the whole resource stopped reading".
	if got.Prefix.ValueString() != "pmcp_agt_new" {
		t.Errorf("prefix = %q, want the refreshed value pmcp_agt_new", got.Prefix.ValueString())
	}
}

func TestTokenResourceReadRevokedIsNotDrift(t *testing.T) {
	ctx := context.Background()
	client := ptFakeHub(t, func(op string, args map[string]any) (any, *ptRPCError) {
		return map[string]any{"tokens": []map[string]any{
			{
				"id": "tok2", "kind": "app", "refSlug": "svc", "prefix": "pmcp_app_x",
				"createdAt": 5, "expiresAt": nil, "lastUsedAt": nil, "revokedAt": 999,
			},
		}}, nil
	})
	r, sch := ptTokenResource(t, client)

	prior := tokenResourceModel{
		Agent: types.StringNull(), App: types.StringValue("svc"),
		ExpiresIn: types.StringNull(), Rotation: types.Int64Null(),
		Token: types.StringNull(), ID: types.StringValue("tok2"), Prefix: types.StringValue("pmcp_app_x"),
		CreatedAt: types.Int64Value(5), ExpiresAt: types.Int64Null(), RevokedAt: types.Int64Null(),
	}
	state := tfsdk.State{Schema: sch}
	if diags := state.Set(ctx, &prior); diags.HasError() {
		t.Fatalf("seeding state: %v", diags)
	}

	resp := &resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("a revoked-but-present token must not be removed from state")
	}

	var got tokenResourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("reading back state: %v", diags)
	}
	if got.RevokedAt.IsNull() || got.RevokedAt.ValueInt64() != 999 {
		t.Errorf("revoked_at = %v, want 999 surfaced as a computed attribute", got.RevokedAt)
	}
}
