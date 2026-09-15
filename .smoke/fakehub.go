//go:build ignore

// A throwaway hub, good enough to drive a real `tofu apply` through the compiled plugin. Not a
// test fixture: the package's own fakes cover unit behaviour. This exists to answer the one
// question no unit test can — does OpenTofu itself accept this provider's schema, plan a
// configuration against it, and apply it end to end.
//
// Run: go run .smoke/fakehub.go -addr 127.0.0.1:8787
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type state struct {
	mu      sync.Mutex
	apps    map[string]map[string]any
	agents  map[string]map[string]any
	grants  map[string]map[string][]string // agent -> app -> roles
	tokens  []map[string]any
	seq     int
	upstrem map[string]string // slug -> last Authorization header applied
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	flag.Parse()

	s := &state{
		apps:    map[string]map[string]any{},
		agents:  map[string]map[string]any{},
		grants:  map[string]map[string][]string{},
		upstrem: map[string]string{},
	}

	mux := http.NewServeMux()

	// The provider resolves its namespace here before any admin call.
	mux.HandleFunc("/api/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"namespace": "smoke", "principal": "user:smoke"})
	})

	mux.HandleFunc("/smoke/mcp/pmcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64 `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("op=%s args=%v", req.Params.Name, req.Params.Arguments)

		result, rpcErr := s.dispatch(req.Params.Name, req.Params.Arguments)
		if rpcErr != nil {
			writeJSON(w, map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": rpcErr.code, "message": rpcErr.message},
			})
			return
		}
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"structuredContent": result},
		})
	})

	log.Printf("fake hub on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type rpcFault struct {
	code    int
	message string
}

// absent mirrors the hub exactly: absence has no code of its own, it is -32602 carrying the
// sentence IsNotFound matches. Getting this wrong in the fake would hide the very bug the
// provider's RemoveResource paths depend on.
func absent(family string) *rpcFault {
	return &rpcFault{code: -32602, message: fmt.Sprintf("no such %s in this namespace", family)}
}

func (s *state) dispatch(op string, args map[string]any) (any, *rpcFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()

	str := func(k string) string { v, _ := args[k].(string); return v }

	switch op {
	case "app_create":
		slug := str("slug")
		if slug == "pmcp" {
			return nil, &rpcFault{code: -32602, message: "slug is reserved"}
		}
		row := map[string]any{
			"slug": slug, "kind": str("kind"), "name": slug, "description": "",
			"archived": false, "logBodies": str("kind") == "tunnel",
			"roles": map[string]any{}, "redact": map[string]any{}, "redactResults": map[string]any{},
			"createdAt": now,
		}
		if str("kind") == "tunnel" {
			row["status"], row["lastSeen"] = "offline", nil
		} else {
			row["endpoint"], row["auth"], row["forwardIdentity"] = str("endpoint"), "headers", false
		}
		apply(row, args)
		s.apps[slug] = row
		return map[string]any{"app": row}, nil

	case "app_get":
		row, ok := s.apps[str("slug")]
		if !ok {
			return nil, absent("app")
		}
		return map[string]any{"app": row}, nil

	case "app_update":
		row, ok := s.apps[str("slug")]
		if !ok {
			return nil, absent("app")
		}
		apply(row, args)
		return map[string]any{"app": row}, nil

	case "app_archive", "app_unarchive":
		row, ok := s.apps[str("slug")]
		if !ok {
			return nil, absent("app")
		}
		row["archived"] = op == "app_archive"
		return map[string]any{"app": row}, nil

	case "app_set_upstream_auth":
		row, ok := s.apps[str("slug")]
		if !ok {
			return nil, absent("app")
		}
		hdrs, _ := args["headers"].(map[string]any)
		keys := make([]string, 0, len(hdrs))
		for k := range hdrs {
			keys = append(keys, k)
		}
		s.upstrem[str("slug")] = strings.Join(keys, ",")
		_ = row
		return map[string]any{"ok": true}, nil

	case "app_delete":
		if _, ok := s.apps[str("slug")]; !ok {
			return nil, absent("app")
		}
		delete(s.apps, str("slug"))
		return map[string]any{"deleted": str("slug")}, nil

	case "agent_create":
		slug := str("slug")
		row := map[string]any{"slug": slug, "name": slug, "description": "", "createdAt": now}
		apply(row, args)
		s.agents[slug] = row
		return map[string]any{"agent": s.agentRow(slug)}, nil

	case "agent_update":
		row, ok := s.agents[str("slug")]
		if !ok {
			return nil, absent("agent")
		}
		apply(row, args)
		return map[string]any{"agent": s.agentRow(str("slug"))}, nil

	case "agent_list":
		out := make([]any, 0, len(s.agents))
		for slug := range s.agents {
			out = append(out, s.agentRow(slug))
		}
		return map[string]any{"agents": out}, nil

	case "agent_delete":
		slug := str("slug")
		if _, ok := s.agents[slug]; !ok {
			return nil, absent("agent")
		}
		delete(s.agents, slug)
		delete(s.grants, slug)
		return map[string]any{"deleted": slug}, nil

	case "grant_set":
		agent, app := str("agent"), str("app")
		if _, ok := s.agents[agent]; !ok {
			return nil, absent("agent")
		}
		roles := make([]string, 0)
		if raw, ok := args["roles"].([]any); ok {
			for _, r := range raw {
				if v, ok := r.(string); ok {
					roles = append(roles, v)
				}
			}
		}
		if s.grants[agent] == nil {
			s.grants[agent] = map[string][]string{}
		}
		if len(roles) == 0 {
			delete(s.grants[agent], app)
		} else {
			s.grants[agent][app] = roles
		}
		return map[string]any{"agent": agent, "app": app, "warnings": []any{}}, nil

	case "token_issue":
		s.seq++
		// `{kind, slug}` — the contract's own argument names. An earlier version of this fake
		// read args["app"]/args["agent"], which are the *resource's* attribute names, and so
		// reported every token as a kindless agent key. The provider was right; the fake lied.
		kind, slug := str("kind"), str("slug")
		id := fmt.Sprintf("tk_%04d", s.seq)
		prefix := fmt.Sprintf("pmcp_%s_%04d", map[string]string{"app": "app", "agent": "agt"}[kind], s.seq)
		row := map[string]any{
			"id": id, "kind": kind, "refSlug": slug, "prefix": prefix,
			"createdAt": now, "expiresAt": nil, "lastUsedAt": nil, "revokedAt": nil,
		}
		s.tokens = append(s.tokens, row)
		// token_issue's result is FLAT - the one op with a declared output schema.
		return map[string]any{
			"id": id, "token": prefix + "_secretsecret", "prefix": prefix,
			"kind": kind, "slug": slug, "expiresAt": nil,
		}, nil

	case "token_list":
		return map[string]any{"tokens": s.tokens}, nil

	case "token_revoke":
		for _, t := range s.tokens {
			if t["id"] == str("id") {
				t["revokedAt"] = now
				return map[string]any{"id": str("id")}, nil
			}
		}
		return nil, absent("token")

	case "app_list":
		out := make([]any, 0, len(s.apps))
		for _, row := range s.apps {
			out = append(out, row)
		}
		return map[string]any{"apps": out}, nil
	}

	return nil, &rpcFault{code: -32601, message: "unknown tool: " + op}
}

func (s *state) agentRow(slug string) map[string]any {
	row := s.agents[slug]
	grants := map[string]any{}
	for app, roles := range s.grants[slug] {
		grants[app] = roles
	}
	out := map[string]any{}
	for k, v := range row {
		out[k] = v
	}
	out["grants"] = grants
	return out
}

// apply copies the op's optional fields onto a row, translating the wire's snake_case arguments
// to the row's camelCase spelling. The two differ, and conflating them is the bug this fake
// would otherwise hide.
func apply(row map[string]any, args map[string]any) {
	for arg, field := range map[string]string{
		"name": "name", "description": "description", "endpoint": "endpoint",
		"auth": "auth", "log_bodies": "logBodies", "forward_identity": "forwardIdentity",
		"roles": "roles", "redact": "redact", "redact_results": "redactResults",
		"capabilities": "capabilities",
	} {
		if v, ok := args[arg]; ok {
			row[field] = v
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
