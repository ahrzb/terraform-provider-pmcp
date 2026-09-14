{
  description = "OpenTofu/Terraform provider for a personal MCP hub (apps, agents, grants)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { self, nixpkgs, flake-utils }:
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

          # BOOTSTRAP: this is a placeholder. The repo was scaffolded from Windows, where nix
          # cannot evaluate, so the real hash has never been computed. On the first WSL/Linux
          # build, run `nix build .#default -L`, take the `got:` hash from the mismatch error,
          # and replace this line. Until then every nix build of this flake fails loudly —
          # which is the intent, rather than a hash that looks real and is not.
          vendorHash = pkgs.lib.fakeHash;

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
      in
      {
        packages = {
          default = provider;
          terraform-provider-pmcp = provider;

          # An OpenTofu with this provider already installed, for trying it out without writing
          # a plugin block: `nix run .#tofu -- plan`. Note the provider needs OpenTofu >= 1.11
          # for write-only attributes; nixos-26.05 ships 1.11.8.
          tofu = pkgs.opentofu.withPlugins (_: [ provider ]);
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
          gotest = provider.overrideAttrs (_old: {
            name = "terraform-provider-pmcp-tests";
            doCheck = true;
          });
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

      # terranixModules.pmcp lands with the typed module (see README, "What is not here yet").
      # It is deliberately absent rather than stubbed: an empty module that evaluates to no
      # resources would let a consumer import it and silently manage nothing.
    };
}
