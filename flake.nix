{
  description = "OpenTofu/Terraform provider for a personal MCP hub (apps, agents, grants)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        version = "0.1.0";

        sourceAddress = "registry.opentofu.org/ahrzb/pmcp";

        provider = pkgs.buildGoModule {
          pname = "terraform-provider-pmcp";
          inherit version;
          src = ./.;

          # Computed by `nix build .#default -L` under WSL (x86_64-linux); the mismatch error's
          # `got:` line is the new value.
          #
          # Regenerate after any change to what the code IMPORTS, not only after a go.mod change.
          # buildGoModule vendors the packages actually imported, so pulling in another
          # subpackage of a module already in go.mod — `resource/schema/stringplanmodifier`, say
          # — moves this hash while go.mod and go.sum stay byte-identical. That is exactly how
          # this value went stale once already.
          vendorHash = "sha256-nZqkF9Gfp5XtPCZi5+tijoDNMAaVF8GWnlllucJtUFA=";

          subPackages = [ "." ];
          # Registry providers are built by goreleaser with cgo off; matching that keeps the
          # binary static and the closure small.
          env.CGO_ENABLED = 0;

          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];

          # `withPlugins` does not look in $out/bin — it expects the exact layout nixpkgs' own
          # mkProvider produces, keyed by the source address and the target platform:
          #   libexec/terraform-providers/<address>/<version>/<goos>_<goarch>/terraform-provider-<name>_<version>
          # Leaving the binary in bin/ makes tofu report the plugin directory as missing.
          postInstall = ''
            dir=$out/libexec/terraform-providers/${sourceAddress}/${version}/''${GOOS}_''${GOARCH}
            mkdir -p "$dir"
            mv $out/bin/terraform-provider-pmcp "$dir/terraform-provider-pmcp_${version}"
            rmdir $out/bin
          '';

          # `terraform.withPlugins` / `opentofu.withPlugins` in nixpkgs key the plugin directory
          # off this attribute, and OpenTofu resolves `source` in required_providers against it.
          # It has to match the Address in main.go, or tofu ignores the plugin and tries to
          # reach a registry that has never heard of it.
          passthru.provider-source-address = sourceAddress;

          meta = {
            description = "Declarative personal-MCP-hub resources for OpenTofu and Terraform";
            homepage = "https://github.com/ahrzb/terraform-provider-pmcp";
            license = pkgs.lib.licenses.mit;
          };
        };

        # An OpenTofu with this provider already installed, offline: `nix run .#tofu -- plan`
        # writes no plugin block and reaches no registry. Needs OpenTofu >= 1.11 for write-only
        # attributes; nixos-26.05 ships 1.11.8. Bound here (not only under `packages`) because
        # `checks.terranix` below also drives it, offline, against `tofu providers schema -json`.
        tofu = pkgs.opentofu.withPlugins (_: [ provider ]);
      in
      {
        packages = {
          default = provider;
          terraform-provider-pmcp = provider;

          # An OpenTofu with this provider already installed, for trying it out without writing
          # a plugin block: `nix run .#tofu -- plan`. Note the provider needs OpenTofu >= 1.11
          # for write-only attributes; nixos-26.05 ships 1.11.8.
          inherit tofu;
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.gcc # cgo is needed to build the test binaries, despite CGO_ENABLED=0 above
            pkgs.opentofu
            pkgs.gofumpt
          ];
        };

        checks = {
          build = provider;
          # `subPackages = [ "." ]` is right for the *package* — it keeps the closure to the one
          # plugin binary — but it also scopes the check phase, and the root package has no
          # tests. Left inherited, this gate would report success having run nothing. Clearing
          # it makes the check see every package; a new tested package is picked up for free.
          gotest = provider.overrideAttrs (_old: {
            name = "terraform-provider-pmcp-tests";
            doCheck = true;
            subPackages = [ ];
            # `go test` runs only a subset of vet. Here because the vendored module cache is
            # already in place, so this costs nothing extra.
            preCheck = ''
              go vet ./...
            '';
            # `subPackages = [ ]` now builds two `package main`s — this one and
            # cmd/coverage-check's — into $out/bin. `provider`'s inherited postInstall assumes
            # exactly one (it `mv`s it out, then `rmdir`s bin) and fails once a second binary is
            # present; this check never needed the provider plugin-directory layout anyway.
            postInstall = "";
          });

          # §22.7: "checks.terranix evaluates the module against a sample configuration and
          # asserts that every attribute name it emits exists in the provider's schema." There
          # is no terranix-to-provider codegen, so the typed layer in modules/terranix/pmcp.nix
          # WILL lag a provider release eventually; this is what catches it, rather than a
          # silently-accepted (or silently-wrong) `tofu plan`. It is deliberately driven against
          # the real, just-built provider's `tofu providers schema -json` — not a hand-copied
          # attribute list, which would only catch drift against itself.
          terranix =
            let
              # The subset of terranix's own top-level vocabulary this module and its sample use
              # (see modules/terranix/pmcp.nix's `extraConfigKeys`) plus the leftover categories
              # terranix supports, declared as plain freeform containers so this check needs no
              # terranix flake input of its own — matching modules/terranix/pmcp.nix, which also
              # has none. A real consumer (e.g. `shed`) gets the actual terranix core instead.
              terranixCore = {
                options =
                  pkgs.lib.genAttrs
                    [
                      "resource"
                      "data"
                      "provider"
                      "terraform"
                      "output"
                      "variable"
                      "locals"
                      "module"
                    ]
                    (
                      _:
                      pkgs.lib.mkOption {
                        type = pkgs.lib.types.attrsOf pkgs.lib.types.anything;
                        default = { };
                      }
                    );
              };

              # Exercises every typed option once: both app trees, the roles bare-list sugar
              # alongside the typed per-family form (on proxy `roles` and tunnel `ownerRoles`),
              # a grant on each app kind, and `extraConfig` reaching into the already-typed proxy
              # app to set `headers_wo` — the field this module deliberately has no `mkOption`
              # for (§22.2, §22.7).
              sample = {
                pmcp.agents.bot = {
                  description = "sample agent";
                };
                pmcp.tunnelApps.tunnel-one = {
                  description = "tunnel sample";
                  archived = false;
                  ownerRoles = {
                    mine = [ "get_.*" ]; # bare-list sugar
                    docs.prompts = [ "draft_.*" ];
                  };
                };
                pmcp.proxyApps.proxy-one = {
                  endpoint = "https://upstream.example/mcp";
                  auth = "headers";
                  forwardIdentity = true;
                  capabilities = [
                    "tools"
                    "prompts"
                  ];
                  headersVersion = 1;
                  roles = {
                    reader = [ "get_.*" ]; # bare-list sugar
                    writer = {
                      tools = [ "set_.*" ];
                      prompts = [ "draft_.*" ];
                    };
                  };
                };
                pmcp.grants.bot.tunnel-one.allow = [ "all" ];
                pmcp.grants.bot.proxy-one = {
                  allow = [ "reader" ];
                  approval = [ "writer" ];
                };
                # Both token kinds, and both halves of the reference rule: `tunnel-one` is
                # declared above so it must emit an interpolated reference, while
                # `unmanaged-elsewhere` is not, so it must emit the bare slug — the shape an
                # operator host uses when it wants credentials for apps it does not manage.
                pmcp.tokens.tunnel-one-a = {
                  app = "tunnel-one";
                  expiresIn = "never";
                };
                pmcp.tokens.unmanaged-a = {
                  app = "unmanaged-elsewhere";
                  rotation = 1;
                };
                pmcp.tokens.bot-key-a = {
                  agent = "bot";
                  expiresIn = 7776000;
                };
                pmcp.extraConfig.resource.pmcp_proxy_app.proxy-one.headers_wo = {
                  Authorization = "Bearer $TOKEN";
                };
              };

              evaluated = pkgs.lib.evalModules {
                modules = [
                  terranixCore
                  ./modules/terranix/pmcp.nix
                  sample
                ];
              };
              rendered = {
                inherit (evaluated.config) resource provider terraform;
              };
              renderedJSON = pkgs.writeText "pmcp-terranix-sample.tf.json" (builtins.toJSON rendered);
            in
            pkgs.runCommand "terraform-provider-pmcp-terranix-check"
              {
                nativeBuildInputs = [
                  tofu
                  pkgs.jq
                ];
              }
              ''
                set -eu
                work="$TMPDIR/work"
                mkdir -p "$work"
                cd "$work"
                cp ${renderedJSON} main.tf.json
                export HOME="$work"
                # `NIX_TERRAFORM_PLUGIN_DIR` (baked in by `withPlugins`) is a filesystem mirror,
                # so this reaches no network — the same property `terraform_plugins_test` in
                # nixpkgs itself relies on to run inside the build sandbox.
                tofu init -input=false -backend=false >tofu-init.log 2>&1
                tofu providers schema -json >schema.json

                jq -n \
                  --argjson cfg "$(cat main.tf.json)" \
                  --argjson schema "$(cat schema.json)" \
                  --arg addr "${sourceAddress}" \
                  '
                    def resourceSchema($type):
                      $schema.provider_schemas[$addr].resource_schemas[$type].block.attributes // {};

                    [
                      ($cfg.resource // {}) | to_entries[] as $rt |
                      resourceSchema($rt.key) as $attrs |
                      $rt.value | to_entries[] as $inst |
                      $inst.value | keys[] as $k |
                      select(($attrs | has($k)) | not) |
                      "\($rt.key).\($inst.key): attribute \"\($k)\" does not exist in the provider schema"
                    ]
                  ' >drift.json

                if [ "$(jq 'length' drift.json)" -ne 0 ]; then
                  echo "modules/terranix/pmcp.nix emits attributes the provider schema does not have:" >&2
                  jq -r '.[]' drift.json >&2
                  exit 1
                fi
                touch $out
              '';
          # CI gates on `nix flake check` alone, so formatting has to be a check rather than a
          # separate workflow step, or it stops being enforced at all.
          gofmt = pkgs.runCommand "terraform-provider-pmcp-gofmt" { nativeBuildInputs = [ pkgs.go ]; } ''
            unformatted=$(cd ${./.} && gofmt -l .)
            if [ -n "$unformatted" ]; then
              echo "not gofmt-clean:" >&2
              echo "$unformatted" >&2
              exit 1
            fi
            touch $out
          '';
          # §22.5's parity oracle: `checks.coverage-check` gates this repo's own `nix flake
          # check` against its pinned coverage/admin-ops.json (§22.5's "provider repo's own CI,
          # against its own pinned fixture"), which is why the hub can't be reached as a flake
          # input instead — both repos are private, and coverage/staged.json exists precisely so
          # the two land a change in separate commits rather than needing a circular input.
          # `-strict` enables the staged-entry expiry rule; this is the one invocation where
          # that rule is meant to run (§22.5, "Gating here, without a deadlock").
          coverage-check =
            let
              coverageCheckBin = provider.overrideAttrs (_old: {
                pname = "terraform-provider-pmcp-coverage-check";
                subPackages = [ "cmd/coverage-check" ];
                # `provider`'s postInstall relocates a `terraform-provider-pmcp` binary into the
                # plugin-directory layout `withPlugins` expects; this build produces a plain
                # `coverage-check` binary instead, so that step does not apply here.
                postInstall = "";
              });
            in
            pkgs.runCommand "terraform-provider-pmcp-coverage-check" { } ''
              ${coverageCheckBin}/bin/coverage-check -strict \
                -staged ${./coverage/staged.json} \
                ${./coverage/admin-ops.json}
              touch $out
            '';
        };

        # §22.5's parity oracle, as a flake app taking the fixture as an argument — what the
        # hub's own CI runs against its own working-tree contract:
        #   nix run github:ahrzb/terraform-provider-pmcp#coverage-check -- ./contracts/admin-ops.json
        # Deliberately not `-strict`: the staged-entry expiry rule belongs only to this repo's
        # own `checks.coverage-check` above (§22.5) — running it here would deadlock the two
        # repos on which one lands its half of a two-commit change first.
        apps.coverage-check = {
          type = "app";
          program = "${pkgs.writeShellScript "coverage-check" ''
            exec ${
              provider.overrideAttrs (_old: {
                pname = "terraform-provider-pmcp-coverage-check";
                subPackages = [ "cmd/coverage-check" ];
                postInstall = "";
              })
            }/bin/coverage-check -staged ${./coverage/staged.json} "$@"
          ''}";
        };

        formatter = pkgs.nixfmt-tree;
      }
    )
    // {
      # For consumers: adds `terraform-provider-pmcp` to pkgs, so an existing
      # `opentofu.withPlugins` call can pick it up alongside the nixpkgs providers.
      overlays.default = final: _prev: {
        terraform-provider-pmcp = self.packages.${final.stdenv.hostPlatform.system}.terraform-provider-pmcp;
      };

      # A typed terranix schema for apps, agents and grants (§22.7), so a consumer writes
      # `pmcp.tunnelApps.foo = { ... };` instead of hand-rolling resource blocks and the
      # app-before-its-grants reference ordering. See modules/terranix/pmcp.nix for what it
      # covers and its `extraConfig` escape hatch; `checks.terranix` keeps it honest against
      # this provider's own schema.
      terranixModules = rec {
        pmcp = ./modules/terranix/pmcp.nix;
        default = pmcp;
      };
    };
}
