# A terranix module for the hub's apps, agents and grants (§22.7 of the hub's design spec).
#
# One module, because the three are one namespace: a grant names an agent and an app, so
# splitting them into separate modules would force every consumer to wire the cross-reference
# by hand instead of writing `pmcp.grants.<agent>.<app> = { ... };` and letting the module find
# both.
#
# Tunnel and proxy apps are deliberately SEPARATE option trees (`tunnelApps` / `proxyApps`),
# not one tree with a `kind` switch. A `kind`-switch tree lets `endpoint`/`auth`/`roles` sit on
# a tunnelled app right up until `tofu apply` tells you the hub rejected it. Two trees make that
# combination unrepresentable: `pmcp.tunnelApps.foo.endpoint = "...";` is "attribute `endpoint`
# does not exist" at `nix eval`, before a plan is ever produced.
#
# Every provider field this module knows about gets a typed `mkOption` — see §22.4's schema —
# but there is no provider-schema-to-Nix codegen (terranix/terranix#100), so the typed layer
# WILL lag a provider release eventually. `extraConfig` is the escape hatch: a freeform blob,
# deep-merged over the typed output last, so a new provider field is one line in a caller's
# config instead of a module release. It is also the only way to set `headers_wo` — there is
# deliberately no typed option for it, because a terranix module renders to a JSON file on disk
# and a static secret has no business living in that file (§22.2, §22.7). `checks.terranix` in
# this repo's flake evaluates a sample against the real provider schema, so reaching for
# `extraConfig` where a typed option should exist is itself the signal that this file is behind.
{ lib, config, ... }:
let
  cfg = config.pmcp;
  inherit (lib) mkOption mkEnableOption types;

  # Terraform resource-local names allow `[A-Za-z0-9_-]`, must not start with a digit, and slugs
  # are `[a-z0-9-]+` (§22.4) — already legal except for the leading-digit case, which is why this
  # exists at all rather than using the slug verbatim.
  resourceName = slug: if builtins.match "[0-9].*" slug != null then "_${slug}" else slug;

  dropNulls = lib.filterAttrs (_: v: v != null);

  # --- shared app schema (§22.4's commonAppAttributes) ----------------------------------------

  commonAppOptions = {
    name = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Display name. The hub defaults it to the slug when omitted at create.";
    };
    description = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Free-text note. Defaults to \"\" on the hub.";
    };
    archived = mkOption {
      type = types.nullOr types.bool;
      default = null;
      description = ''
        Hides the app from consumers, retaining everything. Defaults to `false`. Fires
        `app_archive`/`app_unarchive` on the hub side — never `app_update`.
      '';
    };
    redact = mkOption {
      type = types.attrsOf (types.listOf types.str);
      default = { };
      description = "Tool-or-pattern -> sensitive ARGUMENT paths. Keys are anchored regular expressions.";
    };
    redactResults = mkOption {
      type = types.attrsOf (types.listOf types.str);
      default = { };
      description = "Tool-or-pattern -> sensitive RESULT paths. Same key grammar as `redact`.";
    };
    logBodies = mkOption {
      type = types.nullOr types.bool;
      default = null;
      description = "Records call bodies in the audit ledger. Hub default is by kind: true for tunneled, false for proxied.";
    };
  };

  # A role's wire shape is `oneOf` a bare pattern list or a per-family object, and the provider
  # takes only the object form (§22.4) — the plugin framework cannot express a `oneOf` as one
  # static type. This module accepts both and normalizes the bare list to `{ tools = [...]; }`
  # before emitting, which is this module's stated job (§22.4: "The bare-list sugar lives in the
  # terranix module").
  roleFamilyType = types.submodule {
    options = {
      tools = mkOption {
        type = types.nullOr (types.listOf types.str);
        default = null;
        description = "Tool-name patterns this role grants.";
      };
      prompts = mkOption {
        type = types.nullOr (types.listOf types.str);
        default = null;
        description = "Prompt-name patterns this role grants.";
      };
      resources = mkOption {
        type = types.nullOr (types.listOf types.str);
        default = null;
        description = "Resource-URI patterns this role grants.";
      };
    };
  };
  roleType = types.either (types.listOf types.str) roleFamilyType;
  normalizeRole = r: dropNulls (if builtins.isList r then { tools = r; } else r);

  # --- proxy-only schema (§22.4's pmcp_proxy_app additions) -----------------------------------

  proxyOnlyOptions = {
    endpoint = mkOption {
      type = types.str;
      description = "The upstream MCP endpoint URL the hub forwards calls to. Required.";
    };
    auth = mkOption {
      type = types.nullOr (
        types.enum [
          "headers"
          "oauth"
        ]
      );
      default = null;
      description = ''
        `headers` or `oauth`. Defaults to `headers`. Flipping this wipes any stored upstream
        credential in the same write (§22.2).
      '';
    };
    forwardIdentity = mkOption {
      type = types.nullOr types.bool;
      default = null;
      description = "Sends `X-Pmcp-*` identity headers upstream. Defaults to `false`.";
    };
    roles = mkOption {
      type = types.attrsOf roleType;
      default = { };
      description = "Virtual role definitions, keyed by role name. Bare pattern lists are sugar for `{ tools = [...]; }`.";
    };
    capabilities = mkOption {
      type = types.nullOr (
        types.listOf (
          types.enum [
            "tools"
            "prompts"
            "resources"
            "completions"
          ]
        )
      );
      default = null;
      description = ''
        Declared MCP capability families. Omit entirely for the hub's own default
        (tools-only, §20.2) — an explicit `[ ]` is a different, empty declaration.
      '';
    };
    headersVersion = mkOption {
      type = types.nullOr types.int;
      default = null;
      description = ''
        The operator's intent for `headers_wo` (set via `extraConfig`, see the file header):
        bump this whenever the header value changes. Co-required with `headers_wo` on the
        provider side (§22.2) — this module cannot enforce that itself since `headers_wo`
        never appears in its typed schema.
      '';
    };
  };

  tunnelAppType = types.submodule { options = commonAppOptions; };
  proxyAppType = types.submodule { options = commonAppOptions // proxyOnlyOptions; };

  agentType = types.submodule {
    options = {
      name = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Display name. The hub defaults it to the slug when omitted at create.";
      };
      description = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Free-text note. Defaults to empty.";
      };
    };
  };

  grantType = types.submodule {
    options = {
      allow = mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = "Role names granted without approval gating.";
      };
      approval = mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = "Role names granted with approval gating (`role:approval` on the wire).";
      };
    };
  };

  # --- emission ---------------------------------------------------------------------------------

  commonAppFields = app: {
    inherit (app) name description archived;
    redact = if app.redact == { } then null else app.redact;
    redact_results = if app.redactResults == { } then null else app.redactResults;
    log_bodies = app.logBodies;
  };

  tunnelResources = lib.mapAttrs' (
    slug: app:
    lib.nameValuePair (resourceName slug) (dropNulls (commonAppFields app // { inherit slug; }))
  ) cfg.tunnelApps;

  proxyResources = lib.mapAttrs' (
    slug: app:
    lib.nameValuePair (resourceName slug) (
      dropNulls (
        commonAppFields app
        // {
          inherit slug;
          inherit (app) endpoint capabilities;
          auth = app.auth;
          forward_identity = app.forwardIdentity;
          roles = if app.roles == { } then null else lib.mapAttrs (_: normalizeRole) app.roles;
          headers_version = app.headersVersion;
        }
      )
    )
  ) cfg.proxyApps;

  agentResources = lib.mapAttrs' (
    slug: agent:
    lib.nameValuePair (resourceName slug) (dropNulls {
      inherit slug;
      inherit (agent) name description;
    })
  ) cfg.agents;

  # A grant naming an undeclared app cannot be ordered against it — there would be nothing to
  # reference — so this fails at evaluation rather than producing a config where `app`/`agent`
  # are bare strings (§22.4's "Configurations must reference pmcp_*_app.<name>.slug ... the
  # terranix module emits that reference automatically").
  appRef =
    slug:
    if cfg.tunnelApps ? ${slug} then
      "\${pmcp_tunnel_app.${resourceName slug}.slug}"
    else if cfg.proxyApps ? ${slug} then
      "\${pmcp_proxy_app.${resourceName slug}.slug}"
    else
      throw "pmcp terranix module: grant references app \"${slug}\", which is not declared under pmcp.tunnelApps or pmcp.proxyApps";

  agentRef =
    slug:
    if cfg.agents ? ${slug} then
      "\${pmcp_agent.${resourceName slug}.slug}"
    else
      throw "pmcp terranix module: grant references agent \"${slug}\", which is not declared under pmcp.agents";

  grantResources = lib.foldl' (
    acc: agentSlug:
    acc
    // lib.mapAttrs' (
      appSlug: grant:
      lib.nameValuePair "${resourceName agentSlug}_${resourceName appSlug}" (dropNulls {
        agent = agentRef agentSlug;
        app = appRef appSlug;
        allow = if grant.allow == [ ] then null else grant.allow;
        approval = if grant.approval == [ ] then null else grant.approval;
      })
    ) cfg.grants.${agentSlug}
  ) { } (builtins.attrNames cfg.grants);

  typed = {
    terraform = {
      required_providers.pmcp.source = cfg.sourceAddress;
      # Write-only attributes (`headers_wo`, set via `extraConfig`) require OpenTofu >= 1.11
      # (§22.2, §22.7).
      required_version = ">= 1.11";
    };
    provider.pmcp = { };
    resource = dropNulls {
      pmcp_tunnel_app = if tunnelResources == { } then null else tunnelResources;
      pmcp_proxy_app = if proxyResources == { } then null else proxyResources;
      pmcp_agent = if agentResources == { } then null else agentResources;
      pmcp_grant = if grantResources == { } then null else grantResources;
    };
  };

  # Every top-level Terraform-JSON block `extraConfig` might reasonably add to or extend —
  # matching the vocabulary `shed/docs/opentofu.md` documents for terranix itself
  # (`resource`, `data`, `output`, `provider`, `terraform`, plus `variable`/`locals`/`module`).
  # This list must stay a Nix-level literal, not derived from `cfg.extraConfig`'s own keys: the
  # module system resolves an option's value (here, `resource` etc.) by first asking every
  # contributing module for the ATTRIBUTE NAMES of its `config` return, and it does that before
  # any single option (like `extraConfig`) has been merged. Computing our returned attrNames
  # from `cfg.extraConfig`'s shape would make that shape-discovery depend on a merge that itself
  # depends on the same shape-discovery having already finished — `nix eval`'s "infinite
  # recursion encountered". A fixed key list sidesteps it: the module's own returned shape never
  # depends on `extraConfig`, only each key's *value* lazily does.
  extraConfigKeys = [
    "terraform"
    "provider"
    "resource"
    "data"
    "output"
    "variable"
    "locals"
    "module"
  ];
in
{
  options.pmcp = {
    enable = mkEnableOption "hub resources managed through the pmcp provider" // {
      default = cfg.tunnelApps != { } || cfg.proxyApps != { } || cfg.agents != { } || cfg.grants != { };
    };

    sourceAddress = mkOption {
      type = types.str;
      default = "registry.opentofu.org/ahrzb/pmcp";
      description = "Provider source address; must match how the plugin is installed.";
    };

    agents = mkOption {
      type = types.attrsOf agentType;
      default = { };
      description = "`pmcp_agent` resources, keyed by slug.";
    };

    tunnelApps = mkOption {
      type = types.attrsOf tunnelAppType;
      default = { };
      description = "`pmcp_tunnel_app` resources, keyed by slug. Proxy-only fields are not options here (see the file header).";
    };

    proxyApps = mkOption {
      type = types.attrsOf proxyAppType;
      default = { };
      description = "`pmcp_proxy_app` resources, keyed by slug.";
    };

    grants = mkOption {
      type = types.attrsOf (types.attrsOf grantType);
      default = { };
      description = "`pmcp_grant` resources, keyed by agent slug then app slug — mirroring `mcps.yaml`'s own shape.";
    };

    extraConfig = mkOption {
      type = types.attrs;
      default = { };
      description = ''
        Freeform Terraform JSON, deep-merged over the typed output last. The escape hatch for a
        provider attribute this module has no `mkOption` for yet — including `headers_wo`, which
        is deliberately never typed (see the file header) — without waiting on a module release.
        Reach into an already-typed resource with e.g.
        `resource.pmcp_proxy_app.foo.headers_wo = { ... };`, matching the resource-local name
        `pmcp.proxyApps.foo` produces. Only the top-level blocks named in `extraConfigKeys`
        (this file) are merged; anything else is a module update, not an `extraConfig` entry.
      '';
    };
  };

  config = lib.mkIf cfg.enable (
    lib.genAttrs extraConfigKeys (
      key: lib.recursiveUpdate (typed.${key} or { }) (cfg.extraConfig.${key} or { })
    )
  );
}
