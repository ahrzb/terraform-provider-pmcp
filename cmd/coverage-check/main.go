// Command coverage-check runs §22.5's parity oracle: it takes one admin-ops.json contract
// fixture as its sole positional argument, drives the provider's resources and data sources
// against a scripted fake of the hub, and reports every op or op.field the fixture declares
// that no provider path reaches (or every staged entry the fixture has caught up to, in
// -strict mode). Exit 0 means clean; exit 1 means violations were printed to stderr; exit 2
// means the check itself could not run (bad flags, unreadable files, a bug in this package's
// own scenarios).
//
// Wired two ways in flake.nix, matching §22.5's two invocations:
//   - apps.coverage-check: what the hub's own CI runs, against its own working-tree fixture —
//     `nix run github:ahrzb/terraform-provider-pmcp#coverage-check -- ./contracts/admin-ops.json`.
//     Never -strict: a fixture-ahead-of-provider op fails here, but a provider-ahead staged
//     entry the fixture hasn't caught up to yet must not.
//   - checks.coverage-check: this repository's own `nix flake check`, against its pinned
//     coverage/admin-ops.json, with -strict — the only place the staged-entry expiry rule
//     (coverage/staged.json) runs, per §22.5's two-commit landing sequence.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ahrzb/terraform-provider-pmcp/internal/coverage"
)

func main() {
	strict := flag.Bool("strict", false, "also enforce coverage/staged.json's expiry rule "+
		"(this repo's own CI against its own pinned fixture only — never the hub's invocation)")
	stagedPath := flag.String("staged", "coverage/staged.json", "path to this repo's staged.json")
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: coverage-check [-strict] [-staged path] <admin-ops.json>")
		os.Exit(2)
	}

	result, err := coverage.Check(args[0], *stagedPath, *strict)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverage-check:", err)
		os.Exit(2)
	}

	if !result.OK() {
		for _, v := range result.Violations {
			fmt.Fprintln(os.Stderr, "FAIL:", v)
		}
		fmt.Fprintf(os.Stderr, "coverage-check: %d violation(s)\n", len(result.Violations))
		os.Exit(1)
	}

	fmt.Printf("coverage-check: OK — %d admin ops reached by a provider path\n", len(result.ReachedOps))
}
