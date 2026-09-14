package pmcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The fake stands in for the hub's admin surface. Once resources exist it also becomes the
// parity oracle: it records which admin op each CRUD path calls, which is the only witness that
// can see a mapping a schema dump cannot.
type fake struct {
	t         *testing.T
	namespace string
	calls     []recordedCall
	reply     func(op string) (any, *rpcErrorBody)
}

type recordedCall struct {
	Path string
	Op   string
	Args map[string]any
}

type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (f *fake) server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pmcp_adm_test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "Unauthorized")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"principal": "user:owner",
			"namespace": f.namespace,
		})
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
		if req.Method != "tools/call" {
			f.t.Fatalf("expected tools/call, got %q", req.Method)
		}
		f.calls = append(f.calls, recordedCall{Path: r.URL.Path, Op: req.Params.Name, Args: req.Params.Arguments})

		value, rpcErr := f.reply(req.Params.Name)
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

func TestNewResolvesNamespaceFromWhoami(t *testing.T) {
	f := &fake{t: t, namespace: "owner", reply: func(string) (any, *rpcErrorBody) { return map[string]any{}, nil }}
	srv := f.server()
	defer srv.Close()

	client, err := New(context.Background(), srv.URL, "pmcp_adm_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.Namespace != "owner" {
		t.Errorf("namespace = %q, want %q", client.Namespace, "owner")
	}

	// The admin path must be built from whoami's namespace, never from the token's text.
	if _, err := client.Admin(context.Background(), "app_list", nil); err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got := f.calls[0].Path; got != "/owner/mcp/pmcp" {
		t.Errorf("admin path = %q, want %q", got, "/owner/mcp/pmcp")
	}
}

func TestNewRejectsBadCredentialWithBothCauses(t *testing.T) {
	f := &fake{t: t, namespace: "owner", reply: func(string) (any, *rpcErrorBody) { return nil, nil }}
	srv := f.server()
	defer srv.Close()

	_, err := New(context.Background(), srv.URL, "pmcp_adm_wrong")
	if err == nil {
		t.Fatal("expected an error for a rejected credential")
	}
	// The hub cannot distinguish expired from revoked from malformed, so the message must not
	// claim to. Naming only one cause would send an operator down the wrong path.
	for _, want := range []string{"expired", "revoked", "not an admin token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestAdminSurfacesRPCErrorsWithTheirWireCode(t *testing.T) {
	f := &fake{t: t, namespace: "owner", reply: func(op string) (any, *rpcErrorBody) {
		return nil, &rpcErrorBody{Code: -32601, Message: "method not found"}
	}}
	srv := f.server()
	defer srv.Close()

	client, err := New(context.Background(), srv.URL, "pmcp_adm_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.Admin(context.Background(), "agent_update", nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T (%v)", err, err)
	}
	// -32601 is how a hub older than this provider reports an op it does not have, which is a
	// different remedy from every other failure: upgrade the hub.
	if !rpcErr.MethodNotFound() {
		t.Errorf("code %d should be recognised as method-not-found", rpcErr.Code)
	}
}

func TestAdminTrimsTrailingSlashFromOrigin(t *testing.T) {
	f := &fake{t: t, namespace: "owner", reply: func(string) (any, *rpcErrorBody) { return map[string]any{}, nil }}
	srv := f.server()
	defer srv.Close()

	client, err := New(context.Background(), srv.URL+"/", "pmcp_adm_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Admin(context.Background(), "app_list", nil); err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got := f.calls[0].Path; got != "/owner/mcp/pmcp" {
		t.Errorf("admin path = %q, want %q (a doubled slash would 404)", got, "/owner/mcp/pmcp")
	}
}
