package pmcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// The wire's two spellings are not a mistake to normalize away: admin op *arguments* are
// snake_case (`log_bodies`, `forward_identity`) because they mirror §9's YAML, while *rows* are
// camelCase (`logBodies`, `forwardIdentity`) because they mirror the TypeScript that builds them.
// Every struct below tags both sides explicitly rather than trusting a convention that does not
// hold across the boundary.

// RoleFamilies is one role's patterns, per family. The wire accepts either a bare pattern list
// or this object on the way in; the provider only ever SENDS the object. On the way out,
// `app_get`'s canonical rendering (§20.3) uses whichever is shorter to read — a bare list for a
// tools-only role, the object otherwise — so both shapes must decode, which is what the custom
// UnmarshalJSON below is for.
type RoleFamilies struct {
	Tools     []string `json:"tools,omitempty"`
	Prompts   []string `json:"prompts,omitempty"`
	Resources []string `json:"resources,omitempty"`
}

// UnmarshalJSON accepts both of the wire's read shapes. Without this, decoding the common
// case — any role that grants tools alone, which the hub's canonical rendering always emits as
// a bare list — would fail outright: encoding/json cannot unmarshal a JSON array into a struct.
func (r *RoleFamilies) UnmarshalJSON(data []byte) error {
	var bare []string
	if err := json.Unmarshal(data, &bare); err == nil {
		r.Tools, r.Prompts, r.Resources = bare, nil, nil
		return nil
	}
	type shape RoleFamilies
	var obj shape
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*r = RoleFamilies(obj)
	return nil
}

// AppRow is `app_get`/`app_list`'s row. Proxy-only fields are pointers or nil-able so that
// "absent" survives: `Capabilities == nil` means the owner never declared any, which §20.2 reads
// as tools-only, and is NOT the same as an empty list.
type AppRow struct {
	Slug          string                  `json:"slug"`
	Kind          string                  `json:"kind"` // tunnel | proxy | builtin
	Name          string                  `json:"name"`
	Description   string                  `json:"description"`
	Archived      bool                    `json:"archived"`
	LogBodies     bool                    `json:"logBodies"`
	Roles         map[string]RoleFamilies `json:"roles"`
	Redact        map[string][]string     `json:"redact"`
	RedactResults map[string][]string     `json:"redactResults"`
	Builtin       bool                    `json:"builtin"`
	CreatedAt     int64                   `json:"createdAt"`

	// Proxy only.
	Endpoint        string    `json:"endpoint"`
	Auth            string    `json:"auth"` // headers | oauth
	ForwardIdentity bool      `json:"forwardIdentity"`
	Capabilities    *[]string `json:"capabilities"`
}

// AgentRow is `agent_list`'s row. Grants are the wire's own `role` / `role:approval` strings,
// keyed by app slug; splitting them into two sets is the provider's job, not the client's.
type AgentRow struct {
	Slug        string              `json:"slug"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	CreatedAt   int64               `json:"createdAt"`
	Grants      map[string][]string `json:"grants"`
}

// TokenRow is `token_list`'s row. Every timestamp is epoch ms and nullable except CreatedAt.
type TokenRow struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"` // app | agent
	RefSlug    string `json:"refSlug"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"createdAt"`
	ExpiresAt  *int64 `json:"expiresAt"`
	LastUsedAt *int64 `json:"lastUsedAt"`
	RevokedAt  *int64 `json:"revokedAt"`
}

// IssuedToken is `token_issue`'s result. Token is the only place the plaintext ever appears.
type IssuedToken struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Prefix    string `json:"prefix"`
	Kind      string `json:"kind"`
	Slug      string `json:"slug"`
	ExpiresAt *int64 `json:"expiresAt"`
}

// call runs one op and unmarshals the field the op wraps its payload in.
func call[T any](ctx context.Context, c *Client, op string, args any, field string) (T, error) {
	var zero T
	raw, err := c.Admin(ctx, op, args)
	if err != nil {
		return zero, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return zero, fmt.Errorf("pmcp: %s result was not an object: %w", op, err)
	}
	body, ok := envelope[field]
	if !ok {
		return zero, fmt.Errorf("pmcp: %s result carried no %q", op, field)
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return zero, fmt.Errorf("pmcp: %s %q did not decode: %w", op, field, err)
	}
	return out, nil
}

// AppGet reads one app. A missing app arrives as -32602 with the hub's absence sentence, which
// IsNotFound recognizes; callers turn that into state removal.
func (c *Client) AppGet(ctx context.Context, slug string) (AppRow, error) {
	return call[AppRow](ctx, c, "app_get", map[string]string{"slug": slug}, "app")
}

// AgentList reads every agent with its grants inline. There is no agent_get: agents and grants
// both read through this one op, and the builtin `pmcp` app appears only as a grant key.
func (c *Client) AgentList(ctx context.Context) ([]AgentRow, error) {
	return call[[]AgentRow](ctx, c, "agent_list", nil, "agents")
}

// Agent finds one agent by slug, reporting ErrNotFound rather than a zero row.
func (c *Client) Agent(ctx context.Context, slug string) (AgentRow, error) {
	agents, err := c.AgentList(ctx)
	if err != nil {
		return AgentRow{}, err
	}
	for _, a := range agents {
		if a.Slug == slug {
			return a, nil
		}
	}
	return AgentRow{}, ErrNotFound
}

// TokenList reads token metadata. The filters are the op's own; an empty filter lists all.
func (c *Client) TokenList(ctx context.Context, args any) ([]TokenRow, error) {
	return call[[]TokenRow](ctx, c, "token_list", args, "tokens")
}

// Token finds one token row by id. Revoked and expired rows are still present — only a row
// absent from the list means gone.
func (c *Client) Token(ctx context.Context, id string) (TokenRow, error) {
	rows, err := c.TokenList(ctx, nil)
	if err != nil {
		return TokenRow{}, err
	}
	for _, t := range rows {
		if t.ID == id {
			return t, nil
		}
	}
	return TokenRow{}, ErrNotFound
}

// TokenIssue mints a credential. The plaintext in the result is the only copy that will ever
// exist; the hub keeps a SHA-256. Like grant_set, the result sits at the top level —
// {id, token, prefix, kind, slug, expiresAt} — rather than wrapped under one field, so it does
// not go through call[T]'s single-field envelope.
func (c *Client) TokenIssue(ctx context.Context, args any) (IssuedToken, error) {
	raw, err := c.Admin(ctx, "token_issue", args)
	if err != nil {
		return IssuedToken{}, err
	}
	var out IssuedToken
	if err := json.Unmarshal(raw, &out); err != nil {
		return IssuedToken{}, fmt.Errorf("pmcp: token_issue result did not decode: %w", err)
	}
	return out, nil
}

// TokenRevoke revokes one credential by id. It is idempotent only in the sense the hub gives
// it: revoking an already-revoked-but-present row succeeds again, but an id whose row no longer
// exists (the referenced agent or app was deleted, cascading its tokens) answers absent, which
// IsNotFound recognizes — callers wanting an idempotent Delete check that themselves. The result
// is the unwrapped `{id}`, same shape as token_issue and grant_set.
func (c *Client) TokenRevoke(ctx context.Context, id string) error {
	raw, err := c.Admin(ctx, "token_revoke", map[string]string{"id": id})
	if err != nil {
		return err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("pmcp: token_revoke result did not decode: %w", err)
	}
	return nil
}

// AgentCreate creates an agent. args mirrors agent_create's wire fields: `slug` required,
// `name`/`description` optional. Build args with only the keys the caller actually wants sent —
// an omitted key takes the hub's own default (slug for name, "" for description) — rather than
// sending an empty string, which would ask for that default explicitly rather than deferring to it.
func (c *Client) AgentCreate(ctx context.Context, args any) (AgentRow, error) {
	return call[AgentRow](ctx, c, "agent_create", args, "agent")
}

// AgentUpdate updates an agent's name/description. This is §22.4's hub prerequisite: it is what
// lets pmcp_agent require replacement on nothing but `slug`. A hub predating it answers -32601,
// which callers must surface as an actionable error naming the missing op rather than a bare
// method-not-found.
func (c *Client) AgentUpdate(ctx context.Context, args any) (AgentRow, error) {
	return call[AgentRow](ctx, c, "agent_update", args, "agent")
}

// AgentDelete deletes an agent, cascading its grants and tokens. The hub reports a missing agent
// as ErrNotFound (via IsNotFound); Delete paths treat that as success rather than an error.
func (c *Client) AgentDelete(ctx context.Context, slug string) error {
	_, err := c.Admin(ctx, "agent_delete", map[string]string{"slug": slug})
	return err
}

// GrantSetResult is grant_set's response. Unlike every other op in this file it is not wrapped
// under one field — {agent, app, roles, warnings} sits at the top level — so it does not go
// through call[T]'s single-field envelope.
type GrantSetResult struct {
	Agent    string   `json:"agent"`
	App      string   `json:"app"`
	Roles    []string `json:"roles"`
	Warnings []string `json:"warnings"`
}

// GrantSet replaces the full grant set for one (agent, app) pair. An empty roles list revokes
// everything — the shape pmcp_grant's Delete uses. Warnings are the hub's own undeclared-role
// notices for tunneled apps (registry.setGrants); they are distinct from and additional to the
// provider's own pre-check, which runs first and can attribute a diagnostic to a schema path.
func (c *Client) GrantSet(ctx context.Context, agentSlug, appSlug string, roles []string) (GrantSetResult, error) {
	raw, err := c.Admin(ctx, "grant_set", map[string]any{
		"agent": agentSlug,
		"app":   appSlug,
		"roles": roles,
	})
	if err != nil {
		return GrantSetResult{}, err
	}
	var out GrantSetResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return GrantSetResult{}, fmt.Errorf("pmcp: grant_set result did not decode: %w", err)
	}
	return out, nil
}

// AppCreate creates an app. args mirrors app_create's wire fields: `slug` and `kind` required,
// everything else — including every proxy-only field — optional and hub-defaulted when omitted.
// Building args is the caller's job (as AgentCreate's args), because which fields exist depends
// on tunnel vs proxy and on which are pointer-wrapped to distinguish "leave alone" (nil) from
// "declare explicitly empty" (a non-nil pointer to an empty collection) for the collection-typed
// ones — a distinction plain Go zero values cannot carry.
func (c *Client) AppCreate(ctx context.Context, args any) (AppRow, error) {
	return call[AppRow](ctx, c, "app_create", args, "app")
}

// AppUpdate patches an app: app_create's fields minus `kind`, which is immutable. An omitted
// field means unchanged; flipping `auth` is accepted but wipes any stored upstream credential
// envelope in the same write, which is why the headers convergence machinery treats an auth
// flip as also clearing `headers_applied_version`.
func (c *Client) AppUpdate(ctx context.Context, args any) (AppRow, error) {
	return call[AppRow](ctx, c, "app_update", args, "app")
}

// AppArchive hides an app from consumers, retaining everything. Reversible via AppUnarchive.
func (c *Client) AppArchive(ctx context.Context, slug string) error {
	_, err := c.Admin(ctx, "app_archive", map[string]string{"slug": slug})
	return err
}

// AppUnarchive makes an archived app visible to consumers again.
func (c *Client) AppUnarchive(ctx context.Context, slug string) error {
	_, err := c.Admin(ctx, "app_unarchive", map[string]string{"slug": slug})
	return err
}

// AppDelete deletes an app, its grants, and its tokens. The hub reports a missing app as
// ErrNotFound (via IsNotFound); Delete paths treat that as success rather than an error.
func (c *Client) AppDelete(ctx context.Context, slug string) error {
	_, err := c.Admin(ctx, "app_delete", map[string]string{"slug": slug})
	return err
}

// AppSetUpstreamAuth stores the static headers a proxied `auth: headers` app sends upstream.
// Write-only and imperative like TokenIssue: the headers are sealed at rest and never readable
// back through any op, which is why §22.2's convergence witness (`headers_applied_version`)
// exists — it is the only signal the provider has that a given version was ever accepted.
func (c *Client) AppSetUpstreamAuth(ctx context.Context, slug string, headers map[string]string) error {
	_, err := c.Admin(ctx, "app_set_upstream_auth", map[string]any{"slug": slug, "headers": headers})
	return err
}
