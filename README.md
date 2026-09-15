# terraform-provider-pmcp

An OpenTofu/Terraform provider for a **personal MCP hub** — a Cloudflare Worker that proxies MCP
calls to bots which dial in over a reverse WebSocket tunnel. This provider manages the hub's
*contents*: apps, agents, role grants, and proxied-upstream credentials. It does not deploy the
hub — Wrangler owns that, and the boundary is deliberate.

**Status: scaffolded. The design is complete and reviewed; the resources are not implemented.**
What exists today is the provider block, the admin transport, and its tests. See
[What is not here yet](#what-is-not-here-yet) before opening an issue about a missing resource.

## Why it exists

The hub's only administrative surface is MCP itself — a set of tools on a reserved builtin app,
reached by a single `POST` per call. There is no REST admin API. Before this provider, the
declarative option was a YAML file with `diff`/`apply` subcommands in the hub's CLI, which
covered apps, agents and grants and deliberately excluded everything else.

That tool had a property worth knowing about: it computed **total** desired state, so anything
present on the server and absent from the file was deleted. Pointing it at a hub managed by
anything else destroys it. OpenTofu destroys only what is in its own state, so the two cannot
safely coexist — which is why the hub retires the YAML path rather than keeping both.

## Provider configuration

```hcl
terraform {
  required_version = ">= 1.11" # write-only attributes

  required_providers {
    pmcp = {
      source  = "registry.opentofu.org/ahrzb/pmcp"
      version = "~> 0.1"
    }
  }
}

provider "pmcp" {
  endpoint = "https://hub.example.workers.dev" # or PMCP_URL
  token    = var.pmcp_admin_token              # or PMCP_ADMIN_TOKEN
}
```

Both attributes are optional and resolved **config-then-env**, with no config-file fallback: the
hub's CLI keeps named profiles in `~/.config/pmcp/config.toml`, and reading them here would let
an operator's stale profile shadow a CI credential.

The env var is `PMCP_ADMIN_TOKEN`, **not** the CLI's `PMCP_TOKEN`. Those hold different credential
families: `PMCP_TOKEN` is a browser-equivalent session bearer with a sliding one-week expiry, and
sharing the name would let one silently drive `tofu apply` until it died mid-week.

### The credential

The provider authenticates with a `pmcp_adm_…` **admin token** — a credential family the hub grows
alongside this provider. Two properties are load-bearing:

- It is **narrower than a session**, not wider. An admin token administers the hub and cannot call
  a single app tool, nor reach any browser route. A session bearer can do everything a human can.
- It **cannot mint another admin token**, and cannot decide pending approvals. A leak therefore
  cannot outlive revocation of the human, and cannot approve its own requests.

Mint one from a signed-in session — `pmcp admin-token issue` — and store it wherever your other
infrastructure credentials live. Rotation is issue-then-revoke; there is no rotate operation.

On `401` the provider reports that the token is *expired, revoked, or not an admin token* without
choosing between them, because the hub does not distinguish those cases either — doing so would
tell a holder that a string was once valid.

## Installation

There is no registry listing. Consume it from this flake:

```nix
{
  inputs.pmcp-provider.url = "github:ahrzb/terraform-provider-pmcp";

  # …then, wherever you build your tofu:
  tofu = pkgs.opentofu.withPlugins (p: [
    p.cloudflare_cloudflare
    inputs.pmcp-provider.packages.${system}.default
  ]);
}
```

`withPlugins` keys the plugin directory off the derivation's `passthru.provider-source-address`,
which is why that attribute exists and must match `main.go`'s `Address` exactly. Get it wrong and
tofu ignores the plugin and reaches for a registry that has never heard of it.

Or try it without wiring anything: `nix run github:ahrzb/terraform-provider-pmcp#tofu -- plan`.

`vendorHash` is real. Regenerate it with `nix build .#default -L` after any change to what the
code **imports** — not only after a `go.mod` change, since `buildGoModule` vendors the packages
actually imported, so a new subpackage of an already-required module moves the hash while
`go.mod` stays byte-identical.

## What is not here yet

The full schema — every attribute, mode, default, import ID and lifecycle rule — is specified in
**§22 of the hub's design spec** (`docs/specs/provider/22-opentofu-provider.md` in the hub repo,
which is private). Planned surface:

| Kind | Name | Notes |
|---|---|---|
| resource | `pmcp_tunnel_app` | bots that dial in; `log_bodies` defaults true |
| resource | `pmcp_proxy_app` | upstream MCP endpoints; carries write-only `headers_wo` |
| resource | `pmcp_agent` | consumer identities |
| resource | `pmcp_grant` | `(agent, app)` → role sets, split `allow` / `approval` |
| resource | `pmcp_token` | app tokens and agent keys; the value is in state, see below |
| data source | `pmcp_app`, `pmcp_agent` | singular lookup by slug |
| data source | `pmcp_tokens` | inventory, including tokens this provider did not issue |

Also pending: the terranix module (`terranixModules.pmcp`), the coverage oracle
(`apps.coverage-check`), and the acceptance rig (`apps.acceptance`).

Three design decisions worth knowing before contributing, because each reverses an obvious choice:

- **Tunnel and proxy apps are separate resource types**, not one type with a `kind`. Proxy-only
  fields are meaningless on a tunnelled app and the hub rejects them, so invalid combinations
  should fail at plan time rather than mid-apply.
- **`pmcp_token` keeps the issued secret in state.** Credentials are returned exactly once and
  cannot be read back, so there is no alternative: write-only attributes cannot carry a
  *returned* value, and an ephemeral resource would re-open and orphan a credential every run.
  This is the `aws_iam_access_key` shape, and it means state encryption is a requirement rather
  than a nicety. Tokens issued by hand are unaffected — the provider destroys only rows in its
  own state, so ad-hoc and managed credentials coexist. Destroying a `pmcp_agent`, however,
  revokes *every* token for that agent, including ones this provider never created.
  Rotation is a replacement, and `create_before_destroy` is a guarantee rather than an accident:
  `Create` revokes nothing and `Delete` revokes exactly its own id, so overlapping generations are
  a supported state and a consumer can be switched over between the two legs. **Delivering the
  value to a consumer is out of scope** — and note that nothing hub-side can tell you which
  credential a live consumer is using, `last_used_at` included: it is stamped before the hub
  checks whether the app exists or is archived, it is throttled, and it carries no attribution.
- **Upstream headers live on `pmcp_proxy_app`, not their own resource.** Changing an app's `auth`
  mode wipes the hub-side credential; a separate resource would show no diff when that happens —
  write-only values are null in plan and state by construction — so the headers would never be
  re-sent and the app would go live with none, while apply reported success.

## Development

```bash
go build ./... && go test ./...   # offline: the admin client is tested against an httptest fake
nix develop                       # go, gopls, gcc, opentofu, gofumpt
```

CI gates on `nix flake check -L` plus `nix build .#default -L`, matching the sibling
`terraform-provider-gws` repo. Three checks: `build`, `gotest` (every package, preceded by
`go vet ./...`) and `gofmt`.

One trap worth knowing if you add a package or a check: `nix flake check` sees **tracked files
only**, so a brand-new untracked test file is invisible to it and the gate passes having never
compiled your test. `git add` first. Relatedly, the `gotest` check clears the package's
`subPackages = [ "." ]` — inherited, it scopes the check phase to the root package, which has no
tests, so the gate reported success while running nothing. Both failure modes are silent
successes, which is the only kind worth documenting.

## Licence

MIT. See [LICENSE](LICENSE).
