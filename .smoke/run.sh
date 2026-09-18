#!/usr/bin/env bash
# The end-to-end smoke test: a real `tofu` binary, planning and applying a real configuration
# through the compiled plugin, against the throwaway hub in fakehub.go. Unit tests cannot answer
# whether OpenTofu itself accepts these schemas; this can.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"; kill "${hub_pid:-}" 2>/dev/null || true' EXIT

echo "=== build plugin ==="
mkdir -p "$work/plugins"
(cd "$root" && go build -o "$work/plugins/terraform-provider-pmcp" .)

echo "=== start fake hub ==="
(cd "$root" && go run .smoke/fakehub.go -addr 127.0.0.1:8787 >"$work/hub.log" 2>&1) &
hub_pid=$!
for _ in $(seq 1 60); do
  grep -q "fake hub on http" "$work/hub.log" 2>/dev/null && break
  sleep 0.5
done
grep -q "fake hub on http" "$work/hub.log" || { echo "hub did not start"; cat "$work/hub.log"; exit 1; }

cat >"$work/tofurc" <<EOF
provider_installation {
  dev_overrides { "registry.opentofu.org/ahrzb/pmcp" = "$work/plugins" }
  direct {}
}
EOF

cat >"$work/main.tf" <<'EOF'
terraform {
  required_version = ">= 1.11"
  required_providers { pmcp = { source = "registry.opentofu.org/ahrzb/pmcp" } }
}

provider "pmcp" {
  endpoint = "http://127.0.0.1:8787"
  token    = "pmcp_adm_smoke"
}

resource "pmcp_tunnel_app" "tools" {
  slug        = "mcp-tools"
  description = "bots that dial in"
  redact      = { "^paper_.*$" = ["credentials.token"] }
  typescript_aliases = {
    service = "tools"
    tools   = { "paper_list" = "paperList" }
  }
}

resource "pmcp_proxy_app" "notion" {
  slug            = "notion"
  endpoint        = "https://mcp.notion.com/mcp"
  auth            = "headers"
  headers_wo      = { "X-Api-Key" = "secret-value" }
  headers_version = 1
  roles           = { reader = { tools = ["get_.*"] } }
  capabilities    = ["tools", "resources"]
}

resource "pmcp_agent" "claude" {
  slug = "claude"
  name = "Claude Code"
}

resource "pmcp_grant" "claude_tools" {
  agent = pmcp_agent.claude.slug
  app   = pmcp_tunnel_app.tools.slug
  allow = ["all"]
}

resource "pmcp_grant" "claude_notion" {
  agent    = pmcp_agent.claude.slug
  app      = pmcp_proxy_app.notion.slug
  allow    = ["reader"]
  approval = ["all"]
}

resource "pmcp_token" "tools_app" {
  app        = pmcp_tunnel_app.tools.slug
  expires_in = "never"
  lifecycle { create_before_destroy = true }
}

resource "pmcp_hub_settings" "owner" {
  default_timeout_ms = 45000
  max_timeout_ms     = 120000
}

data "pmcp_app" "tools" {
  slug       = pmcp_tunnel_app.tools.slug
  depends_on = [pmcp_tunnel_app.tools]
}

data "pmcp_tokens" "all" {
  depends_on = [pmcp_token.tools_app]
}

output "token_prefix" { value = pmcp_token.tools_app.prefix }
output "token_id"     { value = pmcp_token.tools_app.id }
output "app_kind"     { value = data.pmcp_app.tools.kind }
output "token_count"  { value = length(data.pmcp_tokens.all.tokens) }
output "hub_settings" { value = "${pmcp_hub_settings.owner.owner_id}:${pmcp_hub_settings.owner.default_timeout_ms}/${pmcp_hub_settings.owner.max_timeout_ms}" }
# `nonsensitive` rather than `sensitive = true`: the assertion is what we want to read, and the
# fact that OpenTofu demands one of the two is itself the evidence that `token` is marked
# sensitive in the schema.
output "plaintext_is_set" { value = nonsensitive(length(pmcp_token.tools_app.token) > 20) }
EOF

export TF_CLI_CONFIG_FILE="$work/tofurc"
cd "$work"

echo "=== plan ==="
tofu plan -no-color 2>&1 | tail -25

echo "=== apply ==="
tofu apply -auto-approve -no-color 2>&1 | tail -20

echo "=== outputs ==="
tofu output -no-color

echo "=== second plan must be EMPTY (no perpetual diff) ==="
tofu plan -no-color -detailed-exitcode 2>&1 | tail -12 && echo "PLAN_CLEAN" || {
  code=$?
  [ "$code" = 2 ] && { echo "PERPETUAL DIFF - plan is not empty after apply"; exit 1; }
  exit "$code"
}

echo "=== destroy ==="
tofu destroy -auto-approve -no-color 2>&1 | tail -8

echo "=== hub call log ==="
grep -o 'op=[a-z_]*' "$work/hub.log" | sort | uniq -c | sort -rn
