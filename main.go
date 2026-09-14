package main

import (
	"context"
	"flag"
	"log"

	"github.com/ahrzb/terraform-provider-pmcp/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is overridden at build time via -ldflags. The Nix derivation sets it from the same
// string that names the plugin directory, so a mismatch is visible rather than silent.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	// The address must match the provider's source address exactly, and the Nix derivation's
	// `passthru.provider-source-address` must match it too: nixpkgs' `withPlugins` keys the
	// plugin directory off that attribute, and a mismatch makes tofu ignore the plugin and
	// reach for a registry that has never heard of it.
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.opentofu.org/ahrzb/pmcp",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
