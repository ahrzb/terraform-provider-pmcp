package coverage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"
)

// recordedCall is one admin op invocation captured during a scenario, exactly as the resource
// sent it. The op name feeds assertion 1 (per-path) and 3 (totality); the argument key set feeds
// assertion 2 (field coverage) — see oracle.go.
type recordedCall struct {
	Op   string
	Args map[string]any
}

// rpcErrorBody is one JSON-RPC error, in the shape internal/provider's own fakeHub uses.
type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// fakeHub stands in for the hub's admin surface, in the style of internal/provider's own
// fakeHub and internal/pmcp's client_test fake — this package cannot import either (both are
// unexported test-only types in packages this package must not edit), so it is its own copy,
// with one addition neither of those needs: it validates every call's arguments against the
// contract's inputSchemas as it records them (§22.5, "The fake validates requests against
// admin-ops.json's inputSchemas, so it cannot drift into accepting what the hub's parseInput
// would reject").
type fakeHub struct {
	contract *Contract
	reply    func(op string, args map[string]any) (any, *rpcErrorBody)

	calls          []recordedCall
	validationErrs []error
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
			f.validationErrs = append(f.validationErrs, fmt.Errorf("admin request was not JSON: %w", err))
			return
		}
		op, args := req.Params.Name, req.Params.Arguments
		if args == nil {
			args = map[string]any{}
		}
		f.calls = append(f.calls, recordedCall{Op: op, Args: args})

		if node, known := f.contract.InputSchemas[op]; !known {
			f.validationErrs = append(f.validationErrs, fmt.Errorf("%s: called by the provider but absent from the contract's inputSchemas entirely", op))
		} else if err := validateArgs(op, node, args); err != nil {
			f.validationErrs = append(f.validationErrs, err)
		}

		value, rpcErr := f.reply(op, args)
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

// newTestClient starts f's server and builds a *pmcp.Client against it, in the style of
// internal/provider's testClient helper. The caller must call the returned close func.
func newTestClient(ctx context.Context, f *fakeHub) (*pmcp.Client, func(), error) {
	srv := f.server()
	client, err := pmcp.New(ctx, srv.URL, "pmcp_adm_test")
	if err != nil {
		srv.Close()
		return nil, nil, fmt.Errorf("pmcp.New: %w", err)
	}
	return client, srv.Close, nil
}
