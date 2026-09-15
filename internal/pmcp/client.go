// Package pmcp is the hub's admin transport: a thin net/http client over the builtin `pmcp`
// MCP app, which is the only administrative surface the hub exposes. There is no REST admin
// API, so every operation here is a JSON-RPC `tools/call` against one endpoint.
package pmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Timeout matches the hub's forwarded-call budget. Calls are at-most-once and carry no
// idempotency key, so a request that has been sent is never retried — only dial failures are.
const Timeout = 30 * time.Second

// ErrNotFound is returned when an object the caller named does not exist. Resource Read paths
// turn this into state removal rather than an error, and Delete paths treat it as success.
var ErrNotFound = errors.New("pmcp: not found")

// RPCError is a JSON-RPC error returned by the hub. Code is the wire code; the hub pins six of
// them and they are part of the contract, unlike the messages.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("pmcp: rpc error %d: %s", e.Code, e.Message)
}

// MethodNotFound reports whether the hub rejected the operation as unknown, which in practice
// means the hub is older than this provider.
func (e *RPCError) MethodNotFound() bool { return e.Code == -32601 }

// notFoundMessage is the hub's absence sentence. The hub does NOT give absence its own wire
// code: `absent()` in server/src/admin.ts builds `invalid("no such <family> in this
// namespace")`, so it arrives as -32602 — the same code as a malformed slug or a bad endpoint.
// Matching the message is therefore the only way to tell "gone" from "you asked wrongly", and
// the distinction is load-bearing: the first means RemoveResource, the second must surface as an
// error. Kept as a prefix match on the stable part of the sentence, since -32602 messages can
// arrive joined with `; ` when a call produces several violations.
const notFoundMessage = "no such "

// IsNotFound reports whether err is the hub saying the named object does not exist.
func IsNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32602 {
		return false
	}
	return strings.HasPrefix(rpc.Message, notFoundMessage) &&
		strings.Contains(rpc.Message, "in this namespace")
}

// HTTPError is a transport-level failure: the request never reached the MCP dispatcher.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("pmcp: http %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// Client talks to one hub as one owner. It is safe for concurrent use: the plugin framework
// serves RPCs on their own goroutines against a single provider instance, and nothing here is
// cached — the hub has no per-plan snapshot a client could safely memoize.
type Client struct {
	http   *http.Client
	origin string
	token  string

	// Namespace is the owner segment resolved at configure time. It is never derived from the
	// token's text; the hub is asked.
	Namespace string
	// Principal is whoami's display spelling for the credential in use.
	Principal string

	adminPath string
	seq       atomic.Int64
}

type whoamiResponse struct {
	Principal string `json:"principal"`
	Namespace string `json:"namespace"`
}

// New builds a client and resolves the owner namespace through GET /api/whoami. That round trip
// is mandatory rather than an optimisation: the admin path is /<namespace>/mcp/pmcp, and the
// namespace cannot be guessed from the credential.
func New(ctx context.Context, origin, token string) (*Client, error) {
	c := &Client{
		http:   &http.Client{Timeout: Timeout},
		origin: strings.TrimRight(origin, "/"),
		token:  token,
	}
	if err := c.resolveNamespace(ctx); err != nil {
		return nil, err
	}
	c.adminPath = "/" + c.Namespace + "/mcp/pmcp"
	return c, nil
}

func (c *Client) resolveNamespace(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+"/api/whoami", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusUnauthorized {
		// The hub deliberately does not distinguish expired from revoked from malformed — doing
		// so would tell a holder that a string was once valid — so this message names both.
		return errors.New("pmcp: credential rejected (401): the admin token is expired, revoked, or not an admin token")
	}
	if res.StatusCode != http.StatusOK {
		return readHTTPError(res)
	}

	var body whoamiResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return fmt.Errorf("pmcp: whoami response was not JSON: %w", err)
	}
	if body.Namespace == "" {
		return errors.New("pmcp: whoami returned no namespace")
	}
	c.Namespace, c.Principal = body.Namespace, body.Principal
	return nil
}

type rpcRequest struct {
	JSONRPC string  `json:"jsonrpc"`
	ID      int64   `json:"id"`
	Method  string  `json:"method"`
	Params  rpcCall `json:"params"`
}

type rpcCall struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

type rpcResponse struct {
	Result *struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Admin invokes one admin operation and returns its structured result. `op` is a tool name such
// as "app_get"; `args` is marshalled as the tool arguments.
func (c *Client) Admin(ctx context.Context, op string, args any) (json.RawMessage, error) {
	if args == nil {
		args = struct{}{}
	}
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      c.seq.Add(1),
		Method:  "tools/call",
		Params:  rpcCall{Name: op, Arguments: args},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+c.adminPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, readHTTPError(res)
	}

	var body rpcResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("pmcp: %s response was not JSON-RPC: %w", op, err)
	}
	if body.Error != nil {
		return nil, &RPCError{Code: body.Error.Code, Message: body.Error.Message}
	}
	if body.Result == nil {
		return nil, fmt.Errorf("pmcp: %s returned neither result nor error", op)
	}
	return body.Result.StructuredContent, nil
}

func readHTTPError(res *http.Response) error {
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(res.Body)
	return &HTTPError{Status: res.StatusCode, Body: buf.String()}
}
